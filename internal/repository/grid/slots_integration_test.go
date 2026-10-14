//go:build integration

package grid_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	gridrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/grid"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestSlots(t *testing.T) {
	d := dbtest.Migrated(t)
	ctx := t.Context()
	store := gridrepo.New(d)
	var clockID uint64
	require.NoError(t, d.Pool.QueryRow(ctx, `INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`).Scan(&clockID))

	id, err := store.Insert(ctx, grid.NewSlot{Weekday: 1, Hour: 8, ClockID: clockID, ValidFrom: day("2026-10-01"), ValidTo: new(day("2026-10-31"))})
	require.NoError(t, err)
	open, err := store.Insert(ctx, grid.NewSlot{Weekday: 1, Hour: 8, ClockID: clockID, ValidFrom: day("2026-11-01")})
	require.NoError(t, err)

	got, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, grid.Slot{
		ID: id, Weekday: 1, Hour: 8, ValidFrom: day("2026-10-01"), ValidTo: new(day("2026-10-31")),
		Clock: grid.ClockRef{ID: clockID, Name: "Morning", Status: "incomplete"},
	}, got)

	// The exclusion constraint refuses an overlap; an unknown clock is
	// named.
	_, err = store.Insert(ctx, grid.NewSlot{Weekday: 1, Hour: 8, ClockID: clockID, ValidFrom: day("2026-10-31")})
	require.ErrorIs(t, err, grid.ErrConflict)
	_, err = store.Insert(ctx, grid.NewSlot{Weekday: 2, Hour: 8, ClockID: 9999, ValidFrom: day("2026-10-01")})
	require.ErrorIs(t, err, grid.ErrUnknownClock)

	// Filters: dates and clock.
	list, err := store.List(ctx, grid.Filter{From: new(day("2026-11-15")), To: new(day("2026-11-20"))})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, open, list[0].ID)
	require.Nil(t, list[0].ValidTo)
	list, err = store.List(ctx, grid.Filter{ClockID: clockID})
	require.NoError(t, err)
	require.Len(t, list, 2)

	err = d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := store.WithTx(tx)
		locked, lerr := st.LockOverlapping(ctx, 1, 8, day("2026-10-31"), new(day("2026-11-01")))
		require.NoError(t, lerr)
		require.Len(t, locked, 2)
		locked, lerr = st.LockOverlapping(ctx, 1, 8, day("2027-01-01"), nil)
		require.NoError(t, lerr)
		require.Len(t, locked, 1, "the open-ended slot")
		locked, lerr = st.LockOverlapping(ctx, 1, 9, day("2026-10-01"), nil)
		require.NoError(t, lerr)
		require.Empty(t, locked)
		return st.SetRange(ctx, id, day("2026-10-01"), new(day("2026-10-15")))
	})
	require.NoError(t, err)
	got, err = store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, day("2026-10-15"), *got.ValidTo)

	require.NoError(t, store.Delete(ctx, id))
	require.ErrorIs(t, store.Delete(ctx, id), grid.ErrNotFound)
	require.ErrorIs(t, store.SetRange(ctx, id, day("2026-10-01"), nil), grid.ErrNotFound)
	_, err = store.Get(ctx, id)
	require.ErrorIs(t, err, grid.ErrNotFound)
}
