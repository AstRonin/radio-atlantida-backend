//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/jobs"
	jobsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/jobs"
)

const key = 42

func TestWithLock(t *testing.T) {
	d := dbtest.Migrated(t)
	// A second pool is a second process.
	other, err := pgxpool.NewWithConfig(t.Context(), d.Pool.Config())
	require.NoError(t, err)
	t.Cleanup(other.Close)
	first, second := jobsrepo.NewLocks(d.Pool), jobsrepo.NewLocks(other)
	ctx := t.Context()

	t.Run("a held lock is refused to another session", func(t *testing.T) {
		ran := false
		require.NoError(t, first.WithLock(ctx, key, func() error {
			err := second.WithLock(ctx, key, func() error {
				ran = true
				return nil
			})
			require.ErrorIs(t, err, jobs.ErrLocked)
			return nil
		}))
		require.False(t, ran)
	})

	t.Run("the lock is released after the run", func(t *testing.T) {
		ran := false
		require.NoError(t, second.WithLock(ctx, key, func() error {
			ran = true
			return nil
		}))
		require.True(t, ran)
	})

	t.Run("other keys do not conflict", func(t *testing.T) {
		require.NoError(t, first.WithLock(ctx, key, func() error {
			return second.WithLock(ctx, key+1, func() error { return nil })
		}))
	})

	t.Run("the error of fn is returned and the lock released", func(t *testing.T) {
		boom := errors.New("boom")
		require.ErrorIs(t, first.WithLock(ctx, key, func() error { return boom }), boom)
		require.NoError(t, second.WithLock(ctx, key, func() error { return nil }))
	})

	t.Run("a cancelled run still releases the lock", func(t *testing.T) {
		rctx, cancel := context.WithCancel(ctx)
		require.NoError(t, first.WithLock(rctx, key, func() error {
			cancel()
			return nil
		}))
		require.NoError(t, second.WithLock(ctx, key, func() error { return nil }))
	})

	require.Zero(t, d.Pool.Stat().AcquiredConns(), "the lock connection goes back to the pool")
}
