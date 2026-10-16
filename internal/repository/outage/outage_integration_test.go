//go:build integration

package outage_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
	outagerepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/outage"
)

var hour = time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)

type fixture struct {
	t      *testing.T
	d      *db.DB
	store  *outagerepo.Store
	artist uint64
	track  uint64
	clock  uint64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.Migrated(t)
	f := &fixture{t: t, d: d, store: outagerepo.New(d.Pool)}
	f.artist = f.id(`INSERT INTO artists (name) VALUES ('Artist') RETURNING id`)
	f.track = f.id(`INSERT INTO tracks (title, artist_id, duration_ms, file_path, type_id)
SELECT 'Song', $1, 180000, 'a.mp3', id FROM playlist_types WHERE code = 'music' RETURNING id`, f.artist)
	f.clock = f.id(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	return f
}

func (f *fixture) id(sql string, args ...any) uint64 {
	f.t.Helper()
	var id uint64
	require.NoError(f.t, f.d.Pool.QueryRow(f.t.Context(), sql, args...).Scan(&id))
	return id
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	_, err := f.d.Pool.Exec(f.t.Context(), sql, args...)
	require.NoError(f.t, err)
}

func (f *fixture) count(sql string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.d.Pool.QueryRow(f.t.Context(), sql, args...).Scan(&n))
	return n
}

// item plans a 3-minute item at hour+offset.
func (f *fixture) item(offset time.Duration, status string) uint64 {
	return f.id(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status)
VALUES ($1, $2, $3, 180000, 'fill', $4) RETURNING id`, f.clock, f.track, hour.Add(offset), status)
}

// aired logs an aired row started at hour+offset.
func (f *fixture) aired(offset time.Duration) uint64 {
	at := hour.Add(offset)
	return f.id(`INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, started_at, planned_out_ms, actual_duration_ms, clock_id)
VALUES ($1, $2, 'aired', $3, $3, 180000, 180000, $4) RETURNING id`, f.track, f.artist, at, f.clock)
}

func TestOutageLifecycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	_, open, err := f.store.OpenOutage(ctx)
	require.NoError(t, err)
	require.False(t, open)

	o, err := f.store.CreateOutage(ctx, outage.NewOutage{
		StartedAt: hour, DetectedBy: outage.DetectedByIcecast, Component: outage.ComponentIcecast, Reason: "mount gone",
	}, nil)
	require.NoError(t, err)
	require.Nil(t, o.EndedAt)
	require.Equal(t, "mount gone", *o.Reason)

	_, err = f.store.CreateOutage(ctx, outage.NewOutage{
		StartedAt: hour, DetectedBy: outage.DetectedByBackend, Component: outage.ComponentBackend,
	}, nil)
	require.ErrorIs(t, err, outage.ErrOutageOpen)

	got, open, err := f.store.OpenOutage(ctx)
	require.NoError(t, err)
	require.True(t, open)
	require.Equal(t, o.ID, got.ID)

	require.NoError(t, f.store.CloseOutage(ctx, o.ID, hour.Add(5*time.Minute)))
	require.Error(t, f.store.CloseOutage(ctx, o.ID, hour.Add(6*time.Minute)), "closed already")
	unmarked, err := f.store.Unmarked(ctx)
	require.NoError(t, err)
	require.Len(t, unmarked, 1)
	require.NoError(t, f.store.SetReconciled(ctx, o.ID, hour.Add(6*time.Minute)))
	unmarked, err = f.store.Unmarked(ctx)
	require.NoError(t, err)
	require.Empty(t, unmarked)

	// A closed outage can be recorded while another is open.
	_, err = f.store.CreateOutage(ctx, outage.NewOutage{
		StartedAt: hour.Add(time.Hour), DetectedBy: outage.DetectedByIcecast, Component: outage.ComponentIcecast,
	}, nil)
	require.NoError(t, err)
	end := hour.Add(-time.Minute)
	_, err = f.store.CreateOutage(ctx, outage.NewOutage{
		StartedAt: hour.Add(-time.Hour), DetectedBy: outage.DetectedByBackend, Component: outage.ComponentBackend,
	}, &end)
	require.NoError(t, err)

	list, err := f.store.Outages(ctx, hour.Add(-2*time.Hour), hour.Add(2*time.Hour))
	require.NoError(t, err)
	require.Len(t, list, 3)
	require.Nil(t, list[0].EndedAt, "newest first, the open one")
	require.Equal(t, outage.ComponentBackend, list[2].Component)
}

func TestMarkMissed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	before := f.item(-10*time.Minute, "planned") // ends 7 min before the outage
	// An in point shortens the item: it ends before the outage starts.
	cutShort := f.item(-4*time.Minute, "planned")
	f.exec(`UPDATE scheduled_items SET planned_in_ms = 120000, planned_out_ms = 180000 WHERE id = $1`, cutShort)
	overlap := f.item(-2*time.Minute, "planned") // runs into it
	queued := f.item(2*time.Minute, "queued")    // inside
	f.exec(`UPDATE scheduled_items SET planned_in_ms = 1000 WHERE id = $1`, queued)
	late := f.item(9*time.Minute, "planned")   // inside, but not before the cutoff
	after := f.item(11*time.Minute, "planned") // after the end
	end := hour.Add(10 * time.Minute)
	o, err := f.store.CreateOutage(ctx, outage.NewOutage{
		StartedAt: hour, DetectedBy: outage.DetectedByLiquidsoapCallback, Component: outage.ComponentLiquidsoap,
	}, &end)
	require.NoError(t, err)

	n, err := f.store.MarkMissed(ctx, o, hour.Add(8*time.Minute))

	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, 4, f.count(`SELECT count(*) FROM scheduled_items WHERE id = ANY($1)`, []uint64{before, cutShort, late, after}))
	require.Equal(t, 1, f.count(`SELECT count(*) FROM play_log WHERE scheduled_item_id = $1 AND planned_in_ms = 1000`, queued),
		"the in point moves with the missed item")
	require.Zero(t, f.count(`SELECT count(*) FROM scheduled_items WHERE id = ANY($1)`, []uint64{overlap, queued}))
	require.Equal(t, 2, f.count(`SELECT count(*) FROM play_log
WHERE status = 'missed' AND resolution = 'open' AND outage_id = $1 AND started_at IS NULL
  AND clock_id = $2 AND artist_id = $3 AND scheduled_item_id = ANY($4)`, o.ID, f.clock, f.artist, []uint64{overlap, queued}))

	list, err := f.store.Outages(ctx, hour, hour.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 2, list[0].MissedCount)

	// Again: nothing left to move.
	n, err = f.store.MarkMissed(ctx, o, hour.Add(8*time.Minute))
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestDropStale(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	stale := f.id(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source)
VALUES ($1, $2, $3, 180000, 'fill') RETURNING id`, f.clock, f.track, hour)
	missedRow := f.id(`INSERT INTO broadcast_outages (started_at, ended_at, reconciled_at, detected_by, component)
VALUES ($1, $1, $1, 'icecast', 'icecast') RETURNING id`, hour.Add(-48*time.Hour))
	missed := f.id(`INSERT INTO play_log (track_id, status, planned_start_utc, outage_id, resolution, resolved_at, resolved_item_id)
VALUES ($1, 'missed', $2, $3, 'rescheduled', $2, 1) RETURNING id`, f.track, hour.Add(-48*time.Hour), missedRow)
	rescheduled := f.id(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, reschedule_of_id)
VALUES ($1, $2, $3, 180000, 'reschedule', $4) RETURNING id`, f.clock, f.track, hour.Add(5*time.Minute), missed)
	// In the window of an outage that is not marked yet: kept for marking.
	waiting := f.item(20*time.Minute, "queued")
	end := hour.Add(25 * time.Minute)
	f.exec(`INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component) VALUES ($1, $2, 'icecast', 'icecast')`,
		hour.Add(21*time.Minute), end)
	future := f.item(40*time.Minute, "planned")

	n, err := f.store.DropStale(ctx, hour.Add(30*time.Minute))

	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Zero(t, f.count(`SELECT count(*) FROM scheduled_items WHERE id = ANY($1)`, []uint64{stale, rescheduled}))
	require.Equal(t, 2, f.count(`SELECT count(*) FROM scheduled_items WHERE id = ANY($1)`, []uint64{waiting, future}))
	require.Equal(t, 1, f.count(`SELECT count(*) FROM play_log WHERE id = $1 AND resolution = 'open' AND resolved_at IS NULL`, missed))
	require.Zero(t, f.count(`SELECT count(*) FROM play_log WHERE status <> 'missed'`), "dropped items are not logged")
}

func TestUnairedAndDue(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	_, found, err := f.store.OldestUnaired(ctx, hour)
	require.NoError(t, err)
	require.False(t, found)

	f.item(0, "queued")
	f.item(10*time.Minute, "planned")
	oldest, found, err := f.store.OldestUnaired(ctx, hour.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, hour, oldest)
	n, err := f.store.CountUnaired(ctx, hour, hour.Add(5*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	for _, tc := range []struct {
		from, to time.Duration
		want     bool
	}{
		{2 * time.Minute, 3 * time.Minute, true},  // the first item plays
		{4 * time.Minute, 5 * time.Minute, false}, // a gap
		{4 * time.Minute, 11 * time.Minute, true}, // the second starts
		{14 * time.Minute, time.Hour, false},      // after both
	} {
		due, err := f.store.ItemsDue(ctx, hour.Add(tc.from), hour.Add(tc.to))
		require.NoError(t, err)
		require.Equal(t, tc.want, due, "%v–%v", tc.from, tc.to)
	}
}

func TestHeartbeat(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	_, found, err := f.store.Heartbeat(ctx)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, f.store.SetHeartbeat(ctx, hour))
	require.NoError(t, f.store.SetHeartbeat(ctx, hour.Add(time.Minute)))
	at, found, err := f.store.Heartbeat(ctx)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, hour.Add(time.Minute), at)
	require.Equal(t, 1, f.count(`SELECT count(*) FROM backend_heartbeat`))
}

func TestHistoryMissedAndDismiss(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	var aired []uint64
	for i := range 5 {
		aired = append(aired, f.aired(time.Duration(i)*time.Minute))
	}
	o := f.id(`INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component, reason)
VALUES ($1, $2, 'icecast', 'icecast', 'mount gone') RETURNING id`, hour.Add(10*time.Minute), hour.Add(20*time.Minute))
	missed := f.id(`INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, planned_out_ms, outage_id, resolution, clock_id)
VALUES ($1, $2, 'missed', $3, 180000, $4, 'open', $5) RETURNING id`, f.track, f.artist, hour.Add(12*time.Minute), o, f.clock)
	emergency := f.id(`INSERT INTO play_log (track_id, status, started_at, actual_duration_ms, source)
VALUES ($1, 'aired', $2, 60000, 'emergency') RETURNING id`, f.track, hour.Add(15*time.Minute))

	page, err := f.store.History(ctx, outage.HistoryFilter{From: hour, To: hour.Add(time.Hour), Limit: 3})
	require.NoError(t, err)
	require.Equal(t, []uint64{emergency, missed, aired[4]}, ids(page))
	require.Nil(t, page[0].PlannedStart)
	m := page[1]
	require.Equal(t, outage.StatusMissed, m.Status)
	require.Nil(t, m.StartedAt)
	require.Equal(t, "Morning", *m.ClockName)
	require.Equal(t, "Artist", *m.ArtistName)
	require.Equal(t, outage.ComponentIcecast, m.Outage.Component)
	require.Equal(t, "mount gone", *m.Outage.Reason)
	require.Equal(t, outage.ResolutionOpen, *m.Resolution)

	last := page[2]
	next, err := f.store.History(ctx, outage.HistoryFilter{
		From: hour, To: hour.Add(time.Hour), Limit: 10, After: &outage.Cursor{SortTime: last.SortTime, ID: last.ID},
	})
	require.NoError(t, err)
	require.Equal(t, []uint64{aired[3], aired[2], aired[1], aired[0]}, ids(next))

	status := outage.StatusMissed
	only, err := f.store.History(ctx, outage.HistoryFilter{From: hour, To: hour.Add(time.Hour), Limit: 10, Status: &status})
	require.NoError(t, err)
	require.Equal(t, []uint64{missed}, ids(only))

	open, err := f.store.Missed(ctx, outage.ResolutionOpen, 10)
	require.NoError(t, err)
	require.Equal(t, []uint64{missed}, ids(open))

	e, err := f.store.Dismiss(ctx, missed, hour.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, outage.ResolutionDismissed, *e.Resolution)
	require.Equal(t, hour.Add(time.Hour), *e.ResolvedAt)
	_, err = f.store.Dismiss(ctx, missed, hour.Add(time.Hour))
	require.ErrorIs(t, err, outage.ErrMissedResolved)
	_, err = f.store.Dismiss(ctx, aired[0], hour.Add(time.Hour))
	require.ErrorIs(t, err, outage.ErrMissedNotFound)
	open, err = f.store.Missed(ctx, outage.ResolutionOpen, 10)
	require.NoError(t, err)
	require.Empty(t, open)
}

func ids(es []outage.Entry) []uint64 {
	out := make([]uint64, len(es))
	for i, e := range es {
		out[i] = e.ID
	}
	return out
}
