//go:build integration

package playout_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
	playoutrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/playout"
)

type fixture struct {
	t      *testing.T
	d      *db.DB
	store  *playoutrepo.Store
	artist uint64
	// tracks: 0 by the artist (180 s), 1 without an artist (20 s), 2 without
	// a duration.
	tracks   []uint64
	playlist uint64
	clock    uint64
}

var hour = time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.Migrated(t)
	f := &fixture{t: t, d: d, store: playoutrepo.New(d.Pool)}
	f.artist = f.id(`INSERT INTO artists (name) VALUES ('A') RETURNING id`)
	f.playlist = f.id(`INSERT INTO playlists (name, type_id) SELECT 'E', id FROM playlist_types WHERE code = 'music' RETURNING id`)
	for i, dur := range []any{180000, 20000, nil} {
		var artist any
		if i == 0 {
			artist = f.artist
		}
		f.tracks = append(f.tracks, f.id(`INSERT INTO tracks (title, artist_id, duration_ms, file_path, type_id)
SELECT $1, $2, $3, $4, id FROM playlist_types WHERE code = 'music' RETURNING id`,
			fmt.Sprintf("T%d", i), artist, dur, fmt.Sprintf("t/%d.mp3", i)))
		f.exec(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2)`, f.tracks[i], f.playlist)
	}
	f.clock = f.id(`INSERT INTO clocks (name) VALUES ('C') RETURNING id`)
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

// item plans a track at hour+offset.
func (f *fixture) item(track uint64, offset time.Duration, outMS int, truncated bool) uint64 {
	return f.id(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, truncated, source)
VALUES ($1, $2, $3, $4, $5, 'fill') RETURNING id`, f.clock, track, hour.Add(offset), outMS, truncated)
}

type logRow struct {
	Status    string
	Truncated bool
	Started   time.Time
	Ended     *time.Time
	Actual    int
	ArtistID  *uint64
	Source    string
	Planned   *time.Time
}

func (f *fixture) logged(where string, args ...any) logRow {
	f.t.Helper()
	var r logRow
	require.NoError(f.t, f.d.Pool.QueryRow(f.t.Context(), `SELECT status, truncated, started_at, ended_at,
       actual_duration_ms, artist_id, source, planned_start_utc FROM play_log WHERE `+where, args...).
		Scan(&r.Status, &r.Truncated, &r.Started, &r.Ended, &r.Actual, &r.ArtistID, &r.Source, &r.Planned))
	return r
}

func TestItemsAndQueueMarks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	a := f.item(f.tracks[0], 0, 180000, false)
	b := f.item(f.tracks[1], 3*time.Minute, 15000, true)
	f.item(f.tracks[0], time.Hour, 180000, false) // next hour
	missed := f.item(f.tracks[1], 10*time.Minute, 20000, false)
	f.exec(`INSERT INTO broadcast_outages (started_at, detected_by, component) VALUES ($1, 'icecast', 'icecast')`, hour)
	f.exec(`UPDATE scheduled_items SET status = 'missed', missed_at = $2,
  outage_id = (SELECT id FROM broadcast_outages) WHERE id = $1`, missed, hour)

	items, err := f.store.Items(ctx, hour, hour.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, []playout.Item{
		{ID: a, TrackID: f.tracks[0], FilePath: "t/0.mp3", Title: "T0", Artist: "A", Start: hour, OutMS: 180000, FileDurationMS: new(180000)},
		{
			ID: b, TrackID: f.tracks[1], FilePath: "t/1.mp3", Title: "T1", Start: hour.Add(3 * time.Minute), OutMS: 15000, Truncated: true,
			FileDurationMS: new(20000),
		},
	}, items)

	// The fades are the track's now; the cut is the item's.
	f.exec(`UPDATE tracks SET cue_in_ms = 9000, fade_in_ms = 1000, fade_out_ms = 2000 WHERE id = $1`, f.tracks[0])
	items, err = f.store.Items(ctx, hour, hour.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 0, items[0].InMS)
	require.Equal(t, []int{1000, 2000}, []int{items[0].FadeInMS, items[0].FadeOutMS})

	ok, err := f.store.MarkQueued(ctx, a)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = f.store.MarkQueued(ctx, a)
	require.NoError(t, err)
	require.False(t, ok, "only a planned item is marked")
	ok, err = f.store.MarkQueued(ctx, b)
	require.NoError(t, err)
	require.True(t, ok)

	queued, err := f.store.Queued(ctx, 10)
	require.NoError(t, err)
	require.Len(t, queued, 2)
	require.Equal(t, "T0", queued[0].Title)
	require.Equal(t, "A", *queued[0].ArtistName)
	require.Nil(t, queued[1].ArtistName)

	require.NoError(t, f.store.MarkPlanned(ctx, b))
	n, err := f.store.Unqueue(ctx, hour, hour.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	items, err = f.store.Items(ctx, hour, hour.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, items[0].Queued)
	require.False(t, items[1].Queued)
}

func TestStartAndEnd(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	full := f.item(f.tracks[0], 0, 180000, false)
	cut := f.item(f.tracks[0], 3*time.Minute, 180000, false)
	trunc := f.item(f.tracks[1], 6*time.Minute, 15000, true)
	started := hour.Add(2 * time.Second)

	moved, err := f.store.Start(ctx, full, started)
	require.NoError(t, err)
	require.True(t, moved)
	moved, err = f.store.Start(ctx, full, started.Add(time.Minute))
	require.NoError(t, err)
	require.False(t, moved, "a repeated start changes nothing")
	r := f.logged(`scheduled_item_id = $1`, full)
	require.Equal(t, "aired", r.Status)
	require.Equal(t, started, r.Started.UTC())
	require.Nil(t, r.Ended)
	require.Equal(t, 180000, r.Actual, "planned length until the end is recorded")
	require.Equal(t, f.artist, *r.ArtistID)
	require.Equal(t, hour, r.Planned.UTC())
	var left int
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT count(*) FROM scheduled_items WHERE id = $1`, full).Scan(&left))
	require.Zero(t, left, "the plan row is moved, not copied")

	np, ok, err := f.store.Logged(ctx, full)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, full, *np.ItemID)
	require.Equal(t, "T0", np.Title)

	ended, err := f.store.End(ctx, full, 179500, 1000)
	require.NoError(t, err)
	require.True(t, ended)
	ended, err = f.store.End(ctx, full, 3000, 1000)
	require.NoError(t, err)
	require.False(t, ended, "the first end wins")
	r = f.logged(`scheduled_item_id = $1`, full)
	require.Equal(t, "aired", r.Status, "within the cut margin")
	require.Equal(t, 179500, r.Actual)
	require.Equal(t, started.Add(179500*time.Millisecond), r.Ended.UTC())

	for _, id := range []uint64{cut, trunc} {
		moved, err = f.store.Start(ctx, id, started)
		require.NoError(t, err)
		require.True(t, moved)
	}
	require.Equal(t, "truncated", f.logged(`scheduled_item_id = $1`, trunc).Status, "truncated by the plan")
	_, err = f.store.End(ctx, cut, 60000, 1000)
	require.NoError(t, err)
	r = f.logged(`scheduled_item_id = $1`, cut)
	require.Equal(t, "truncated", r.Status, "cut by the switch")
	require.True(t, r.Truncated)

	// An item with an in point: the planned length is out - in, and the
	// in point moves to the play log.
	inPoint := f.id(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_in_ms, planned_out_ms, source)
VALUES ($1, $2, $3, 5000, 45000, 'fill') RETURNING id`, f.clock, f.tracks[0], hour.Add(9*time.Minute))
	moved, err = f.store.Start(ctx, inPoint, started)
	require.NoError(t, err)
	require.True(t, moved)
	require.Equal(t, 40000, f.logged(`scheduled_item_id = $1`, inPoint).Actual, "the planned length")
	var in int
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT planned_in_ms FROM play_log WHERE scheduled_item_id = $1`, inPoint).Scan(&in))
	require.Equal(t, 5000, in)
	np, _, err = f.store.Logged(ctx, inPoint)
	require.NoError(t, err)
	require.Equal(t, 5000, np.InMS)
	require.Equal(t, 45000, *np.OutMS)
	_, err = f.store.End(ctx, inPoint, 39500, 1000)
	require.NoError(t, err)
	require.Equal(t, "aired", f.logged(`scheduled_item_id = $1`, inPoint).Status, "40 s planned, 39.5 s played")

	ended, err = f.store.End(ctx, 999999, 1000, 1000)
	require.NoError(t, err)
	require.False(t, ended)
	moved, err = f.store.Start(ctx, 999999, started)
	require.NoError(t, err)
	require.False(t, moved)
}

func TestEmergency(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	tracks, err := f.store.PlaylistTracks(ctx, f.playlist)
	require.NoError(t, err)
	require.Len(t, tracks, 2, "a track without a duration cannot be played")
	require.Equal(t, "t/0.mp3", tracks[0].FilePath)

	tr, ok, err := f.store.Track(ctx, f.tracks[1])
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 20000, *tr.DurationMS)
	_, ok, err = f.store.Track(ctx, 999999)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, f.store.LogEmergency(ctx, f.tracks[0], hour, 5000))
	r := f.logged(`source = 'emergency'`)
	require.Equal(t, "aired", r.Status)
	require.Equal(t, 5000, r.Actual)
	require.Nil(t, r.Planned)
	require.Equal(t, hour.Add(5*time.Second), r.Ended.UTC())
	// A replayed journal line logs nothing new.
	require.NoError(t, f.store.LogEmergency(ctx, f.tracks[0], hour, 5000))
	var n int
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT count(*) FROM play_log WHERE source = 'emergency'`).Scan(&n))
	require.Equal(t, 1, n)
	require.Error(t, f.store.LogEmergency(ctx, 999999, hour, 5000))
}

func TestLockPlan(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	require.NoError(t, f.d.WithTx(t.Context(), func(ctx context.Context, tx db.Tx) error {
		return f.store.WithTx(tx).LockPlan(ctx)
	}))
}
