// Package migrations holds the embedded, forward-only schema migrations and
// the runner behind `backend migrate up` (§9.2 of the design), and the
// downgrade guard behind `backend migrate check` and the startup check of
// `backend serve` (compat.go).
//
// Rules for migration files (docs/deploy-prereqs-plan.md B5, B7; CLAUDE.md):
//
//   - Only *.up.sql files exist. An applied migration is never edited; a fix
//     is a new migration. There is no down direction.
//   - Every file runs in one implicit transaction (golang-migrate sends it as
//     one simple-protocol query), so a failing file leaves nothing behind and
//     the runner resets the version (ErrRolledBack). Explicit transaction
//     control (BEGIN, COMMIT, SAVEPOINT, ...) is forbidden; lint_test.go
//     enforces it.
//   - A statement that cannot run in a transaction (CREATE INDEX
//     CONCURRENTLY, VACUUM, ...) is the only statement of a file whose first
//     line is NonTransactionalMarker. Its failure leaves the database dirty
//     (ErrDirty), and the deploy tool restores the pre-update dump.
//   - From 1.0.0 every migration is expand-only and safe while the previous
//     release runs against it. A contract migration (or any migration an
//     older release cannot work with) ends with
//     UPDATE schema_compat SET min_app_version = '<oldest compatible release>', set_by_migration = <n>, updated_at = now() WHERE id = 1;
//     raises TargetMinAppVersion to the same value, is marked
//     `migrations: contract` in the release tag, and comes no earlier than
//     the release after the one that stopped using what it drops.
package migrations

import (
	"bufio"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

//go:embed *.up.sql
var files embed.FS

// NonTransactionalMarker is the first line of a migration file that holds a
// single statement which cannot run in a transaction block.
const NonTransactionalMarker = "-- migrate: non-transactional"

// ErrDirty means a migration failed part-way, or its outcome is unknown. The
// schema is in an unknown state, and only a restore of the pre-update dump
// brings it back. `backend migrate up` exits with code 5.
var ErrDirty = errors.New("database is in a dirty migration state")

// ErrRolledBack means a migration failed and PostgreSQL rolled the whole
// file back; the runner reset the version to the previous migration, so the
// database is clean and the running release keeps working. `backend migrate
// up` exits with code 4.
var ErrRolledBack = errors.New("migration rolled back")

// Up applies every pending migration in order and logs each version. A
// database that is already newer than this binary (a rollback to an earlier
// release, §9.2) is not an error: expand and contract keeps the older code
// working against it.
//
// A failing migration returns ErrRolledBack when PostgreSQL rolled it back
// (the version is reset, the database is clean), or ErrDirty when it may be
// half applied. Files applied earlier in the same run stay applied.
//
// Up opens its own connection (db.OpenMigrationDB) and closes it before it
// returns. Cancelling ctx stops the run between migrations; a migration that
// has started runs to its end.
func Up(ctx context.Context, databaseURL string, log *slog.Logger) error {
	return up(ctx, files, databaseURL, log)
}

// up is Up over the migration files of fsys (tests add failing files).
func up(ctx context.Context, fsys fs.FS, databaseURL string, log *slog.Logger) error {
	src, err := iofs.New(fsys, ".")
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

	// cur is the applied version, database.NilVersion when none is.
	cur := database.NilVersion
	current, dirty, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		log.Info("empty database, applying all migrations", "latest", latest)
	case err != nil:
		return fmt.Errorf("read schema version: %w", err)
	case dirty:
		return logDirty(log, current, "the database was already dirty")
	case current > latest:
		log.Warn("database schema is newer than this release; nothing to apply",
			"version", current, "latest_known", latest)
		return nil
	case current == latest:
		log.Info("schema is up to date", "version", current)
		return nil
	default:
		cur = int(current)
		log.Info("applying migrations", "from", current, "to", latest)
	}

	// Stop between migrations when ctx is cancelled (golang-migrate has no
	// context API). GracefulStop has a buffer of one and AfterFunc fires at
	// most once, so the send never blocks.
	stop := context.AfterFunc(ctx, func() { m.GracefulStop <- true })
	defer stop()

	// One step at a time, so every applied version is logged and a failure
	// is known to belong to one file.
	for {
		next, err := nextVersion(src, cur)
		if errors.Is(err, fs.ErrNotExist) {
			break // no more migrations
		}
		if err != nil {
			return fmt.Errorf("read embedded migrations: %w", err)
		}
		start := time.Now()
		err = m.Steps(1)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return recoverFailed(m, src, next, cur, err, log)
		}
		v, _, err := m.Version()
		if err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("migrations interrupted at version %d: %w", v, err)
		}
		log.Info("migration applied", "version", v, "duration_ms", time.Since(start).Milliseconds())
		cur = int(v)
	}
	log.Info("migrations complete", "version", latest)
	return nil
}

// recoverFailed handles the failure of the step that applied version v on
// top of prev (database.NilVersion for none). When PostgreSQL answered the
// migration with an error, it rolled the file's implicit transaction back,
// so only golang-migrate's dirty mark is left: Force resets it to prev.
// Anything else (a marked non-transactional file, a broken connection, an
// error while golang-migrate writes the version) leaves the outcome unknown.
func recoverFailed(m *migrate.Migrate, src source.Driver, v uint, prev int, err error, log *slog.Logger) error {
	var de migrate.ErrDirty
	if errors.As(err, &de) {
		return logDirty(log, uint(de.Version), "the database was already dirty")
	}
	short := shortError(err)

	nonTx, herr := nonTransactional(src, v)
	if herr != nil {
		return logDirty(log, v, short+"; "+herr.Error())
	}
	pgErr, ok := serverError(err)
	if nonTx || !ok {
		return logDirty(log, v, short)
	}
	// golang-migrate marks v dirty before it runs the file; nothing else
	// may have changed the version since.
	if got, dirty, verr := m.Version(); verr != nil || got != v || !dirty {
		return logDirty(log, v, short+"; the version is not the failed migration's dirty mark")
	}
	if ferr := m.Force(prev); ferr != nil {
		return logDirty(log, v, short+"; reset of the version failed: "+shortError(ferr))
	}

	schema := max(prev, 0)
	log.Error("migration rolled back",
		"version_failed", v, "schema_version", schema, "sqlstate", pgErr.Code, logging.KeyError, short)
	return fmt.Errorf("%w: migration %d failed (SQLSTATE %s: %s); schema unchanged at version %d",
		ErrRolledBack, v, pgErr.Code, short, schema)
}

// serverError returns the PostgreSQL error of a failed migration file.
// golang-migrate's pgx driver wraps it in a database.Error value (not a
// pointer: those come from its version writes) whose Err starts with
// "migration failed"; database.Error has no Unwrap, so errors.As cannot
// reach the server error through it.
func serverError(err error) (*pgconn.PgError, bool) {
	var de database.Error
	if !errors.As(err, &de) || !strings.HasPrefix(de.Err, "migration failed") {
		return nil, false
	}
	var pgErr *pgconn.PgError
	if !errors.As(de.OrigErr, &pgErr) {
		return nil, false
	}
	return pgErr, true
}

// nonTransactional reports whether the file of version v starts with
// NonTransactionalMarker.
func nonTransactional(src source.Driver, v uint) (bool, error) {
	r, _, err := src.ReadUp(v)
	if err != nil {
		return false, fmt.Errorf("read migration %d: %w", v, err)
	}
	defer func() { _ = r.Close() }()
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false, fmt.Errorf("read migration %d: %w", v, err)
	}
	return strings.TrimSpace(line) == NonTransactionalMarker, nil
}

// shortError is the message of a migration error without the migration's
// SQL: golang-migrate puts the whole file into it.
func shortError(err error) string {
	var de database.Error
	if errors.As(err, &de) {
		return dbErrorMessage(de)
	}
	var dp *database.Error
	if errors.As(err, &dp) {
		return dbErrorMessage(*dp)
	}
	msg, _, _ := strings.Cut(err.Error(), "\n")
	return msg
}

func dbErrorMessage(de database.Error) string {
	// The pgx driver puts a server error's message into Err; any other
	// cause is only in OrigErr.
	msg, line := de.Err, de.Line
	var pgErr *pgconn.PgError
	switch {
	case de.OrigErr == nil:
	case errors.As(de.OrigErr, &pgErr):
		if pgErr.Position == 0 {
			line = 0 // the server gave no position; the driver's line 1 is a guess
		}
	case msg == "":
		msg = de.OrigErr.Error()
	default:
		msg += ": " + de.OrigErr.Error()
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(de.Query)), "\n")
	if line > 0 {
		return fmt.Sprintf("%s in line %d of %q", msg, line, first)
	}
	if first != "" {
		return fmt.Sprintf("%s in %q", msg, first)
	}
	return msg
}

// logDirty logs the one line of a dirty outcome and returns ErrDirty.
func logDirty(log *slog.Logger, version uint, reason string) error {
	log.Error("database dirty", "version", version, logging.KeyError, reason)
	return dirtyError(version, reason)
}

func dirtyError(version uint, reason string) error {
	return fmt.Errorf("%w at version %d (%s): the schema may be half applied. "+
		"The deploy tool restores the pre-update dump; then release a fixed migration", ErrDirty, version, reason)
}

// nextVersion returns the embedded migration after cur
// (database.NilVersion: the first one); fs.ErrNotExist when there is none.
func nextVersion(src source.Driver, cur int) (uint, error) {
	if cur == database.NilVersion {
		return src.First()
	}
	return src.Next(uint(cur))
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
