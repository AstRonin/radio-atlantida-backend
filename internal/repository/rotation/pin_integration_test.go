//go:build integration

package rotation_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
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
	opID, err := svc.Pin(ctx, rotation.PinRequest{PlanItemID: fill.ID, OffsetMS: target})
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

// addRulePlaylist gives every clock of the fixture a spread playlist of five
// jingles.
func addRulePlaylist(t *testing.T, d *db.DB, count int) uint64 {
	t.Helper()
	ctx := t.Context()
	var jingle, playlist uint64
	require.NoError(t, d.Pool.QueryRow(ctx, `SELECT id FROM playlist_types WHERE code = 'jingle'`).Scan(&jingle))
	require.NoError(t, d.Pool.QueryRow(ctx, `INSERT INTO playlists (name, type_id, clock_rule, clock_count)
VALUES ('Jingles', $1, 'spread', $2) RETURNING id`, jingle, count).Scan(&playlist))
	_, err := d.Pool.Exec(ctx, `
INSERT INTO tracks (title, duration_ms, file_path, type_id)
SELECT 'Jingle ' || g, 20000, 'jingle/' || g || '.mp3', $1 FROM generate_series(1, 5) g`, jingle)
	require.NoError(t, err)
	_, err = d.Pool.Exec(ctx, `INSERT INTO track_playlists (track_id, playlist_id)
SELECT id, $1 FROM tracks WHERE title LIKE 'Jingle %'`, playlist)
	require.NoError(t, err)
	_, err = d.Pool.Exec(ctx, `INSERT INTO clock_playlists (clock_id, playlist_id, position) SELECT id, $1, 1 FROM clocks`, playlist)
	require.NoError(t, err)
	return playlist
}

func countSource(hour rotation.PlannedHour, source rotation.Source) int {
	n := 0
	for _, it := range hour.Items {
		if it.Source == string(source) {
			n++
		}
	}
	return n
}

// TestPinnedRuleTrackKeepsItsOffsetAfterRebuildRange pins a planned rule
// track: the pin takes over one slot of the rule, keeps its track and start
// on a rebuild of the range, and the unpin brings the slot back.
func TestPinnedRuleTrackKeepsItsOffsetAfterRebuildRange(t *testing.T) {
	d := dbtest.Migrated(t)
	seedSixMonthFixture(t, d)
	playlist := addRulePlaylist(t, d, 3)
	svc, tracker := fixtureServices(t, d)
	ctx := t.Context()

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
	span := rotation.Range{From: hourAt, To: hourAt.Add(time.Hour)}
	before, err := svc.Plan(ctx, span)
	require.NoError(t, err)
	require.Len(t, before, 1)
	rules := countSource(before[0], rotation.SourceRule)
	require.Equal(t, 3, rules, "the hour has the slots of the rule")
	var row rotation.PlannedItem
	for _, it := range before[0].Items {
		if it.Source == string(rotation.SourceRule) {
			row = it
			break
		}
	}
	require.NotZero(t, row.ID)
	require.NotNil(t, row.Playlist)
	require.Equal(t, playlist, row.Playlist.ID)

	const target = 50 * 60 * 1000
	opID, err := svc.Pin(ctx, rotation.PinRequest{PlanItemID: row.ID, OffsetMS: target})
	require.NoError(t, err)
	waitOperation(t, tracker, opID)

	pinned, err := svc.Plan(ctx, span)
	require.NoError(t, err)
	require.Len(t, pinned[0].Inserts, 1)
	ins := pinned[0].Inserts[0]
	require.Equal(t, row.TrackID, ins.TrackID)
	require.Equal(t, playlist, *ins.RulePlaylistID)
	require.Equal(t, int(row.Start.Sub(hourAt).Milliseconds()), *ins.RuleOriginMS)
	require.Equal(t, rules-1, countSource(pinned[0], rotation.SourceRule), "the pin took over one slot")
	at := hourInsertOf(t, pinned, ins.ID)
	require.Equal(t, row.TrackID, at.TrackID)

	opID, err = svc.RebuildRange(ctx, span)
	require.NoError(t, err)
	waitOperation(t, tracker, opID)

	after, err := svc.Plan(ctx, span)
	require.NoError(t, err)
	require.Equal(t, pinned[0].Inserts, after[0].Inserts, "the pin and its claim stay")
	kept := hourInsertOf(t, after, ins.ID)
	require.Equal(t, at.TrackID, kept.TrackID)
	require.True(t, at.Start.Equal(kept.Start), "the pinned track keeps its start: %s, then %s", at.Start, kept.Start)
	require.Equal(t, rules-1, countSource(after[0], rotation.SourceRule))

	opID, err = svc.RemoveInsert(ctx, ins.ID)
	require.NoError(t, err)
	waitOperation(t, tracker, opID)
	unpinned, err := svc.Plan(ctx, span)
	require.NoError(t, err)
	require.Empty(t, unpinned[0].Inserts)
	require.Equal(t, rules, countSource(unpinned[0], rotation.SourceRule), "the unpin brings the slot back")
}
