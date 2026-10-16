//go:build integration

package ingest_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/ingest"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	ingestrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/ingest"
)

var now = time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)

type fixture struct {
	t      *testing.T
	d      *db.DB
	store  *ingestrepo.Store
	music  uint64
	pop    uint64
	jazz   uint64
	clock  uint64
	artist uint64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d := dbtest.Migrated(t)
	f := &fixture{t: t, d: d, store: ingestrepo.New(d.Pool)}
	f.music = f.id(`SELECT id FROM playlist_types WHERE code = 'music'`)
	f.pop = f.id(`INSERT INTO playlists (name, type_id) VALUES ('Pop', $1) RETURNING id`, f.music)
	f.jazz = f.id(`INSERT INTO playlists (name, type_id) VALUES ('Jazz', $1) RETURNING id`, f.music)
	f.clock = f.id(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	f.artist = f.id(`INSERT INTO artists (name) VALUES ('ABBA') RETURNING id`)
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

func analysis(path, sum string, issues ...library.QualityIssue) ingest.Analysis {
	return ingest.Analysis{
		FilePath: path, Checksum: sum, AnalyzedAt: now,
		Measurements: ingest.Measurements{
			DurationMS: new(180000), Format: new(media.FormatMP3), Codec: new("mp3"), Bitrate: new(320000),
			SampleRate: new(44100), Channels: new(2), LoudnessLUFS: new(-16.2), TruePeakDBTP: new(-1.4),
			LoudnessRangeLU: new(5.5),
		},
		Issues: append([]library.QualityIssue{}, issues...),
	}
}

func TestCreateAndRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()

	ok, err := f.store.TypeExists(ctx, f.music)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = f.store.TypeExists(ctx, 999999)
	require.NoError(t, err)
	require.False(t, ok)
	missing, err := f.store.MissingPlaylists(ctx, []uint64{f.jazz, 999998, f.pop, 999997})
	require.NoError(t, err)
	require.Equal(t, []uint64{999997, 999998}, missing)

	a := analysis("library/song.mp3", "sum-1", library.IssueLoudness)
	a.Measurements.SourceBitrate = new(96000)
	id, err := f.store.CreateTrack(ctx, ingest.NewTrack{
		Title: "Song", ArtistID: &f.artist, TypeID: f.music, PlaylistIDs: []uint64{f.pop, f.jazz}, Analysis: a,
	})
	require.NoError(t, err)

	byChecksum, ok, err := f.store.TrackByChecksum(ctx, "sum-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, ingest.TrackFile{ID: id, FilePath: "library/song.mp3"}, byChecksum)
	_, ok, err = f.store.TrackByChecksum(ctx, "sum-2")
	require.NoError(t, err)
	require.False(t, ok)
	got, ok, err := f.store.TrackByPath(ctx, "library/song.mp3")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, id, got)

	ts, err := f.store.Tracks(ctx, []uint64{id, 999999})
	require.NoError(t, err)
	require.Len(t, ts, 1)
	require.Equal(t, ingest.Track{
		ID: id, FilePath: "library/song.mp3", Checksum: new("sum-1"), Measurements: a.Measurements,
		Issues: []library.QualityIssue{library.IssueLoudness}, AnalyzedAt: new(now),
	}, ts[0])

	var qualityOK bool
	var playlists int
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT quality_ok, (SELECT count(*) FROM track_playlists WHERE track_id = $1)
FROM tracks WHERE id = $1`, id).Scan(&qualityOK, &playlists))
	require.False(t, qualityOK)
	require.Equal(t, 2, playlists)

	_, err = f.store.CreateTrack(ctx, ingest.NewTrack{Title: "Copy", TypeID: f.music, Analysis: analysis("library/song.mp3", "sum-3")})
	require.ErrorIs(t, err, library.ErrFilePathTaken)
	_, err = f.store.CreateTrack(ctx, ingest.NewTrack{Title: "X", TypeID: 999999, Analysis: analysis("library/x.mp3", "sum-4")})
	require.ErrorIs(t, err, library.ErrUnknownType)
	err = f.d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		_, cerr := f.store.WithTx(tx).CreateTrack(ctx, ingest.NewTrack{
			Title: "Y", TypeID: f.music, PlaylistIDs: []uint64{999999}, Analysis: analysis("library/y.mp3", "sum-5"),
		})
		return cerr
	})
	require.ErrorIs(t, err, library.ErrUnknownPlaylist)
}

func TestArtistExists(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ok, err := f.store.ArtistExists(t.Context(), f.artist)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = f.store.ArtistExists(t.Context(), 999999)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestTrackPaths(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	ids := map[string]uint64{}
	for _, p := range []string{
		"music/a.mp3", "music/rock/b.optimal.mp3", "music-old/c.mp3", "music0/d.mp3", "musicx.mp3", "jingles/id.mp3", "top.mp3",
	} {
		ids[p] = f.id(`INSERT INTO tracks (title, file_path, type_id) VALUES ($1, $1, $2) RETURNING id`, p, f.music)
	}

	got, err := f.store.TrackPaths(ctx, "music")
	require.NoError(t, err)
	require.Equal(t, map[string]uint64{"music/a.mp3": ids["music/a.mp3"], "music/rock/b.optimal.mp3": ids["music/rock/b.optimal.mp3"]}, got,
		"only below music/, not music-old, music0, or musicx.mp3")
	got, err = f.store.TrackPaths(ctx, "music/rock")
	require.NoError(t, err)
	require.Len(t, got, 1)
	got, err = f.store.TrackPaths(ctx, "")
	require.NoError(t, err)
	require.Equal(t, ids, got)
	got, err = f.store.TrackPaths(ctx, "missing")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestEnsureArtist(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	id, err := f.store.EnsureArtist(ctx, "abba")
	require.NoError(t, err)
	require.Equal(t, f.artist, id, "found whatever the case")
	other, err := f.store.EnsureArtist(ctx, "Queen")
	require.NoError(t, err)
	require.NotEqual(t, f.artist, other)
	again, err := f.store.EnsureArtist(ctx, "Queen")
	require.NoError(t, err)
	require.Equal(t, other, again)
}

func TestAnalysisUpdates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	// Registered by hand: never analysed.
	id := f.id(`INSERT INTO tracks (title, file_path, type_id, quality_ok) VALUES ('Old', 'old.wav', $1, true) RETURNING id`, f.music)
	f.id(`INSERT INTO tracks (title, file_path, type_id, analyzed_at) VALUES ('Done', 'done.mp3', $1, now()) RETURNING id`, f.music)

	ts, err := f.store.Unanalysed(ctx, 10)
	require.NoError(t, err)
	require.Len(t, ts, 1)
	require.Equal(t, id, ts[0].ID)
	require.Empty(t, ts[0].Issues)
	require.Nil(t, ts[0].AnalyzedAt)

	require.NoError(t, f.store.SaveAnalysis(ctx, id, analysis("old.optimal.mp3", "sum-9", library.IssueChannels, library.IssueLowBitrate)))
	ts, err = f.store.Tracks(ctx, []uint64{id})
	require.NoError(t, err)
	require.Equal(t, "old.optimal.mp3", ts[0].FilePath)
	require.Equal(t, []library.QualityIssue{library.IssueChannels, library.IssueLowBitrate}, ts[0].Issues)
	var qualityOK bool
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT quality_ok FROM tracks WHERE id = $1`, id).Scan(&qualityOK))
	require.False(t, qualityOK, "the analysis replaces the manual value")

	require.NoError(t, f.store.MarkMissing(ctx, id, now.Add(time.Hour)))
	ts, err = f.store.Tracks(ctx, []uint64{id})
	require.NoError(t, err)
	require.Equal(t, []library.QualityIssue{library.IssueFileMissing}, ts[0].Issues)
	require.Equal(t, now.Add(time.Hour), *ts[0].AnalyzedAt)
	require.Equal(t, -16.2, *ts[0].Measurements.LoudnessLUFS, "measurements stay")

	require.ErrorIs(t, f.store.SaveAnalysis(ctx, 999999, analysis("z.mp3", "z")), library.ErrTrackNotFound)
	require.ErrorIs(t, f.store.MarkMissing(ctx, 999999, now), library.ErrTrackNotFound)
	f.id(`INSERT INTO tracks (title, file_path, type_id) VALUES ('Other', 'other.mp3', $1) RETURNING id`, f.music)
	require.ErrorIs(t, f.store.SaveAnalysis(ctx, id, analysis("other.mp3", "s")), library.ErrFilePathTaken)
}

func TestBusy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	track := func(name string) uint64 {
		return f.id(`INSERT INTO tracks (title, file_path, type_id, duration_ms, artist_id) VALUES ($1, $1, $2, 180000, $3) RETURNING id`,
			name, f.music, f.artist)
	}
	queued, soon, later, onAir, ended, stale, free := track("q"), track("s"), track("l"), track("a"), track("e"), track("st"), track("f")
	item := func(id uint64, start time.Time, status string) {
		f.exec(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status)
VALUES ($1, $2, $3, 180000, 'clock', $4)`, f.clock, id, start, status)
	}
	item(queued, now.Add(time.Hour), "queued") // queued far ahead still counts
	item(soon, now.Add(5*time.Minute), "planned")
	item(later, now.Add(30*time.Minute), "planned")
	logRow := func(id uint64, started time.Time, ended *time.Time) {
		f.exec(`INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, started_at, ended_at, planned_out_ms,
                      actual_duration_ms, clock_id)
VALUES ($1, $2, 'aired', $3, $3, $4, 180000, 0, $5)`, id, f.artist, started, ended, f.clock)
	}
	logRow(onAir, now.Add(-time.Minute), nil)
	logRow(ended, now.Add(-5*time.Minute), new(now.Add(-2*time.Minute)))
	logRow(stale, now.Add(-7*time.Hour), nil) // its end callback was lost

	busy, err := f.store.Busy(ctx, []uint64{queued, soon, later, onAir, ended, stale, free}, now, now.Add(10*time.Minute))
	require.NoError(t, err)
	require.Equal(t, []uint64{queued, soon, onAir}, busy)
}

func TestSaveAnalysisClearsReviewedMarkOnNewChecksum(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	id := f.id(`INSERT INTO tracks (title, file_path, type_id, file_checksum, reviewed_at)
VALUES ('Song', 'song.mp3', $1, 'sum-1', now()) RETURNING id`, f.music)
	reviewed := func() bool {
		t.Helper()
		var ok bool
		require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT reviewed_at IS NOT NULL FROM tracks WHERE id = $1`, id).Scan(&ok))
		return ok
	}

	require.NoError(t, f.store.SaveAnalysis(ctx, id, analysis("song.mp3", "sum-1", library.IssueLoudness)))
	require.True(t, reviewed(), "an analysis of the same file keeps the mark")

	// A transcode (replace or .optimal copy) or a file replaced on disk.
	require.NoError(t, f.store.SaveAnalysis(ctx, id, analysis("song.optimal.mp3", "sum-2")))
	require.False(t, reviewed(), "a new checksum clears it")
}
