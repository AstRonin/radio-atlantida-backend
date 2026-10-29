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
	"net"
	"net/url"
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
	// LokiURL is the base URL of Loki; the backend pushes its log lines to
	// it. Empty turns the push off (tests, a run without the dev stack).
	LokiURL string `env:"LOKI_URL"`
	// PlayoutJournalRoot is the directory where Liquidsoap writes its
	// playout journal; the backend only reads it, to reconcile what aired
	// (§7.1).
	PlayoutJournalRoot string `env:"PLAYOUT_JOURNAL_ROOT,required,notEmpty"`
	// HTTPAddr is the listen address of the API. It must stay on localhost or
	// a private network, because the API has no authentication (§10).
	HTTPAddr string `env:"HTTP_ADDR" envDefault:"127.0.0.1:8090"`
	// LiveUDPAddr is the one UDP socket of the live voice bridge (WebRTC,
	// docs/phase-12-plan.md §2 decision 4). Like HTTPAddr, it is a listen
	// address tied to the compose port mapping and stays on localhost or a
	// private network.
	LiveUDPAddr string `env:"LIVE_UDP_ADDR" envDefault:"127.0.0.1:8189"`
	// LivePublicIPs are the addresses on which the browser reaches
	// LiveUDPAddr (inside Docker the container's own address is not
	// reachable). Empty: the host's interface addresses.
	LivePublicIPs []string `env:"LIVE_PUBLIC_IPS" envSeparator:","`

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
	cfg, err := load[Config](dotenvPath)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate checks the values the env tags cannot.
func (c *Config) validate() error {
	if _, _, err := net.SplitHostPort(c.LiveUDPAddr); err != nil {
		return fmt.Errorf("invalid environment variables: LIVE_UDP_ADDR: %w", err)
	}
	if c.LokiURL != "" {
		u, err := url.Parse(c.LokiURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("invalid environment variables: LOKI_URL: want an http or https URL, got %q", c.LokiURL)
		}
	}
	ips := c.LivePublicIPs[:0]
	for _, ip := range c.LivePublicIPs {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("invalid environment variables: LIVE_PUBLIC_IPS: %q is not an IP address", ip)
		}
		ips = append(ips, ip)
	}
	c.LivePublicIPs = ips
	return nil
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
