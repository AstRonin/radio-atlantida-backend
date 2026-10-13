// Package migrations holds the embedded, forward-only schema migrations and
// the runner behind `backend migrate up` (§9.2 of the design).
//
// Only *.up.sql files exist. An applied migration is never edited; a fix is a
// new migration. There is no down direction.
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

//go:embed *.up.sql
var files embed.FS

// ErrDirty means an earlier migration failed part-way. The schema is in an
// unknown state, and only a restore of the pre-update dump brings it back.
var ErrDirty = errors.New("database is in a dirty migration state")

// Up applies every pending migration in order and logs each version. A
// database that is already newer than this binary (a rollback to an earlier
// release, §9.2) is not an error: expand and contract keeps the older code
// working against it.
//
// Up opens its own connection (db.OpenMigrationDB) and closes it before it
// returns. Cancelling ctx stops the run between migrations; a migration that
// has started runs to its end.
func Up(ctx context.Context, databaseURL string, log *slog.Logger) error {
	src, err := iofs.New(files, ".")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	latest, err := latestVersion(src)
	if err != nil {
		return err
	}

	sqlDB, err := db.OpenMigrationDB(databaseURL)
	if err != nil {
		return err
	}
	drv, err := pgx.WithInstance(sqlDB, &pgx.Config{})
	if err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("open migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", drv)
	if err != nil {
		_ = drv.Close()
		return fmt.Errorf("init migrate: %w", err)
	}
	// Closes the source, the driver, and sqlDB.
	defer func() { _, _ = m.Close() }()

	current, dirty, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		log.Info("empty database, applying all migrations", "latest", latest)
	case err != nil:
		return fmt.Errorf("read schema version: %w", err)
	case dirty:
		return dirtyError(current)
	case current > latest:
		log.Warn("database schema is newer than this release; nothing to apply",
			"version", current, "latest_known", latest)
		return nil
	case current == latest:
		log.Info("schema is up to date", "version", current)
		return nil
	default:
		log.Info("applying migrations", "from", current, "to", latest)
	}

	// Stop between migrations when ctx is cancelled (golang-migrate has no
	// context API).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			m.GracefulStop <- true
		case <-done:
		}
	}()

	// One step at a time, so every applied version is logged.
	for {
		start := time.Now()
		err := m.Steps(1)
		if errors.Is(err, fs.ErrNotExist) {
			break // no more migrations
		}
		if err != nil {
			var de migrate.ErrDirty
			if errors.As(err, &de) {
				return dirtyError(uint(de.Version))
			}
			return fmt.Errorf("apply migration: %w", err)
		}
		v, _, err := m.Version()
		if err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("migrations interrupted at version %d: %w", v, err)
		}
		log.Info("migration applied", "version", v, "duration_ms", time.Since(start).Milliseconds())
	}
	log.Info("migrations complete", "version", latest)
	return nil
}

func dirtyError(version uint) error {
	return fmt.Errorf("%w at version %d: a migration failed part-way. "+
		"Restore the database from the dump taken before the update (update.bat restore), "+
		"then release a fixed migration", ErrDirty, version)
}

// latestVersion returns the highest embedded migration version.
func latestVersion(src source.Driver) (uint, error) {
	v, err := src.First()
	if err != nil {
		return 0, fmt.Errorf("no embedded migrations: %w", err)
	}
	for {
		next, err := src.Next(v)
		if errors.Is(err, fs.ErrNotExist) {
			return v, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read embedded migrations: %w", err)
		}
		v = next
	}
}
