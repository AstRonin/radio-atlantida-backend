//go:build integration

package clocks_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	clocksrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/clocks"
)

type fixture struct {
	t        *testing.T
	d        *db.DB
	store    *clocksrepo.Store
	check    *clocksrepo.CheckStore
	playlist int64
	artist   int64
	tracks   []int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.Migrated(t)
	f := &fixture{t: t, d: d, store: clocksrepo.New(d), check: clocksrepo.NewCheck(d.Pool)}
	f.playlist = f.id(`INSERT INTO playlists (name, type_id) SELECT 'Music', id FROM playlist_types WHERE code = 'music' RETURNING id`)
	f.artist = f.id(`INSERT INTO artists (name) VALUES ('ABBA') RETURNING id`)
	for i, dur := range []any{200000, nil} {
		tr := f.id(`INSERT INTO tracks (title, file_path, type_id, duration_ms)
SELECT $1, $2, id, $3 FROM playlist_types WHERE code = 'music' RETURNING id`,
			fmt.Sprintf("T%d", i), fmt.Sprintf("m/%d.mp3", i), dur)
		f.id(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2) RETURNING track_id`, tr, f.playlist)
		f.tracks = append(f.tracks, tr)
	}
	return f
}

func (f *fixture) id(sql string, args ...any) int64 {
	f.t.Helper()
	var id int64
	require.NoError(f.t, f.d.Pool.QueryRow(f.t.Context(), sql, args...).Scan(&id))
	return id
}

// inTx runs fn with the store bound to a transaction, as the service does.
func (f *fixture) inTx(fn func(st clocks.Store) error) error {
	return f.d.WithTx(f.t.Context(), func(_ context.Context, tx db.Tx) error { return fn(f.store.WithTx(tx)) })
}

func (f *fixture) elem(minutes int) clocks.Element {
	return clocks.Element{
		Placement: clocks.PlacementFlow, NominalDurationMS: minutes * 60000, DurationToleranceMS: 30000,
		FixedToleranceMS: 60000, Filler: clocks.Filler{Mode: clocks.FillerNone}, PlaylistIDs: []int64{f.playlist},
	}
}

func TestStoreCreateUpdateKeepsElementIDs(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	fixedEl := f.elem(1)
	fixedEl.Placement, fixedEl.FixedMinute = clocks.PlacementFixed, new(30)
	c := clocks.Clock{
		Name: "Morning", Status: clocks.StatusIncomplete,
		DefaultFiller: &clocks.Filler{Mode: clocks.FillerArtist, SourceID: &f.artist},
		Elements:      []clocks.Element{f.elem(20), fixedEl, f.elem(20)},
	}
	var id int64
	require.NoError(t, f.inTx(func(st clocks.Store) (err error) {
		id, err = st.Create(ctx, c)
		return err
	}))
	got, err := f.store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, clocks.FillerArtist, got.DefaultFiller.Mode)
	require.Equal(t, f.artist, *got.DefaultFiller.SourceID)
	require.Len(t, got.Elements, 3)
	require.Equal(t, 30, *got.Elements[1].FixedMinute)
	require.Equal(t, []int64{f.playlist}, got.Elements[0].PlaylistIDs)

	// Reorder, drop the fixed element, add one, set a filler: kept
	// elements keep their IDs.
	first, last := got.Elements[0], got.Elements[2]
	last.Filler = clocks.Filler{Mode: clocks.FillerPlaylist, SourceID: &f.playlist}
	got.Elements = []clocks.Element{last, first, f.elem(10)}
	got.Status = clocks.StatusComplete
	got.DefaultFiller = nil
	require.NoError(t, f.inTx(func(st clocks.Store) error { return st.Update(ctx, got) }))

	again, err := f.store.Get(ctx, id)
	require.NoError(t, err)
	require.Nil(t, again.DefaultFiller)
	require.Equal(t, clocks.StatusComplete, again.Status)
	require.Len(t, again.Elements, 3)
	require.Equal(t, last.ID, again.Elements[0].ID)
	require.Equal(t, f.playlist, *again.Elements[0].Filler.SourceID)
	require.Equal(t, first.ID, again.Elements[1].ID)
	require.NotZero(t, again.Elements[2].ID)

	list, err := f.store.List(ctx)
	require.NoError(t, err)
	require.Equal(t, []clocks.Summary{{ID: id, Name: "Morning", Status: clocks.StatusComplete, ElementCount: 3, UpdatedAt: list[0].UpdatedAt}}, list)

	// Name taken, unknown playlist.
	err = f.inTx(func(st clocks.Store) (txErr error) { _, txErr = st.Create(ctx, c); return txErr })
	require.ErrorIs(t, err, clocks.ErrNameTaken)
	c.Name = "Other"
	c.Elements = []clocks.Element{f.elem(60)}
	c.Elements[0].PlaylistIDs = []int64{9999}
	err = f.inTx(func(st clocks.Store) (txErr error) { _, txErr = st.Create(ctx, c); return txErr })
	require.ErrorIs(t, err, clocks.ErrUnknownPlaylist)

	missing, err := f.store.MissingPlaylists(ctx, []int64{f.playlist, 9999})
	require.NoError(t, err)
	require.Equal(t, []int64{9999}, missing)
}

func TestStoreDeleteAndPool(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var id int64
	require.NoError(t, f.inTx(func(st clocks.Store) (err error) {
		id, err = st.Create(ctx, clocks.Clock{Name: "C", Status: clocks.StatusComplete, Elements: []clocks.Element{f.elem(60)}})
		return err
	}))

	// The pool keeps used rows and replaces unused ones after them.
	require.NoError(t, f.inTx(func(st clocks.Store) error { return st.SetPool(ctx, id, f.tracks) }))
	_, err := f.d.Pool.Exec(ctx, `UPDATE clock_filler_pool SET used_at = now() WHERE clock_id = $1 AND position = 0`, id)
	require.NoError(t, err)
	require.NoError(t, f.inTx(func(st clocks.Store) error { return st.SetPool(ctx, id, []int64{f.tracks[1]}) }))
	pool, err := f.store.Pool(ctx, id)
	require.NoError(t, err)
	require.Len(t, pool, 2)
	require.NotNil(t, pool[0].UsedAt)
	require.Equal(t, f.tracks[1], pool[1].TrackID)
	require.Equal(t, 1, pool[1].Position, "right after the used row")
	err = f.inTx(func(st clocks.Store) error { return st.SetPool(ctx, id, []int64{9999}) })
	require.ErrorIs(t, err, clocks.ErrUnknownTrack)

	slot := f.id(`INSERT INTO grid_slots (weekday, hour, clock_id, valid_from) VALUES (1, 8, $1, '2026-10-01') RETURNING id`, id)
	require.ErrorIs(t, f.store.Delete(ctx, id), clocks.ErrInUse)
	users, err := f.store.Users(ctx, id, 10)
	require.NoError(t, err)
	require.Len(t, users, 1)
	require.Equal(t, slot, users[0].ID)
	require.Nil(t, users[0].ValidTo)

	_, err = f.d.Pool.Exec(ctx, `DELETE FROM grid_slots WHERE id = $1`, slot)
	require.NoError(t, err)
	f.id(`INSERT INTO play_log (track_id, status, planned_start_utc, started_at, actual_duration_ms, clock_id)
VALUES ($1, 'aired', now(), now(), 1000, $2) RETURNING id`, f.tracks[0], id)
	require.ErrorIs(t, f.store.Delete(ctx, id), clocks.ErrHasHistory)
	require.ErrorIs(t, f.store.Delete(ctx, 9999), clocks.ErrNotFound)
}

func TestCheckStoreCandidates(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	trackRule := f.id(`INSERT INTO play_rules (name, window_from, window_to, weekdays) VALUES ('A', '06:00', '08:00', '{1,2}') RETURNING id`)
	listRule := f.id(`INSERT INTO play_rules (name, max_per_hour) VALUES ('B', 1) RETURNING id`)
	f.id(`INSERT INTO track_play_rules (track_id, rule_id) VALUES ($1, $2) RETURNING rule_id`, f.tracks[0], trackRule)
	other := f.id(`INSERT INTO playlists (name, type_id) SELECT 'Other', id FROM playlist_types WHERE code = 'jingle' RETURNING id`)
	f.id(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2) RETURNING track_id`, f.tracks[0], other)
	f.id(`INSERT INTO playlist_play_rules (playlist_id, rule_id) VALUES ($1, $2) RETURNING rule_id`, other, listRule)

	// The rules of every playlist of the track apply, not only the
	// requested ones.
	got, err := f.check.Candidates(ctx, []int64{f.playlist})
	require.NoError(t, err)
	require.Len(t, got.Tracks, 2)
	require.Equal(t, f.tracks[0], got.Tracks[0].TrackID)
	require.Equal(t, 200000, *got.Tracks[0].DurationMS)
	require.Equal(t, []int64{f.playlist}, got.Tracks[0].PlaylistIDs)
	require.ElementsMatch(t, []int64{trackRule, listRule}, got.Tracks[0].RuleIDs)
	require.Nil(t, got.Tracks[1].DurationMS)
	require.Empty(t, got.Tracks[1].RuleIDs)
	a := got.Rules[trackRule]
	require.Equal(t, "06:00", a.WindowFrom.String())
	require.Equal(t, "08:00", a.WindowTo.String())
	require.Equal(t, []int{1, 2}, a.Weekdays)
	require.Nil(t, got.Rules[listRule].WindowFrom)

	h, err := f.check.TrackSeparationHours(ctx)
	require.NoError(t, err)
	require.Positive(t, h)
}

func TestStoreNotFound(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Get(t.Context(), 9999)
	require.True(t, errors.Is(err, clocks.ErrNotFound))
	err = f.inTx(func(st clocks.Store) error { return st.Lock(t.Context(), 9999) })
	require.ErrorIs(t, err, clocks.ErrNotFound)
}
