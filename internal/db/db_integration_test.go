//go:build integration

package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// postgresImage matches the db service in docker-compose.dev.yml.
const postgresImage = "postgres:17.11"

func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, postgresImage,
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

func TestOpenPoolSessionSettings(t *testing.T) {
	url := startPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := OpenPool(ctx, url)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(ctx))

	var tz, stmt, idle string
	err = pool.QueryRow(ctx,
		`SELECT current_setting('TimeZone'), current_setting('statement_timeout'),
		        current_setting('idle_in_transaction_session_timeout')`).Scan(&tz, &stmt, &idle)
	require.NoError(t, err)
	require.Equal(t, "UTC", tz)
	require.Equal(t, "30s", stmt)
	require.Equal(t, "1min", idle)
}
