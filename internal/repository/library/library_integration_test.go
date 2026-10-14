//go:build integration

package library_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	libraryrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/library"
)

type fixture struct {
	t         *testing.T
	d         *db.DB
	types     *libraryrepo.Types
	playlists *libraryrepo.Playlists
	artists   *libraryrepo.Artists
	tracks    *libraryrepo.Tracks
	music     uint64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.Migrated(t)
	f := &fixture{
		t: t, d: d,
		types: libraryrepo.NewTypes(d), playlists: libraryrepo.NewPlaylists(d),
		artists: libraryrepo.NewArtists(d), tracks: libraryrepo.NewTracks(d),
	}
	require.NoError(t, d.Pool.QueryRow(t.Context(), `SELECT id FROM playlist_types WHERE code = 'music'`).Scan(&f.music))
	return f
}

func (f *fixture) id(sql string, args ...any) uint64 {
	f.t.Helper()
	var id uint64
	require.NoError(f.t, f.d.Pool.QueryRow(f.t.Context(), sql, args...).Scan(&id))
	return id
}

func (f *fixture) playlist(name string) uint64 {
	f.t.Helper()
	id, err := f.playlists.Create(f.t.Context(), library.PlaylistInput{Name: name, TypeID: f.music})
	require.NoError(f.t, err)
	return id
}

func (f *fixture) track(title string, artistID *uint64) uint64 {
	f.t.Helper()
	id, err := f.tracks.Create(f.t.Context(), library.TrackInput{
		Title: title, ArtistID: artistID, FilePath: "music/" + title + ".mp3", TypeID: f.music, DurationMS: new(200000),
	})
	require.NoError(f.t, err)
	return id
}

func TestTypes(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	ts, err := f.types.List(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"Jingle", "Music", "News", "Segment"}, []string{ts[0].Name, ts[1].Name, ts[2].Name, ts[3].Name})

	id, err := f.types.Create(ctx, "promo", "Promo")
	require.NoError(t, err)
	_, err = f.types.Create(ctx, "promo", "Other")
	require.ErrorIs(t, err, library.ErrTypeCodeTaken)
	require.ErrorIs(t, f.types.Rename(ctx, id, "Music"), library.ErrTypeNameTaken)
	require.ErrorIs(t, f.types.Rename(ctx, 9999, "X"), library.ErrTypeNotFound)
	require.ErrorIs(t, f.types.Delete(ctx, 9999), library.ErrTypeNotFound)

	// Users lists at most limit of each kind.
	for i := range 3 {
		_, err = f.playlists.Create(ctx, library.PlaylistInput{Name: fmt.Sprintf("P%d", i), TypeID: id})
		require.NoError(t, err)
	}
	_, err = f.tracks.Create(ctx, library.TrackInput{Title: "T", FilePath: "t.mp3", TypeID: id})
	require.NoError(t, err)
	require.ErrorIs(t, f.types.Delete(ctx, id), library.ErrInUse)
	users, err := f.types.Users(ctx, id, 2)
	require.NoError(t, err)
	require.Len(t, users, 3)
	require.Equal(t, []string{"P0", "P1", "T"}, []string{users[0].Name, users[1].Name, users[2].Name})
	require.Equal(t, library.KindTrack, users[2].Kind)

	got, err := f.types.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 3, got.PlaylistCount)
	require.Equal(t, 1, got.TrackCount)
	require.Equal(t, time.UTC, got.CreatedAt.Location())
}

// A playlist is used by a clock in three ways; each one names the clock.
func TestPlaylistUsers(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	pop := f.playlist("Pop")
	artist := f.id(`INSERT INTO artists (name) VALUES ('ABBA') RETURNING id`)

	inElement := f.id(`INSERT INTO clocks (name) VALUES ('B element') RETURNING id`)
	e := f.id(`INSERT INTO clock_elements (clock_id, position, nominal_duration_ms) VALUES ($1, 0, 1000) RETURNING id`, inElement)
	f.id(`INSERT INTO clock_element_playlists (clock_element_id, playlist_id) VALUES ($1, $2) RETURNING playlist_id`, e, pop)
	asFiller := f.id(`INSERT INTO clocks (name) VALUES ('C element filler') RETURNING id`)
	f.id(`INSERT INTO clock_elements (clock_id, position, nominal_duration_ms, filler_mode, filler_playlist_id)
	      VALUES ($1, 0, 1000, 'playlist', $2) RETURNING id`, asFiller, pop)
	asDefault := f.id(`INSERT INTO clocks (name, default_filler_mode, default_filler_playlist_id)
	                   VALUES ('A default filler', 'playlist', $1) RETURNING id`, pop)
	f.id(`INSERT INTO clocks (name, default_filler_mode, default_filler_artist_id) VALUES ('D artist', 'artist', $1) RETURNING id`, artist)

	err := f.playlists.Delete(ctx, pop)
	require.ErrorIs(t, err, library.ErrInUse)
	users, err := f.playlists.Users(ctx, pop, library.MaxUsers)
	require.NoError(t, err)
	require.Equal(t, []library.Ref{
		{Kind: library.KindClock, ID: asDefault, Name: "A default filler"},
		{Kind: library.KindClock, ID: inElement, Name: "B element"},
		{Kind: library.KindClock, ID: asFiller, Name: "C element filler"},
	}, users)

	users, err = f.artists.Users(ctx, artist, library.MaxUsers)
	require.NoError(t, err)
	require.Equal(t, []string{"D artist"}, []string{users[0].Name})
}

func TestPlaylistTracksAndLock(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	pop := f.playlist("Pop")
	a, b, c := f.track("a", nil), f.track("b", nil), f.track("c", nil)

	set := func(ids ...uint64) []uint64 {
		t.Helper()
		require.NoError(t, f.d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
			ps := f.playlists.WithTx(tx)
			if err := ps.Lock(ctx, pop); err != nil {
				return err
			}
			return ps.SetTracks(ctx, pop, ids)
		}))
		page, err := f.tracks.List(ctx, library.TrackFilter{PlaylistID: pop, Limit: 10})
		require.NoError(t, err)
		out := []uint64{}
		for _, tr := range page {
			out = append(out, tr.ID)
		}
		return out
	}
	require.Equal(t, []uint64{a, b}, set(a, b))
	require.Equal(t, []uint64{b, c}, set(b, c), "a removed, b kept, c added")
	require.Equal(t, []uint64{}, set())

	err := f.d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return f.playlists.WithTx(tx).Lock(ctx, 9999)
	})
	require.ErrorIs(t, err, library.ErrPlaylistNotFound)

	err = f.d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		return f.playlists.WithTx(tx).SetTracks(ctx, pop, []uint64{a, 9999})
	})
	require.ErrorIs(t, err, library.ErrUnknownTrack)
}

func TestTrackHistoryAndRollback(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	planned := f.track("planned", nil)
	clock := f.id(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	f.id(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source)
	      VALUES ($1, $2, now(), 200000, 'clock') RETURNING id`, clock, planned)

	require.ErrorIs(t, f.tracks.Delete(ctx, planned), library.ErrTrackHasHistory, "the track plan counts as history")
	require.ErrorIs(t, f.tracks.Delete(ctx, 9999), library.ErrTrackNotFound)
	require.ErrorIs(t, f.tracks.Update(ctx, 9999, library.TrackInput{Title: "x", FilePath: "x.mp3", TypeID: f.music}), library.ErrTrackNotFound)

	// A failing membership rolls back the new track.
	err := f.d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		ts := f.tracks.WithTx(tx)
		id, err := ts.Create(ctx, library.TrackInput{Title: "new", FilePath: "new.mp3", TypeID: f.music})
		if err != nil {
			return err
		}
		return ts.SetPlaylists(ctx, id, []uint64{9999})
	})
	require.ErrorIs(t, err, library.ErrUnknownPlaylist)
	page, err := f.tracks.List(ctx, library.TrackFilter{Query: "new", Limit: 10})
	require.NoError(t, err)
	require.Empty(t, page)
}

func TestTrackQualityFilter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	fresh := f.track("fresh", nil)
	ok := f.track("ok", nil)
	bad := f.track("bad", nil)
	_, err := f.d.Pool.Exec(ctx, `
UPDATE tracks SET analyzed_at = now(), quality_issues = '{}', quality_ok = true, codec = 'mp3', channels = 2,
                  loudness_lufs = -16.1, true_peak_dbtp = -1.2, loudness_range_lu = 4.5
WHERE id = $1`, ok)
	require.NoError(t, err)
	_, err = f.d.Pool.Exec(ctx, `
UPDATE tracks SET analyzed_at = now(), quality_issues = '{loudness,low_bitrate}', quality_ok = false, source_bitrate = 96000
WHERE id = $1`, bad)
	require.NoError(t, err)

	ids := func(quality string) []uint64 {
		page, lerr := f.tracks.List(ctx, library.TrackFilter{Quality: quality, Limit: 10})
		require.NoError(t, lerr)
		var out []uint64
		for _, tr := range page {
			out = append(out, tr.ID)
		}
		return out
	}
	require.Equal(t, []uint64{ok}, ids(library.QualityOK))
	require.Equal(t, []uint64{bad}, ids(library.QualityIssuesFound))
	require.Equal(t, []uint64{fresh}, ids(library.QualityUnanalysed))
	require.Len(t, ids(""), 3)

	tr, err := f.tracks.Get(ctx, ok)
	require.NoError(t, err)
	require.Equal(t, "mp3", *tr.Codec)
	require.Equal(t, 2, *tr.Channels)
	require.Equal(t, -16.1, *tr.LoudnessLUFS)
	require.Equal(t, -1.2, *tr.TruePeakDBTP)
	require.Equal(t, 4.5, *tr.LoudnessRangeLU)
	require.Empty(t, tr.QualityIssues)
	require.NotNil(t, tr.AnalyzedAt)

	tr, err = f.tracks.Get(ctx, bad)
	require.NoError(t, err)
	require.Equal(t, []library.QualityIssue{library.IssueLoudness, library.IssueLowBitrate}, tr.QualityIssues)
	require.Equal(t, 96000, *tr.SourceBitrate)

	tr, err = f.tracks.Get(ctx, fresh)
	require.NoError(t, err)
	require.Nil(t, tr.AnalyzedAt)
	require.NotNil(t, tr.QualityIssues)
	require.Empty(t, tr.QualityIssues)

	// The database takes every issue code of the domain, and no other.
	all := make([]string, 0, len(library.QualityIssues()))
	for _, c := range library.QualityIssues() {
		all = append(all, string(c))
	}
	_, err = f.d.Pool.Exec(ctx, `UPDATE tracks SET quality_issues = $2 WHERE id = $1`, bad, all)
	require.NoError(t, err)
	_, err = f.d.Pool.Exec(ctx, `UPDATE tracks SET quality_issues = '{noise}' WHERE id = $1`, bad)
	require.ErrorContains(t, err, "ck_tracks_quality_issues")
}
