//go:build integration

package airwindow

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/airwindow"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
)

var hour = time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)

type fixture struct {
	t      *testing.T
	d      *db.DB
	track  uint64
	clock  uint64
	outage uint64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, d: dbtest.Migrated(t)}
	f.track = f.id(`WITH a AS (INSERT INTO artists (name) VALUES ('A') RETURNING id)
INSERT INTO tracks (title, artist_id, duration_ms, file_path, type_id)
SELECT 'T', a.id, 180000, 't.mp3', pt.id FROM a, playlist_types pt WHERE pt.code = 'music' RETURNING id`)
	f.clock = f.id(`INSERT INTO clocks (name) VALUES ('C') RETURNING id`)
	f.outage = f.id(`INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component)
VALUES ($1, $2, 'icecast', 'icecast') RETURNING id`, hour, hour.Add(time.Hour))
	return f
}

func (f *fixture) id(sql string, args ...any) uint64 {
	f.t.Helper()
	var id uint64
	require.NoError(f.t, f.d.Pool.QueryRow(f.t.Context(), sql, args...).Scan(&id))
	return id
}

// logged adds a play log row: started (aired, truncated) or missed.
func (f *fixture) logged(status string, planned time.Time, started *time.Time) uint64 {
	f.t.Helper()
	switch status {
	case "missed":
		return f.id(`INSERT INTO play_log (track_id, status, planned_start_utc, planned_out_ms, outage_id, resolution)
VALUES ($1, 'missed', $2, 240000, $3, 'open') RETURNING id`, f.track, planned, f.outage)
	default:
		return f.id(`INSERT INTO play_log (track_id, status, planned_start_utc, started_at, planned_out_ms,
                      actual_duration_ms, truncated)
VALUES ($1, $2, $3, $4, 180000, 60000, $2 = 'truncated') RETURNING id`, f.track, status, planned, started)
	}
}

func (f *fixture) planned(status string, start time.Time) uint64 {
	f.t.Helper()
	return f.id(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status)
VALUES ($1, $2, $3, 180000, 'fill', $4) RETURNING id`, f.clock, f.track, start, status)
}

func TestPlayed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := New(f.d.Pool)
	m := func(n int) time.Time { return hour.Add(time.Duration(n) * time.Minute) }

	first := f.logged("aired", m(0), new(m(0)))
	// Missed: due at its planned start, between the two aired rows.
	missed := f.logged("missed", m(3), nil)
	// Started late: due when it started (m(7)), after the missed row.
	truncated := f.logged("truncated", m(2), new(m(7)))
	last := f.logged("aired", m(10), new(m(10)))
	// Plan items are not played rows, whatever their status.
	f.planned("planned", m(-30))
	f.planned("queued", m(-20))

	got, err := s.Played(t.Context(), 3)
	require.NoError(t, err)
	require.Equal(t, []uint64{last, truncated, missed}, playedIDs(got), "the last 3 by due time, newest first")

	got, err = s.Played(t.Context(), 10)
	require.NoError(t, err)
	require.Equal(t, []uint64{last, truncated, missed, first}, playedIDs(got))
	require.Equal(t, airwindow.Played{
		ID: missed, Status: "missed", Source: "auto", TrackID: f.track, Title: "T", ArtistName: new("A"),
		PlannedStart: new(m(3)), PlannedOutMS: new(240000), Resolution: new("open"), OutageID: new(f.outage),
	}, got[2])
	require.Equal(t, "truncated", got[1].Status)
	require.Equal(t, new(m(7)), got[1].StartedAt)
	require.Equal(t, new(60000), got[1].ActualDurationMS)
	require.Nil(t, got[1].Resolution)
}

func TestQueued(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := New(f.d.Pool)
	m := func(n int) time.Time { return hour.Add(time.Duration(n) * time.Minute) }

	f.planned("planned", m(-3)) // start passed, did not air: the outage code owns it
	f.planned("queued", m(-1))  // start passed
	atFrom := f.planned("queued", m(0))
	f.id(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status, outage_id, missed_at)
VALUES ($1, $2, $3, 180000, 'fill', 'missed', $4, $3) RETURNING id`, f.clock, f.track, m(1), f.outage)
	f.id(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status)
VALUES ($1, $2, $3, 180000, 'fill', 'aired') RETURNING id`, f.clock, f.track, m(2))
	later := f.planned("planned", m(6))
	next := f.planned("planned", m(3))
	f.planned("planned", m(9))

	got, err := s.Queued(t.Context(), m(0), 3)
	require.NoError(t, err)
	require.Equal(t, []airwindow.Queued{
		{ID: atFrom, TrackID: f.track, Title: "T", ArtistName: new("A"), Start: m(0), OutMS: 180000, InLiquidsoap: true},
		{ID: next, TrackID: f.track, Title: "T", ArtistName: new("A"), Start: m(3), OutMS: 180000},
		{ID: later, TrackID: f.track, Title: "T", ArtistName: new("A"), Start: m(6), OutMS: 180000},
	}, got, "planned and queued only, from the boundary on, in planned order")
}

func playedIDs(ps []airwindow.Played) []uint64 {
	out := make([]uint64, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

// TestExplain keeps the plans in the doc comments of Played and Queued
// honest: 10 days of play log and 48 hours of plan at 15 items per hour.
func TestExplain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	_, err := f.d.Pool.Exec(ctx, `INSERT INTO play_log (track_id, status, planned_start_utc, started_at, planned_out_ms,
                      actual_duration_ms)
SELECT $1, 'aired', g, g, 180000, 180000
FROM generate_series($2::timestamptz - interval '10 days', $2::timestamptz, interval '4 minutes') g`, f.track, hour)
	require.NoError(t, err)
	_, err = f.d.Pool.Exec(ctx, `INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status)
SELECT $1, $2, g, 180000, 'fill', CASE WHEN g < $3::timestamptz + interval '10 minutes' THEN 'queued' ELSE 'planned' END
FROM generate_series($3::timestamptz, $3::timestamptz + interval '48 hours', interval '4 minutes') g`, f.clock, f.track, hour)
	require.NoError(t, err)
	_, err = f.d.Pool.Exec(ctx, `ANALYZE play_log; ANALYZE scheduled_items`)
	require.NoError(t, err)

	explain := func(sql string, args ...any) string {
		rows, err := f.d.Pool.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY ON) `+sql, args...)
		require.NoError(t, err)
		defer rows.Close()
		var plan []string
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			t.Log(line)
			plan = append(plan, line)
		}
		require.NoError(t, rows.Err())
		return strings.Join(plan, "\n")
	}
	require.Contains(t, explain(playedSQL, 11), "ix_play_log_sort_time")
	require.Contains(t, explain(queuedSQL, hour.Add(time.Minute), 9), "ix_scheduled_items_planned_start_utc")
}
