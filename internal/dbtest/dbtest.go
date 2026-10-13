//go:build integration

// Package dbtest starts PostgreSQL in a container for integration tests
// (testcontainers-go). Repository tests run against a real database, never a
// mock.
package dbtest

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/migrations"
)

// Image matches the db service in docker-compose.dev.yml.
const Image = "postgres:17.11"

// StartPostgres starts an empty database for the test and returns its URL.
// The container is removed when the test ends.
func StartPostgres(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	ctr, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("radio"),
		postgres.WithUsername("radio"),
		postgres.WithPassword("radio"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	testcontainers.CleanupContainer(t, ctr)
	require.NoError(t, err)

	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	return url
}

// Migrated starts a database, applies every migration, and returns a DB
// opened like the service opens it.
func Migrated(t *testing.T) *db.DB {
	t.Helper()
	url := StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	require.NoError(t, migrations.Up(ctx, url, Logger(t)))

	d, err := db.Open(ctx, url, Logger(t))
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

// Logger writes log entries to the test output.
func Logger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(logging.NewHandler(t.Output(), slog.LevelDebug))
}
