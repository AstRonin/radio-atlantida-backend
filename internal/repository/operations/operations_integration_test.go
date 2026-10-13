//go:build integration

package operations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	operationsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"
)

var t0 = time.Date(2026, 10, 13, 6, 44, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func TestStore(t *testing.T) {
	d := dbtest.Migrated(t)
	store := operationsrepo.New(d.Pool)
	ctx := t.Context()

	id, err := store.Insert(ctx, operations.KindRetention, t0)
	require.NoError(t, err)

	t.Run("insert stores a running row", func(t *testing.T) {
		op, err := store.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, operations.Operation{
			ID: id, Kind: "retention", Status: operations.StatusRunning, StartedAt: t0,
		}, utc(op))
	})

	t.Run("finish stores the outcome once", func(t *testing.T) {
		kind, err := store.Finish(ctx, operations.Finish{
			ID: id, Status: operations.StatusCompleted, FinishedAt: t0.Add(time.Minute), ResultSummary: ptr("ok"),
		})
		require.NoError(t, err)
		require.Equal(t, operations.KindRetention, kind)

		op, err := store.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, operations.StatusCompleted, op.Status)
		require.Equal(t, t0.Add(time.Minute), op.FinishedAt.UTC())
		require.Equal(t, "ok", *op.ResultSummary)

		_, err = store.Finish(ctx, operations.Finish{
			ID: id, Status: operations.StatusFailed, FinishedAt: t0, ErrorMessage: ptr("x"),
		})
		require.ErrorIs(t, err, operations.ErrNotRunning)
	})

	t.Run("finish of an unknown operation", func(t *testing.T) {
		_, err := store.Finish(ctx, operations.Finish{ID: 999999, Status: operations.StatusCompleted, FinishedAt: t0})
		require.ErrorIs(t, err, operations.ErrNotRunning)
	})

	t.Run("get unknown", func(t *testing.T) {
		_, err := store.Get(ctx, 999999)
		require.ErrorIs(t, err, operations.ErrNotFound)
	})

	t.Run("fail running and list", func(t *testing.T) {
		running, err := store.Insert(ctx, operations.KindRetention, t0.Add(time.Hour))
		require.NoError(t, err)

		n, err := store.FailRunning(ctx, t0.Add(2*time.Hour), "interrupted")
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
		op, err := store.Get(ctx, running)
		require.NoError(t, err)
		require.Equal(t, operations.StatusFailed, op.Status)
		require.Equal(t, "interrupted", *op.ErrorMessage)

		all, err := store.List(ctx, operations.ListFilter{Limit: 10})
		require.NoError(t, err)
		require.Equal(t, []int64{id, running}, ids(all))

		page, err := store.List(ctx, operations.ListFilter{AfterID: id, Limit: 10})
		require.NoError(t, err)
		require.Equal(t, []int64{running}, ids(page))

		limited, err := store.List(ctx, operations.ListFilter{Limit: 1})
		require.NoError(t, err)
		require.Equal(t, []int64{id}, ids(limited))

		// Started before, finished at the same instant: kept.
		since, err := store.List(ctx, operations.ListFilter{Since: t0.Add(2 * time.Hour), Limit: 10})
		require.NoError(t, err)
		require.Equal(t, []int64{running}, ids(since))
	})
}

func TestStoreWithTx(t *testing.T) {
	d := dbtest.Migrated(t)
	store := operationsrepo.New(d.Pool)
	ctx := t.Context()
	errRollback := errors.New("rollback")

	var id int64
	err := d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		var err error
		id, err = store.WithTx(tx).Insert(ctx, operations.KindRetention, t0)
		require.NoError(t, err)
		_, err = store.WithTx(tx).Get(ctx, id)
		require.NoError(t, err, "visible inside the transaction")
		return errRollback
	})
	require.ErrorIs(t, err, errRollback)

	_, err = store.Get(ctx, id)
	require.ErrorIs(t, err, operations.ErrNotFound, "rolled back")
}

func utc(op operations.Operation) operations.Operation {
	op.StartedAt = op.StartedAt.UTC()
	return op
}

func ids(ops []operations.Operation) []int64 {
	out := make([]int64, len(ops))
	for i, op := range ops {
		out[i] = op.ID
	}
	return out
}
