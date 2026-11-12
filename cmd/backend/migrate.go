package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/config"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/migrations"
)

// errIncompatible means the database schema needs a newer release than this
// binary (schema_compat.min_app_version, design §9.2). The process exits
// with code 3.
var errIncompatible = errors.New("incompatible database schema")

// compatTimeout bounds one read of the schema compatibility.
const compatTimeout = 10 * time.Second

// compatReader reads the schema compatibility (migrations.Compat).
type compatReader func(ctx context.Context) (migrations.Status, error)

// migrateUp applies the embedded migrations and exits. It logs JSON to out
// (stdout) only: the migrate container runs once, and its output goes to the
// update log of the deploy script (§9.2). A failed migration is logged by
// the runner in one line ("migration rolled back" or "database dirty").
func migrateUp(ctx context.Context, out io.Writer) error {
	cfg, err := config.LoadMigrate(".env")
	if err != nil {
		return err
	}
	log := logging.For(slog.New(logging.NewHandler(out, slog.LevelInfo)), logging.ComponentDB)

	if err := migrations.Up(ctx, cfg.DatabaseURL, log); err != nil {
		if !errors.Is(err, migrations.ErrRolledBack) && !errors.Is(err, migrations.ErrDirty) {
			log.Error("migration failed", logging.KeyError, err)
		}
		return err
	}
	return nil
}

// migrateCheck prints the schema compatibility of this binary as one JSON
// object and returns errIncompatible when the binary may not run against
// the schema.
func migrateCheck(ctx context.Context, out io.Writer) error {
	cfg, err := config.LoadMigrate(".env")
	if err != nil {
		return err
	}
	return writeCheck(ctx, out, func(ctx context.Context) (migrations.Status, error) {
		return migrations.Compat(ctx, cfg.DatabaseURL)
	}, version)
}

// checkReport is the output of `backend migrate check`. The deploy tool
// parses it; fields are only ever added.
type checkReport struct {
	// SchemaVersion is the applied migration version; 0 when none is.
	SchemaVersion uint   `json:"schema_version"`
	Dirty         bool   `json:"dirty"`
	MinAppVersion string `json:"min_app_version"`
	BinaryVersion string `json:"binary_version"`
	Compatible    bool   `json:"compatible"`
}

func writeCheck(ctx context.Context, out io.Writer, read compatReader, binary string) error {
	ctx, cancel := context.WithTimeout(ctx, compatTimeout)
	defer cancel()
	st, err := read(ctx)
	if err != nil {
		return err
	}
	r := checkReport{
		SchemaVersion: st.SchemaVersion,
		Dirty:         st.Dirty,
		MinAppVersion: st.MinAppVersion,
		BinaryVersion: binary,
		Compatible:    st.Compatible(binary),
	}
	if err := json.NewEncoder(out).Encode(r); err != nil {
		return err
	}
	if !r.Compatible {
		return incompatible(st, binary)
	}
	return nil
}

func incompatible(st migrations.Status, binary string) error {
	return fmt.Errorf("%w: it needs at least %s; this backend is %s", errIncompatible, st.MinAppVersion, binary)
}

// checkSchema is the startup guard of `backend serve`: it reads the schema
// compatibility once. checked is false when the database could not be read;
// the error is errIncompatible (logged) when this binary may not run. A
// dirty database does not stop serve (developer decision 2026-11-12): it
// is only logged at ERROR; the deploy tool, which saw `migrate up` exit 5,
// restores the pre-update dump.
func checkSchema(ctx context.Context, read compatReader, binary string, log *slog.Logger) (checked bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, compatTimeout)
	defer cancel()
	st, err := read(ctx)
	if err != nil {
		return false, err
	}
	if st.Dirty {
		log.Error(fmt.Sprintf("database dirty at schema version %d: a migration failed part-way; serving anyway", st.SchemaVersion),
			"schema_version", st.SchemaVersion, "version", binary)
	}
	if !st.Compatible(binary) {
		err := incompatible(st, binary)
		log.Error(fmt.Sprintf("database schema needs at least %s; this backend is %s", st.MinAppVersion, binary),
			"min_app_version", st.MinAppVersion, "version", binary, "schema_version", st.SchemaVersion)
		return true, err
	}
	return true, nil
}

// guardSchema repeats checkSchema every retry until the database answers,
// for a backend that started while the database was unreachable. It
// returns errIncompatible (which stops serve) or nil once the schema is
// compatible or ctx ends.
func guardSchema(ctx context.Context, read compatReader, binary string, log *slog.Logger, retry time.Duration) error {
	warned := false
	for {
		checked, err := checkSchema(ctx, read, binary, log)
		if checked {
			return err
		}
		if !warned {
			log.Warn("schema compatibility not checked yet; retrying", logging.KeyError, err)
			warned = true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
}
