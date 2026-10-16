// Package db opens the PostgreSQL connections: a pgx pool for hot paths and a
// GORM handle for admin CRUD on top of the same pool (§5 of the design).
//
// Every connection runs with the session timezone UTC (§10.2) and with
// server-side timeouts, so a stuck statement or an abandoned transaction
// cannot hold locks forever (§1.2 of the init plan).
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Server-side limits applied to every service connection.
const (
	statementTimeout       = 30 * time.Second
	idleInTxSessionTimeout = 60 * time.Second
	applicationName        = "radio-atlantida-backend"
)

// Server-side limits of the migration connection. A migration may run longer
// than an API statement, but it must not queue behind a lock for long: while
// it waits, every query that needs the same table waits behind it.
const (
	migrationLockTimeout = 10 * time.Second
	migrationAppName     = "radio-atlantida-backend-migrate"
)

// DB holds the two handles over one connection pool.
type DB struct {
	// Pool is used for hot paths (play log, rotation, play rule checks,
	// reconciliation).
	Pool *pgxpool.Pool
	// Gorm is used for admin CRUD.
	Gorm *gorm.DB

	sqlDB *sql.DB
}

// Open creates the pool and the GORM handle over it. Like OpenPool, it does
// not wait for the database. Slow GORM queries are logged to log.
func Open(ctx context.Context, databaseURL string, log *slog.Logger) (*DB, error) {
	pool, err := OpenPool(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	// GORM shares the pool, so both handles see the same session settings
	// and one limit on connections.
	sqlDB := stdlib.OpenDBFromPool(pool)
	gdb, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		// Transactions are explicit (DB.WithTx).
		SkipDefaultTransaction: true,
		// Constraint violations come back as gorm.ErrDuplicatedKey etc.,
		// still wrapping *pgconn.PgError with the constraint name.
		TranslateError: true,
		Logger:         newGormLogger(log),
		NowFunc:        func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		_ = sqlDB.Close()
		pool.Close()
		return nil, fmt.Errorf("open gorm: %w", err)
	}
	return &DB{Pool: pool, Gorm: gdb, sqlDB: sqlDB}, nil
}

// Close closes the GORM handle and the pool.
func (d *DB) Close() {
	// Closing the *sql.DB from OpenDBFromPool does not close the pool.
	_ = d.sqlDB.Close()
	d.Pool.Close()
}

// OpenPool creates the pgx pool of the service. It does not wait for the
// database: the pool connects lazily, so the service starts and reports "not
// ready" through /readyz while the database is unavailable.
func OpenPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return openPool(ctx, databaseURL, map[string]string{
		"statement_timeout":                   fmt.Sprint(statementTimeout.Milliseconds()),
		"idle_in_transaction_session_timeout": fmt.Sprint(idleInTxSessionTimeout.Milliseconds()),
		"application_name":                    applicationName,
	})
}

// OpenMigrationDB opens the connection used by `backend migrate up`. It has no
// statement timeout, because a migration over a large table may take longer
// than an API statement, but it gives up on a lock after
// migrationLockTimeout instead of stalling the running service.
func OpenMigrationDB(databaseURL string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, errInvalidURL
	}
	setRuntimeParams(cfg, map[string]string{
		"statement_timeout": "0",
		"lock_timeout":      fmt.Sprint(migrationLockTimeout.Milliseconds()),
		"application_name":  migrationAppName,
	})
	return stdlib.OpenDB(*cfg), nil
}

// errInvalidURL replaces the parse error, which may echo the URL and with it
// the password.
var errInvalidURL = errors.New("parse DATABASE_URL: invalid connection string")

func openPool(ctx context.Context, databaseURL string, params map[string]string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errInvalidURL
	}
	setRuntimeParams(cfg.ConnConfig, params)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

func setRuntimeParams(cfg *pgx.ConnConfig, params map[string]string) {
	cfg.RuntimeParams["timezone"] = "UTC"
	for k, v := range params {
		cfg.RuntimeParams[k] = v
	}
}
