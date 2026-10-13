// Package db opens the PostgreSQL connection pool.
//
// Every connection runs with the session timezone UTC (§10.2) and with
// server-side timeouts, so a stuck statement or an abandoned transaction
// cannot hold locks forever (§1.2 of the init plan).
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Server-side limits applied to every connection.
const (
	statementTimeout       = 30 * time.Second
	idleInTxSessionTimeout = 60 * time.Second
	applicationName        = "radio-atlantida-backend"
)

// OpenPool creates the pgx pool. It does not wait for the database: the pool
// connects lazily, so the service starts and reports "not ready" through
// /readyz while the database is unavailable.
func OpenPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// The parse error may echo the URL, which contains the password.
		return nil, fmt.Errorf("parse DATABASE_URL: invalid connection string")
	}
	params := cfg.ConnConfig.RuntimeParams
	params["timezone"] = "UTC"
	params["statement_timeout"] = fmt.Sprint(statementTimeout.Milliseconds())
	params["idle_in_transaction_session_timeout"] = fmt.Sprint(idleInTxSessionTimeout.Milliseconds())
	params["application_name"] = applicationName

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}
