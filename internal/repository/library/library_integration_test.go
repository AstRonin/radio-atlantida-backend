//go:build integration

package library_test

import (
	"context"
	"fmt"
	"log/slog"
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

// A playlist is used by a clock as a clock playlist or as the source of an
// insert; each names the clock. A changed placement rule touches only the
// clocks that use the playlist as a clock playlist.
func TestPlaylistUsers(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	pop := f.playlist("Pop")
	artist := f.id(`INSERT INTO artists (name) VALUES ('ABBA') RETURNING id`)
	f.track("song", &artist)

	inClock := f.id(`INSERT INTO clocks (name, updated_at) VALUES ('A playlist', '2026-01-01Z') RETURNING id`)
	f.id(`INSERT INTO clock_playlists (clock_id, playlist_id, position) VALUES ($1, $2, 0) RETURNING clock_id`, inClock, pop)
	asInsert := f.id(`INSERT INTO clocks (name, updated_at) VALUES ('B insert', '2026-01-01Z') RETURNING id`)
	f.id(`INSERT INTO clock_inserts (clock_id, offset_ms, playlist_id) VALUES ($1, 0, $2) RETURNING id`, asInsert, pop)

	err := f.playlists.Delete(ctx, pop)
	require.ErrorIs(t, err, library.ErrInUse)
	users, err := f.playlists.Users(ctx, pop, library.MaxUsers)
	require.NoError(t, err)
	require.Equal(t, []library.Ref{
		{Kind: library.KindClock, ID: inClock, Name: "A playlist"},
		{Kind: library.KindClock, ID: asInsert, Name: "B insert"},
	}, users)

	touched, err := f.playlists.TouchClocks(ctx, pop)
	require.NoError(t, err)
	require.True(t, touched)
	var stale int
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT count(*) FROM clocks WHERE updated_at > '2026-01-02Z'`).Scan(&stale))
	require.Equal(t, 1, stale, "only the clock that has the playlist")
	touched, err = f.playlists.TouchClocks(ctx, f.playlist("Unused"))
	require.NoError(t, err)
	require.False(t, touched)

	// An artist is used only by its tracks.
	users, err = f.artists.Users(ctx, artist, library.MaxUsers)
	require.NoError(t, err)
	require.Equal(t, []library.Ref{{Kind: library.KindTrack, ID: users[0].ID, Name: "song"}}, users)
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
		page, _, err := f.tracks.List(ctx, library.TrackFilter{PlaylistID: pop, Limit: 10})
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
	      VALUES ($1, $2, now(), 200000, 'fill') RETURNING id`, clock, planned)

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
	page, _, err := f.tracks.List(ctx, library.TrackFilter{Query: "new", Limit: 10})
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
		page, _, lerr := f.tracks.List(ctx, library.TrackFilter{Quality: quality, Limit: 10})
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

// A bulk delete on the real stores: each reference to a track gives its
// refusal, the other tracks go with their links (docs/phase-19-plan.md).
func TestDeleteTracks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	pop := f.playlist("Pop")
	free, inClock, played, planned, inserted := f.track("free", nil), f.track("in clock", nil), f.track("played", nil),
		f.track("planned", nil), f.track("inserted", nil)
	require.NoError(t, f.tracks.SetPlaylists(ctx, free, []uint64{pop}))
	rule := f.id(`INSERT INTO play_rules (name, max_per_hour) VALUES ('once', 1) RETURNING id`)
	f.id(`INSERT INTO track_play_rules (track_id, rule_id) VALUES ($1, $2) RETURNING track_id`, free, rule)

	clock := f.id(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	f.id(`INSERT INTO clock_inserts (clock_id, offset_ms, track_id) VALUES ($1, 0, $2) RETURNING id`, clock, inClock)
	f.id(`INSERT INTO play_log (track_id, status, planned_start_utc, started_at, actual_duration_ms)
	      VALUES ($1, 'aired', now(), now(), 1000) RETURNING id`, played)
	f.id(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source)
	      VALUES ($1, $2, now(), 200000, 'fill') RETURNING id`, clock, planned)
	hourInsert := f.id(`INSERT INTO hour_inserts (hour_start_utc, track_id, placement) VALUES ('2026-10-15 08:00Z', $1, 'start') RETURNING id`, inserted)

	evening := f.id(`INSERT INTO clocks (name) VALUES ('Evening') RETURNING id`)
	f.id(`INSERT INTO clock_inserts (clock_id, offset_ms, track_id) VALUES ($1, 0, $2) RETURNING id`, evening, inClock)
	f.id(`INSERT INTO clock_inserts (clock_id, offset_ms, track_id) VALUES ($1, 60000, $2) RETURNING id`, evening, inClock)
	blockers, err := f.tracks.Blockers(ctx, []uint64{inClock, played, 9999}, 1)
	require.NoError(t, err)
	require.Equal(t, map[uint64]library.TrackBlockers{
		inClock: {Title: "in clock", UsedBy: []library.Ref{{Kind: library.KindClock, ID: evening, Name: "Evening"}}},
		played:  {Title: "played", History: true},
	}, blockers, "a clock once, in name order, at most limit; unknown IDs absent")

	svc := library.NewService(f.d, f.types, f.playlists, f.artists, f.tracks, nil, nil, noneOnAir{}, slog.New(slog.DiscardHandler))
	res, err := svc.DeleteTracks(ctx, []uint64{free, inClock, played, planned, inserted, 9999})
	require.NoError(t, err)
	require.Equal(t, []uint64{free}, res.Deleted)
	require.Equal(t, []library.TrackRefusal{
		{TrackID: inClock, Title: "in clock", Err: library.ErrTrackInUse, UsedBy: []library.Ref{
			{Kind: library.KindClock, ID: evening, Name: "Evening"}, {Kind: library.KindClock, ID: clock, Name: "Morning"},
		}},
		{TrackID: played, Title: "played", Err: library.ErrTrackHasHistory},
		{TrackID: planned, Title: "planned", Err: library.ErrTrackHasHistory},
		{TrackID: inserted, Title: "inserted", Err: library.ErrTrackInUse, UsedBy: []library.Ref{
			{Kind: library.KindHourInsert, ID: hourInsert, Name: "2026-10-15T08:00Z"},
		}},
		{TrackID: 9999, Err: library.ErrTrackNotFound},
	}, res.Refused)

	var links int
	require.NoError(t, f.d.Pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM track_playlists WHERE track_id = $1) + (SELECT count(*) FROM track_play_rules WHERE track_id = $1)`,
		free).Scan(&links))
	require.Zero(t, links, "the links of the deleted track are gone")
}
