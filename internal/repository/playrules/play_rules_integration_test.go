//go:build integration

package playrules_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
	playrulesrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/playrules"
)

func tod(s string) *playrules.TimeOfDay {
	t, err := playrules.ParseTimeOfDay(s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestStore(t *testing.T) {
	d := dbtest.Migrated(t)
	store := playrulesrepo.New(d.Pool)
	ctx := t.Context()

	id := func(sql string) uint64 {
		var v uint64
		require.NoError(t, d.Pool.QueryRow(ctx, sql).Scan(&v))
		return v
	}
	jingle := id(`INSERT INTO tracks (title, file_path, type_id)
	              SELECT 'Jingle A', 'j/a.mp3', id FROM playlist_types WHERE code = 'jingle' RETURNING id`)
	jingles := id(`INSERT INTO playlists (name, type_id) SELECT 'Jingles', id FROM playlist_types WHERE code = 'jingle' RETURNING id`)

	t.Run("round trip of every column", func(t *testing.T) {
		in := playrules.RuleInput{
			Name: "Night", WindowFrom: tod("22:00"), WindowTo: tod("02:30"), Weekdays: []int{1, 5},
			MinIntervalMinutes: new(10), ArtistMinIntervalMinutes: new(120), MaxPerHour: new(1), Notes: "n",
		}
		rid, err := store.Create(ctx, in)
		require.NoError(t, err)
		got, err := store.Get(ctx, rid)
		require.NoError(t, err)
		require.Equal(t, in, got.RuleInput)
		require.Equal(t, time.UTC, got.CreatedAt.Location())
		require.Empty(t, got.TrackIDs)

		// Update removes the parts left out.
		require.NoError(t, store.Update(ctx, rid, playrules.RuleInput{Name: "Night", MaxPerHour: new(2)}))
		got, err = store.Get(ctx, rid)
		require.NoError(t, err)
		require.Equal(t, playrules.RuleInput{Name: "Night", MaxPerHour: new(2)}, got.RuleInput)

		_, err = store.Create(ctx, playrules.RuleInput{Name: "Night", MaxPerHour: new(1)})
		require.ErrorIs(t, err, playrules.ErrNameTaken)

		// The migration created "Music rotation" (72 h) from the separation
		// rules; the longest interval counts track and artist intervals.
		longest, err := store.LongestInterval(ctx)
		require.NoError(t, err)
		require.Equal(t, 72*60, longest)
		_, err = store.Create(ctx, playrules.RuleInput{Name: "Artists", ArtistMinIntervalMinutes: new(5 * 1440)})
		require.NoError(t, err)
		longest, err = store.LongestInterval(ctx)
		require.NoError(t, err)
		require.Equal(t, 5*1440, longest)
		_, err = store.Get(ctx, 9999)
		require.ErrorIs(t, err, playrules.ErrNotFound)
		require.ErrorIs(t, store.Update(ctx, 9999, in), playrules.ErrNotFound)
	})

	t.Run("links, in use, users", func(t *testing.T) {
		rid, err := store.Create(ctx, playrules.RuleInput{Name: "Hourly", MaxPerHour: new(1)})
		require.NoError(t, err)
		other, err := store.Create(ctx, playrules.RuleInput{Name: "Spaced", MinIntervalMinutes: new(15)})
		require.NoError(t, err)

		inTx := func(fn func(st playrules.Store) error) error {
			return d.WithTx(ctx, func(_ context.Context, tx db.Tx) error { return fn(store.WithTx(tx)) })
		}
		require.NoError(t, inTx(func(st playrules.Store) error { return st.SetTrackRules(ctx, jingle, []uint64{rid, other}) }))
		require.NoError(t, inTx(func(st playrules.Store) error { return st.SetTrackRules(ctx, jingle, []uint64{other}) }))
		ids, err := store.TrackRules(ctx, jingle)
		require.NoError(t, err)
		require.Equal(t, []uint64{other}, ids, "rid removed, other kept")

		require.NoError(t, inTx(func(st playrules.Store) error {
			return st.SetTargets(ctx, rid, []uint64{jingle}, []uint64{jingles})
		}))
		got, err := store.Get(ctx, rid)
		require.NoError(t, err)
		require.Equal(t, []uint64{jingle}, got.TrackIDs)
		require.Equal(t, []uint64{jingles}, got.PlaylistIDs)
		ids, err = store.PlaylistRules(ctx, jingles)
		require.NoError(t, err)
		require.Equal(t, []uint64{rid}, ids)

		require.ErrorIs(t, store.Delete(ctx, rid), playrules.ErrInUse)
		users, err := store.Users(ctx, rid, playrules.MaxUsers)
		require.NoError(t, err)
		require.Equal(t, []playrules.Ref{
			{Kind: playrules.KindTrack, ID: jingle, Name: "Jingle A"},
			{Kind: playrules.KindPlaylist, ID: jingles, Name: "Jingles"},
		}, users)

		err = inTx(func(st playrules.Store) error { return st.SetPlaylistRules(ctx, jingles, []uint64{9999}) })
		require.ErrorIs(t, err, playrules.ErrUnknownRule)
		err = inTx(func(st playrules.Store) error { return st.SetTargets(ctx, rid, nil, []uint64{9999}) })
		require.ErrorIs(t, err, playrules.ErrUnknownPlaylist)
		err = inTx(func(st playrules.Store) error { return st.LockTrack(ctx, 9999) })
		require.ErrorIs(t, err, playrules.ErrTrackNotFound)

		require.NoError(t, inTx(func(st playrules.Store) error { return st.SetTargets(ctx, rid, nil, nil) }))
		require.NoError(t, store.Delete(ctx, rid))
		require.ErrorIs(t, store.Delete(ctx, rid), playrules.ErrNotFound)
	})
}
