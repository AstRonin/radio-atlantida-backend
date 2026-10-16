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
	playlist uint64
	tracks   []uint64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.Migrated(t)
	f := &fixture{t: t, d: d, store: clocksrepo.New(d), check: clocksrepo.NewCheck(d.Pool)}
	f.playlist = f.id(`INSERT INTO playlists (name, type_id) SELECT 'Music', id FROM playlist_types WHERE code = 'music' RETURNING id`)
	for i, dur := range []any{200000, nil} {
		tr := f.id(`INSERT INTO tracks (title, file_path, type_id, duration_ms)
SELECT $1, $2, id, $3 FROM playlist_types WHERE code = 'music' RETURNING id`,
			fmt.Sprintf("T%d", i), fmt.Sprintf("m/%d.mp3", i), dur)
		f.id(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2) RETURNING track_id`, tr, f.playlist)
		f.tracks = append(f.tracks, tr)
	}
	return f
}

func (f *fixture) id(sql string, args ...any) uint64 {
	f.t.Helper()
	var id uint64
	require.NoError(f.t, f.d.Pool.QueryRow(f.t.Context(), sql, args...).Scan(&id))
	return id
}

// inTx runs fn with the store bound to a transaction, as the service does.
func (f *fixture) inTx(fn func(st clocks.Store) error) error {
	return f.d.WithTx(f.t.Context(), func(_ context.Context, tx db.Tx) error { return fn(f.store.WithTx(tx)) })
}

func rule(r clocks.Rule) *clocks.Rule { return &r }

func TestStoreCreateUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	jingles := f.id(`INSERT INTO playlists (name, type_id, clock_rule, clock_count)
SELECT 'Jingles', id, 'spread', 2 FROM playlist_types WHERE code = 'jingle' RETURNING id`)

	c := clocks.Clock{
		Name: "Morning",
		Playlists: []clocks.ClockPlaylist{
			{PlaylistID: f.playlist, FillWeight: new(70)},
			{PlaylistID: jingles, Rule: rule(clocks.RuleSpread), Count: new(4)},
		},
		Inserts: []clocks.Insert{
			{OffsetMS: 1_780_000, ToleranceMS: 20_000, TrackID: &f.tracks[0]},
			{OffsetMS: 1_800_000, ToleranceMS: 60_000, PlaylistID: &jingles},
		},
	}
	var id uint64
	require.NoError(t, f.inTx(func(st clocks.Store) (err error) {
		id, err = st.Create(ctx, c)
		return err
	}))
	got, err := f.store.Get(ctx, id)
	require.NoError(t, err)
	require.Len(t, got.Playlists, 2)
	require.Equal(t, clocks.ClockPlaylist{
		PlaylistID: f.playlist, FillWeight: new(70), Own: clocks.PlaylistRule{Rule: clocks.RuleFill},
	}, got.Playlists[0])
	require.Equal(t, clocks.ClockPlaylist{
		PlaylistID: jingles, Rule: rule(clocks.RuleSpread), Count: new(4),
		Own: clocks.PlaylistRule{Rule: clocks.RuleSpread, Count: new(2)},
	}, got.Playlists[1])
	require.Equal(t, 4, *got.Playlists[1].EffectiveCount())
	require.Len(t, got.Inserts, 2)
	require.Equal(t, f.tracks[0], *got.Inserts[0].TrackID)
	require.Equal(t, jingles, *got.Inserts[1].PlaylistID)

	// Reorder the playlists, drop one insert, change the other: the insert
	// at a kept offset keeps its ID.
	kept := got.Inserts[1]
	kept.ToleranceMS = 0
	got.Playlists = []clocks.ClockPlaylist{{PlaylistID: jingles}, {PlaylistID: f.playlist}}
	got.Inserts = []clocks.Insert{kept, {OffsetMS: 0, ToleranceMS: 0, TrackID: &f.tracks[1]}}
	require.NoError(t, f.inTx(func(st clocks.Store) error { return st.Update(ctx, got) }))
	again, err := f.store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, []uint64{jingles, f.playlist}, []uint64{again.Playlists[0].PlaylistID, again.Playlists[1].PlaylistID})
	require.Nil(t, again.Playlists[0].Rule)
	require.Len(t, again.Inserts, 2)
	require.Equal(t, 0, again.Inserts[0].OffsetMS, "by offset")
	require.Equal(t, kept.ID, again.Inserts[1].ID)
	require.Zero(t, again.Inserts[1].ToleranceMS)
	require.True(t, again.UpdatedAt.After(got.UpdatedAt) || again.UpdatedAt.Equal(got.UpdatedAt))

	list, err := f.store.List(ctx)
	require.NoError(t, err)
	require.Equal(t, []clocks.Summary{{ID: id, Name: "Morning", PlaylistCount: 2, InsertCount: 2, UpdatedAt: list[0].UpdatedAt}}, list)

	rules, err := f.store.PlaylistRules(ctx, []uint64{f.playlist, jingles, 9999})
	require.NoError(t, err)
	require.Equal(t, map[uint64]clocks.PlaylistRule{
		f.playlist: {Rule: clocks.RuleFill}, jingles: {Rule: clocks.RuleSpread, Count: new(2)},
	}, rules)
	missing, err := f.store.MissingTracks(ctx, []uint64{f.tracks[0], 9999})
	require.NoError(t, err)
	require.Equal(t, []uint64{9999}, missing)

	// Name taken, unknown playlist, unknown track.
	err = f.inTx(func(st clocks.Store) (txErr error) { _, txErr = st.Create(ctx, c); return txErr })
	require.ErrorIs(t, err, clocks.ErrNameTaken)
	err = f.inTx(func(st clocks.Store) (txErr error) {
		_, txErr = st.Create(ctx, clocks.Clock{Name: "Other", Playlists: []clocks.ClockPlaylist{{PlaylistID: 9999}}})
		return txErr
	})
	require.ErrorIs(t, err, clocks.ErrUnknownPlaylist)
	err = f.inTx(func(st clocks.Store) (txErr error) {
		_, txErr = st.Create(ctx, clocks.Clock{
			Name: "Other", Playlists: []clocks.ClockPlaylist{{PlaylistID: f.playlist}},
			Inserts: []clocks.Insert{{OffsetMS: 0, TrackID: new(uint64(9999))}},
		})
		return txErr
	})
	require.ErrorIs(t, err, clocks.ErrUnknownTrack)
}

func TestStoreDelete(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var id uint64
	require.NoError(t, f.inTx(func(st clocks.Store) (err error) {
		id, err = st.Create(ctx, clocks.Clock{Name: "C", Playlists: []clocks.ClockPlaylist{{PlaylistID: f.playlist}}})
		return err
	}))

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
	got, err := f.check.Candidates(ctx, []uint64{f.playlist})
	require.NoError(t, err)
	require.Len(t, got.Tracks, 2)
	require.Equal(t, f.tracks[0], got.Tracks[0].TrackID)
	require.Equal(t, 200000, *got.Tracks[0].AirMS)
	require.Equal(t, []uint64{f.playlist}, got.Tracks[0].PlaylistIDs)
	require.ElementsMatch(t, []uint64{trackRule, listRule}, got.Tracks[0].RuleIDs)
	require.Nil(t, got.Tracks[1].AirMS, "no duration: cannot be planned")
	require.Empty(t, got.Tracks[1].RuleIDs)
	a := got.Rules[trackRule]
	require.Equal(t, "06:00", a.WindowFrom.String())
	require.Equal(t, "08:00", a.WindowTo.String())
	require.Equal(t, []int{1, 2}, a.Weekdays)
	require.Nil(t, got.Rules[listRule].WindowFrom)
	require.Empty(t, got.PlaylistRules[f.playlist])

	// The rules attached to a requested playlist, with their interval.
	spaced := f.id(`INSERT INTO play_rules (name, min_interval_minutes) VALUES ('C', 30) RETURNING id`)
	f.id(`INSERT INTO playlist_play_rules (playlist_id, rule_id) VALUES ($1, $2) RETURNING rule_id`, f.playlist, spaced)
	got, err = f.check.Candidates(ctx, []uint64{f.playlist})
	require.NoError(t, err)
	require.Equal(t, []uint64{spaced}, got.PlaylistRules[f.playlist])
	require.Equal(t, 30, *got.Rules[spaced].MinIntervalMinutes)
}

func TestStoreNotFound(t *testing.T) {
	f := newFixture(t)
	_, err := f.store.Get(t.Context(), 9999)
	require.True(t, errors.Is(err, clocks.ErrNotFound))
	err = f.inTx(func(st clocks.Store) error { return st.Lock(t.Context(), 9999) })
	require.ErrorIs(t, err, clocks.ErrNotFound)
}
