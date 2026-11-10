//go:build integration

package rotation_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

// waitOperation waits until the operation ends and requires it to complete.
func waitOperation(t *testing.T, tracker *operations.Tracker, id uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		op, err := tracker.Get(t.Context(), id)
		require.NoError(t, err)
		if op.Status != operations.StatusRunning {
			require.Equal(t, operations.StatusCompleted, op.Status, op.ErrorMessage)
			return
		}
		require.True(t, time.Now().Before(deadline), "operation %d did not end", id)
		time.Sleep(10 * time.Millisecond)
	}
}

// hourInsertOf returns the planned item of the one-time insert in the hour.
func hourInsertOf(t *testing.T, plan []rotation.PlannedHour, insertID uint64) rotation.PlannedItem {
	t.Helper()
	require.Len(t, plan, 1, "one hour")
	for _, it := range plan[0].Items {
		if it.HourInsertID != nil && *it.HourInsertID == insertID {
			return it
		}
	}
	require.FailNow(t, "the one-time insert is not placed in the hour")
	return rotation.PlannedItem{}
}

// TestPinnedTrackKeepsItsOffsetAfterRebuildRange pins a planned fill track
// inside an hour, then rebuilds the range that holds the hour: the pinned
// track keeps its track and its place (within its tolerance), and the
// one-time insert stays.
func TestPinnedTrackKeepsItsOffsetAfterRebuildRange(t *testing.T) {
	d := dbtest.Migrated(t)
	seedSixMonthFixture(t, d)
	svc, tracker := fixtureServices(t, d)
	ctx := t.Context()

	// The planner runs as in serve; it stops when the test ends.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- svc.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("planner: %v", err)
		}
	})

	_, err := svc.RunJob(ctx)
	require.NoError(t, err)

	hourAt := fixtureNow.Truncate(time.Hour).Add(48 * time.Hour)
	before, err := svc.Plan(ctx, rotation.Range{From: hourAt, To: hourAt.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, before, 1)
	var fill rotation.PlannedItem
	for _, it := range before[0].Items {
		if it.Source == string(rotation.SourceFill) && it.Start.After(hourAt.Add(10*time.Minute)) {
			fill = it
			break
		}
	}
	require.NotZero(t, fill.ID, "the hour has a fill track to pin")

	const target = 30 * 60 * 1000
	opID, err := svc.PinFill(ctx, rotation.PinRequest{PlanItemID: fill.ID, OffsetMS: target})
	require.NoError(t, err)
	waitOperation(t, tracker, opID)

	pinned, err := svc.Plan(ctx, rotation.Range{From: hourAt, To: hourAt.Add(time.Hour)})
	require.NoError(t, err)
	require.Len(t, pinned[0].Inserts, 1, "the pin is a one-time insert")
	ins := pinned[0].Inserts[0]
	require.Equal(t, fill.TrackID, ins.TrackID, "the pin keeps the track of the fill item")
	require.Equal(t, target, *ins.OffsetMS)
	at := hourInsertOf(t, pinned, ins.ID)
	require.Equal(t, fill.TrackID, at.TrackID)
	want := hourAt.Add(time.Duration(target) * time.Millisecond)
	require.LessOrEqual(t, at.Start.Sub(want).Abs(), time.Duration(ins.ToleranceMS)*time.Millisecond,
		"placed within the tolerance of its offset")

	// A rebuild of the range keeps the pinned track at the same start. The
	// other items of the hour may change; this one must not.
	opID, err = svc.RebuildRange(ctx, rotation.Range{From: hourAt, To: hourAt.Add(time.Hour)})
	require.NoError(t, err)
	waitOperation(t, tracker, opID)

	after, err := svc.Plan(ctx, rotation.Range{From: hourAt, To: hourAt.Add(time.Hour)})
	require.NoError(t, err)
	require.Equal(t, pinned[0].Inserts, after[0].Inserts, "the one-time insert stays")
	kept := hourInsertOf(t, after, ins.ID)
	require.Equal(t, at.TrackID, kept.TrackID)
	require.True(t, at.Start.Equal(kept.Start), "the pinned track keeps its start: %s, then %s", at.Start, kept.Start)
}
