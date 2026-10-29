//go:build integration

package rotation_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	rotationrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/rotation"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

type fixture struct {
	t     *testing.T
	d     *db.DB
	store *rotationrepo.Store
	clock uint64
	// tracks: 0 and 1 by artist A in playlist P (rule R on P), 2 without
	// artist, 3 without a duration.
	tracks   []uint64
	artist   uint64
	playlist uint64
	rule     uint64
	insert   uint64
}

var hour = time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.Migrated(t)
	f := &fixture{t: t, d: d, store: rotationrepo.New(d.Pool)}
	f.artist = f.id(`INSERT INTO artists (name) VALUES ('A') RETURNING id`)
	f.playlist = f.id(`INSERT INTO playlists (name, type_id) SELECT 'P', id FROM playlist_types WHERE code = 'music' RETURNING id`)
	for i, dur := range []any{180000, 200000, 30000, nil} {
		var artist any
		if i < 2 {
			artist = f.artist
		}
		f.tracks = append(f.tracks, f.id(`INSERT INTO tracks (title, artist_id, duration_ms, file_path, type_id)
SELECT $1, $2, $3, $4, id FROM playlist_types WHERE code = 'music' RETURNING id`,
			fmt.Sprintf("T%d", i), artist, dur, fmt.Sprintf("t/%d.mp3", i)))
	}
	for _, tr := range f.tracks[:2] {
		f.exec(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2)`, tr, f.playlist)
	}
	f.rule = f.id(`INSERT INTO play_rules (name, window_from, window_to, max_per_hour, artist_min_interval_minutes)
VALUES ('Day', '06:00', '22:00', 2, 120) RETURNING id`)
	f.exec(`INSERT INTO playlist_play_rules (playlist_id, rule_id) VALUES ($1, $2)`, f.playlist, f.rule)
	f.clock = f.id(`INSERT INTO clocks (name) VALUES ('C') RETURNING id`)
	f.insert = f.id(`INSERT INTO clock_inserts (clock_id, offset_ms, track_id) VALUES ($1, 1800000, $2) RETURNING id`, f.clock, f.tracks[2])
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

func (f *fixture) inTx(fn func(ctx context.Context, st rotation.Store) error) {
	f.t.Helper()
	require.NoError(f.t, f.d.WithTx(f.t.Context(), func(ctx context.Context, tx db.Tx) error {
		st := f.store.WithTx(tx)
		if err := st.LockPlan(ctx); err != nil {
			return err
		}
		return fn(ctx, st)
	}))
}

func (f *fixture) planHour(start time.Time) rotation.PlanHour {
	return rotation.PlanHour{Start: start, ClockID: f.clock, ClockUpdatedAt: hour, Seed: start.Unix(), BuiltAt: hour.Add(-time.Hour)}
}

func TestLibrary(t *testing.T) {
	f := newFixture(t)
	lib, err := f.store.Library(t.Context(), rotation.LibraryScope{All: true})
	require.NoError(t, err)
	require.Len(t, lib.Tracks, 3, "a track without a duration cannot be planned")
	require.Equal(t, f.tracks[0], lib.Tracks[0].ID)
	require.Equal(t, []uint64{f.playlist}, lib.Tracks[0].PlaylistIDs)
	require.Equal(t, []uint64{f.rule}, lib.Tracks[0].RuleIDs, "the rules of its playlists")
	require.Equal(t, f.artist, *lib.Tracks[0].ArtistID)
	require.Empty(t, lib.Tracks[0].Title, "the snapshot does not read titles")
	require.Nil(t, lib.Tracks[2].ArtistID)
	require.Empty(t, lib.Tracks[2].PlaylistIDs)
	r := lib.Rules[f.rule]
	require.Equal(t, "06:00", r.WindowFrom.String())
	require.Equal(t, "22:00", r.WindowTo.String())
	require.Equal(t, 2, *r.MaxPerHour)
	require.Equal(t, 120, *r.ArtistMinIntervalMinutes)
	require.Nil(t, r.MinIntervalMinutes)
	// "Music rotation", created by migration 0013 from the separation rules.
	require.Len(t, lib.Rules, 2)

	titles, err := f.store.TracksOf(t.Context(), []uint64{f.tracks[0]})
	require.NoError(t, err)
	require.Len(t, titles, 1)
	require.Equal(t, "T0", titles[0].Title)
	require.Equal(t, "A", *titles[0].ArtistName)
}

// A scoped snapshot holds the tracks of its playlists and the tracks it names,
// and nothing else; a track in no playlist is left out unless it is named.
func TestLibraryScope(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	all, err := f.store.Library(ctx, rotation.LibraryScope{All: true})
	require.NoError(t, err)

	byPlaylist, err := f.store.Library(ctx, rotation.LibraryScope{Playlists: []uint64{f.playlist}})
	require.NoError(t, err)
	for _, tr := range byPlaylist.Tracks {
		require.Contains(t, tr.PlaylistIDs, f.playlist)
	}
	require.Len(t, byPlaylist.Tracks, 2, "the tracks of the playlist; the one without a playlist is left out")
	require.Len(t, byPlaylist.Rules, len(all.Rules), "every play rule is read")

	named, err := f.store.Library(ctx, rotation.LibraryScope{Playlists: []uint64{f.playlist}, Tracks: []uint64{all.Tracks[2].ID}})
	require.NoError(t, err)
	require.Len(t, named.Tracks, len(all.Tracks), "a named track joins the playlists")
	require.Equal(t, all.Tracks, named.Tracks, "the same rows as the whole library")
}

// The snapshot carries the length on air and the cue in point
// (docs/phase-22-plan.md §3.2); a cue past the end of the file (a duration
// entered by hand since) leaves the track out.
func TestLibraryUsesTheCut(t *testing.T) {
	f := newFixture(t)
	f.exec(`UPDATE tracks SET cue_in_ms = 10000, cue_out_ms = 40000 WHERE id = $1`, f.tracks[0])
	f.exec(`UPDATE tracks SET cue_in_ms = 2000 WHERE id = $1`, f.tracks[1])
	f.exec(`UPDATE tracks SET cue_out_ms = duration_ms + 1000 WHERE id = $1`, f.tracks[2])
	lib, err := f.store.Library(t.Context(), rotation.LibraryScope{All: true})
	require.NoError(t, err)
	require.Len(t, lib.Tracks, 2)
	require.Equal(t, []int{30000, 10000}, []int{lib.Tracks[0].DurationMS, lib.Tracks[0].CueInMS})
	var duration int
	require.NoError(t, f.d.Pool.QueryRow(t.Context(), `SELECT duration_ms FROM tracks WHERE id = $1`, f.tracks[1]).Scan(&duration))
	require.Equal(t, []int{duration - 2000, 2000}, []int{lib.Tracks[1].DurationMS, lib.Tracks[1].CueInMS})
}

func TestReplaceHourAndHistory(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	// Aired an hour before; a missed row does not count.
	f.exec(`INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, started_at, planned_out_ms, actual_duration_ms, clock_id)
VALUES ($1, $2, 'aired', $3, $3, 180000, 180000, $4)`, f.tracks[0], f.artist, hour.Add(-time.Hour), f.clock)
	outage := f.id(`INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component) VALUES ($1, $1, 'icecast', 'icecast') RETURNING id`, hour)
	f.exec(`INSERT INTO play_log (track_id, status, planned_start_utc, planned_out_ms, clock_id, outage_id, resolution)
VALUES ($1, 'missed', $2, 180000, $3, $4, 'open')`, f.tracks[1], hour.Add(-2*time.Hour), f.clock, outage)

	f.inTx(func(ctx context.Context, st rotation.Store) error {
		return st.ReplaceHour(ctx, f.planHour(hour), []rotation.Item{
			{TrackID: f.tracks[1], OffsetMS: 0, OutMS: 200000, Source: rotation.SourceFill},
			{TrackID: f.tracks[0], OffsetMS: 200000, InMS: 5000, OutMS: 100000, Truncated: true, Source: rotation.SourceRule},
		})
	})
	f.inTx(func(ctx context.Context, st rotation.Store) error {
		return st.ReplaceHour(ctx, f.planHour(hour.Add(time.Hour)), []rotation.Item{
			{TrackID: f.tracks[2], OffsetMS: 0, OutMS: 30000, Source: rotation.SourceInsert, ClockInsertID: &f.insert},
		})
	})

	// Rebuilding the hour sees the aired play and the next hour, not its
	// own planned items.
	hist, err := f.store.History(ctx, hour.Add(-72*time.Hour), hour.Add(73*time.Hour), hour)
	require.NoError(t, err)
	require.ElementsMatch(t, []rotation.Play{
		{TrackID: f.tracks[0], ArtistID: &f.artist, Start: hour.Add(-time.Hour)},
		{TrackID: f.tracks[2], Start: hour.Add(time.Hour), Planned: true},
	}, hist)

	plan, err := f.store.Plan(ctx, hour, hour.Add(2*time.Hour))
	require.NoError(t, err)
	require.Len(t, plan, 2)
	require.Equal(t, "C", plan[0].ClockName)
	require.Len(t, plan[0].Items, 2)
	it := plan[0].Items[1]
	require.Equal(t, "T0", it.Title)
	require.Equal(t, "A", *it.ArtistName)
	require.True(t, it.Truncated)
	require.Equal(t, 5000, it.InMS)
	require.Equal(t, 100000, it.OutMS)
	require.Equal(t, 180000, it.DurationMS)
	require.Equal(t, hour.Add(200*time.Second), it.Start)
	require.Equal(t, "rule", it.Source)
	require.Len(t, plan[1].Items, 1)
	require.Equal(t, f.insert, *plan[1].Items[0].InsertID)
	require.Equal(t, "insert", plan[1].Items[0].Source)

	// A rebuild replaces the items; a cleared hour is gone.
	f.inTx(func(ctx context.Context, st rotation.Store) error {
		return st.ReplaceHour(ctx, f.planHour(hour), nil)
	})
	f.inTx(func(ctx context.Context, st rotation.Store) error { return st.ClearHour(ctx, hour.Add(time.Hour)) })
	_, found, err := f.store.PlanHour(ctx, hour.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, found)
	ph, found, err := f.store.PlanHour(ctx, hour)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, f.planHour(hour), ph)
}

// TestExplainHistory prints the plan of the history query over a realistic
// volume, for the comment at Store.History.
func TestExplainHistory(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	// 10 days of play log (15 plays per hour) and 48 hours of plan.
	f.exec(`INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, started_at, planned_out_ms, actual_duration_ms, clock_id)
SELECT $1, $2, 'aired', g, g, 180000, 180000, $3
FROM generate_series($4::timestamptz - interval '10 days', $4::timestamptz, interval '4 minutes') g`,
		f.tracks[0], f.artist, f.clock, hour)
	f.exec(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source)
SELECT $1, $2, g, 180000, 'fill'
FROM generate_series($3::timestamptz, $3::timestamptz + interval '48 hours', interval '4 minutes') g`,
		f.clock, f.tracks[1], hour)
	f.exec(`ANALYZE play_log`)
	f.exec(`ANALYZE scheduled_items`)
	rows, err := f.d.Pool.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY ON)
SELECT pl.track_id, pl.artist_id, pl.started_at AS start
FROM play_log pl
WHERE pl.status IN ('aired', 'truncated') AND pl.started_at >= $1 AND pl.started_at < $2
UNION ALL
SELECT si.track_id, t.artist_id, si.planned_start_utc AS start
FROM scheduled_items si
JOIN tracks t ON t.id = si.track_id
WHERE si.status IN ('planned', 'queued')
  AND si.planned_start_utc >= $1 AND si.planned_start_utc < $2
  AND NOT (si.status = 'planned' AND si.planned_start_utc >= $3 AND si.planned_start_utc < $4)`,
		hour.Add(-72*time.Hour), hour.Add(73*time.Hour), hour.Add(24*time.Hour), hour.Add(25*time.Hour))
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		t.Log(line)
	}
	require.NoError(t, rows.Err())
}

// A build reads the same tracks from the scoped snapshot as from the whole
// library: the items of 24 hours are equal, with a one-time insert of a track
// that no clock playlist holds (it is named in the scope).
func TestFilteredSnapshotBuildsTheSameItems(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	whole, err := f.store.Library(ctx, rotation.LibraryScope{All: true})
	require.NoError(t, err)
	scope := rotation.LibraryScope{Playlists: []uint64{f.playlist}, Tracks: []uint64{f.tracks[2]}}
	scoped, err := f.store.Library(ctx, scope)
	require.NoError(t, err)

	clk := clocks.Clock{ID: f.clock, Name: "C", Playlists: []clocks.ClockPlaylist{
		{PlaylistID: f.playlist, Own: clocks.PlaylistRule{Rule: clocks.RuleFill}},
	}}
	start := hour.Truncate(time.Hour)
	for h := range 24 {
		at := start.Add(time.Duration(h) * time.Hour)
		var inserts []rotation.Insert
		if h == 0 {
			inserts = []rotation.Insert{{
				ID: 1, HourStart: at, TrackID: f.tracks[2], Placement: rotation.PlacementFixed,
				OffsetMS: new(600000), ToleranceMS: 60000,
			}}
		}
		in := func(lib *rotation.Library) rotation.HourInput {
			return rotation.HourInput{
				Start: at, Location: time.UTC, Clock: clk, HourInserts: inserts, Library: lib,
				Settings: rotation.Settings{MinGapMS: 10000},
				Seed:     uint64(at.Unix()), //nolint:gosec // G115: hour starts are after 1970
			}
		}
		a := rotation.Build(in(&whole))
		b := rotation.Build(in(&scoped))
		require.Equal(t, a.Items, b.Items, "hour %d", h)
		require.NotEmpty(t, a.Items, "hour %d", h)
	}
}
