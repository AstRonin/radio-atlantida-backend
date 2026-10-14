// Package config loads the process configuration from environment variables.
//
// Only the database connection, secrets, host paths, and the listen address
// come from the environment (§3.1, §6.3 of the design). Every other setting is
// an admin setting stored in the database. Configuration is read once at
// startup; no other package calls os.Getenv.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

// Config is the environment configuration of the backend.
type Config struct {
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
	// MediaRoot is the directory that holds the audio files. Track paths in
	// the database are relative to it (§8.7).
	MediaRoot string `env:"MEDIA_ROOT,required,notEmpty"`
	// LogRoot is the directory for the daily JSON log files (§10.1).
	LogRoot string `env:"LOG_ROOT,required,notEmpty"`
	// PlayoutJournalRoot is the directory where Liquidsoap writes its
	// playout journal; the backend only reads it, to reconcile what aired
	// (§7.1).
	PlayoutJournalRoot string `env:"PLAYOUT_JOURNAL_ROOT,required,notEmpty"`
	// HTTPAddr is the listen address of the API. It must stay on localhost or
	// a private network, because the API has no authentication (§10).
	HTTPAddr string `env:"HTTP_ADDR" envDefault:"127.0.0.1:8080"`

	Secrets Secrets
}

// Secrets are credentials that are never stored in the database, returned by
// the API, or logged. They are optional at startup and required by the
// features that use them.
type Secrets struct {
	IcecastSourcePassword     string `env:"ICECAST_SOURCE_PASSWORD"`
	LiquidsoapControlUser     string `env:"LIQUIDSOAP_CONTROL_USER"`
	LiquidsoapControlPassword string `env:"LIQUIDSOAP_CONTROL_PASSWORD"`
}

// String hides secret values, so a Config printed by accident leaks nothing.
func (Secrets) String() string { return "{***}" }

// GoString hides secret values from %#v.
func (s Secrets) GoString() string { return s.String() }

// IsSet reports, by environment variable name, which secrets have a value.
// The settings API shows only this, never the values (§6.3).
func (s Secrets) IsSet() map[string]bool {
	return map[string]bool{
		"ICECAST_SOURCE_PASSWORD":     s.IcecastSourcePassword != "",
		"LIQUIDSOAP_CONTROL_USER":     s.LiquidsoapControlUser != "",
		"LIQUIDSOAP_CONTROL_PASSWORD": s.LiquidsoapControlPassword != "",
	}
}

// Migrate is the configuration of `backend migrate up`. It needs only the
// database, so the migrate container of the deploy stack needs no host paths.
type Migrate struct {
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
}

// Load reads the configuration of `backend serve`. Variables from dotenvPath
// are loaded first without overriding variables that are already set; a
// missing file is not an error. Pass an empty dotenvPath to skip the file.
//
// The returned error lists every missing or invalid variable at once.
func Load(dotenvPath string) (Config, error) {
	return load[Config](dotenvPath)
}

// LoadMigrate reads the configuration of `backend migrate up`, with the same
// rules as Load.
func LoadMigrate(dotenvPath string) (Migrate, error) {
	return load[Migrate](dotenvPath)
}

func load[T any](dotenvPath string) (T, error) {
	var zero T
	if dotenvPath != "" {
		if err := godotenv.Load(dotenvPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return zero, fmt.Errorf("load %s: %w", dotenvPath, err)
		}
	}

	cfg, err := env.ParseAs[T]()
	if err != nil {
		return zero, describe(err)
	}
	return cfg, nil
}

// describe turns the aggregate error of caarlos0/env into one message that
// names every problem variable.
func describe(err error) error {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return fmt.Errorf("parse environment: %w", err)
	}

	var missing, invalid []string
	for _, e := range agg.Errors {
		var req env.VarIsNotSetError
		var empty env.EmptyVarError
		switch {
		case errors.As(e, &req):
			missing = append(missing, req.Key)
		case errors.As(e, &empty):
			missing = append(missing, empty.Key)
		default:
			invalid = append(invalid, e.Error())
		}
	}
	sort.Strings(missing)
	missing = slices.Compact(missing)

	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "missing required environment variables: "+strings.Join(missing, ", "))
	}
	if len(invalid) > 0 {
		parts = append(parts, "invalid environment variables: "+strings.Join(invalid, "; "))
	}
	return errors.New(strings.Join(parts, "; "))
}
