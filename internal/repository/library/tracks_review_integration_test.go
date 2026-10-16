//go:build integration

package library_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	_, err := f.d.Pool.Exec(f.t.Context(), sql, args...)
	require.NoError(f.t, err)
}

// reviewedAt returns the reviewed mark of a track as stored.
func (f *fixture) reviewedAt(ctx context.Context, id uint64) *time.Time {
	f.t.Helper()
	var at *time.Time
	require.NoError(f.t, f.d.Pool.QueryRow(ctx, `SELECT reviewed_at FROM tracks WHERE id = $1`, id).Scan(&at))
	return at
}

func TestReviewedMark(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	a, b := f.track("A", nil), f.track("B", nil)

	found, err := f.tracks.SetReviewed(ctx, []uint64{a, 999999, b}, true)
	require.NoError(t, err)
	require.ElementsMatch(t, []uint64{a, b}, found, "only existing tracks")
	first := f.reviewedAt(ctx, a)
	require.NotNil(t, first)

	// A second mark keeps the first time.
	f.exec(`UPDATE tracks SET reviewed_at = reviewed_at - interval '1 day' WHERE id = $1`, a)
	before := f.reviewedAt(ctx, a)
	_, err = f.tracks.SetReviewed(ctx, []uint64{a}, true)
	require.NoError(t, err)
	require.Equal(t, *before, *f.reviewedAt(ctx, a))

	got, err := f.tracks.Get(ctx, a)
	require.NoError(t, err)
	require.Equal(t, before.UTC(), *got.ReviewedAt)

	found, err = f.tracks.SetReviewed(ctx, []uint64{b}, false)
	require.NoError(t, err)
	require.Equal(t, []uint64{b}, found)
	require.Nil(t, f.reviewedAt(ctx, b))
}

func TestUpdateClearsReviewedMarkOnContentChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	artist, err := f.artists.Create(ctx, "Band")
	require.NoError(t, err)
	other, err := f.artists.Create(ctx, "Other Band")
	require.NoError(t, err)
	jingle := f.id(`SELECT id FROM playlist_types WHERE code = 'jingle'`)
	pl := f.playlist("Rock")

	base := library.TrackInput{
		Title: "Song", ArtistID: &artist, FilePath: "music/song.mp3", TypeID: f.music, DurationMS: new(200000),
	}
	tests := []struct {
		name  string
		edit  func(in *library.TrackInput)
		clear bool
	}{
		{"same content", func(*library.TrackInput) {}, false},
		{"type, notes, quality override", func(in *library.TrackInput) {
			in.TypeID, in.QualityNotes, in.QualityOK = jingle, "checked by ear", new(true)
		}, false},
		{"duration and format", func(in *library.TrackInput) { in.DurationMS, in.Format = new(1000), new("mp3") }, false},
		{"title", func(in *library.TrackInput) { in.Title = "Song (Edit)" }, true},
		{"artist", func(in *library.TrackInput) { in.ArtistID = &other }, true},
		{"artist removed", func(in *library.TrackInput) { in.ArtistID = nil }, true},
		{"file path", func(in *library.TrackInput) { in.FilePath = "music/moved.mp3" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			in.FilePath = "music/" + tt.name + ".mp3"
			id, cerr := f.tracks.Create(ctx, in)
			require.NoError(t, cerr)
			_, cerr = f.tracks.SetReviewed(ctx, []uint64{id}, true)
			require.NoError(t, cerr)

			edited := in
			tt.edit(&edited)
			require.NoError(t, f.tracks.Update(ctx, id, edited))
			require.Equal(t, tt.clear, f.reviewedAt(ctx, id) == nil)
		})
	}

	// Playlists are not content: SetPlaylists keeps the mark.
	id := f.track("Kept", nil)
	_, err = f.tracks.SetReviewed(ctx, []uint64{id}, true)
	require.NoError(t, err)
	require.NoError(t, f.tracks.SetPlaylists(ctx, id, []uint64{pl}))
	require.NotNil(t, f.reviewedAt(ctx, id))
}

func TestReviewedFilterAndSort(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	ids := map[string]uint64{}
	for _, title := range []string{"A", "B", "C", "D", "E"} {
		ids[title] = f.track(title, nil)
	}
	f.exec(`UPDATE tracks SET reviewed_at = $2 WHERE id = $1`, ids["C"], time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	f.exec(`UPDATE tracks SET reviewed_at = $2 WHERE id = $1`, ids["A"], time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	f.exec(`UPDATE tracks SET reviewed_at = $2 WHERE id = $1`, ids["E"], time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))

	titles := func(f2 library.TrackFilter) ([]string, int) {
		t.Helper()
		f2.Limit = 100
		ts, total, err := f.tracks.List(ctx, f2)
		require.NoError(t, err)
		out := make([]string, len(ts))
		for i, tr := range ts {
			out[i] = tr.Title
		}
		return out, total
	}
	got, total := titles(library.TrackFilter{Reviewed: new(true)})
	require.Equal(t, []string{"A", "C", "E"}, got)
	require.Equal(t, 3, total)
	got, total = titles(library.TrackFilter{Reviewed: new(false)})
	require.Equal(t, []string{"B", "D"}, got)
	require.Equal(t, 2, total)

	got, _ = titles(library.TrackFilter{Sort: library.SortReviewed})
	require.Equal(t, []string{"C", "E", "A", "B", "D"}, got, "not reviewed last, ties by title")
	got, _ = titles(library.TrackFilter{Sort: library.SortReviewed, Desc: true})
	require.Equal(t, []string{"A", "E", "C", "B", "D"}, got, "not reviewed last in both orders")

	got, total = titles(library.TrackFilter{Sort: library.SortReviewed, Reviewed: new(false), Offset: 1})
	require.Equal(t, []string{"D"}, got)
	require.Equal(t, 2, total)
}

func newService(f *fixture) *library.Service {
	return library.NewService(f.d, f.types, f.playlists, f.artists, f.tracks, nil, nil, noneOnAir{},
		slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
}

// noneOnAir: nothing is on air.
type noneOnAir struct{}

func (noneOnAir) OnAirTrack() (uint64, bool) { return 0, false }

func TestRenameTracks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	site := f.id(`INSERT INTO tracks (title, file_path, type_id) VALUES ('Song [www.site.com]', 's1.mp3', $1) RETURNING id`, f.music)
	upper := f.id(`INSERT INTO tracks (title, file_path, type_id) VALUES ('Hit [WWW.SITE.COM] Mix', 's2.mp3', $1) RETURNING id`, f.music)
	plain := f.id(`INSERT INTO tracks (title, file_path, type_id) VALUES ('Plain', 's3.mp3', $1) RETURNING id`, f.music)
	only := f.id(`INSERT INTO tracks (title, file_path, type_id) VALUES ('[www.site.com]', 's4.mp3', $1) RETURNING id`, f.music)
	_, err := f.tracks.SetReviewed(ctx, []uint64{site, upper, plain, only}, true)
	require.NoError(t, err)
	svc := newService(f)
	req := library.RenameRequest{TrackIDs: []uint64{site, upper, plain, only, 999999}, Find: " [www.site.com]", DryRun: true}

	preview, err := svc.RenameTracks(ctx, req)
	require.NoError(t, err)
	want := library.RenameResult{
		Changed: []library.TitleChange{
			{TrackID: site, OldTitle: "Song [www.site.com]", NewTitle: "Song"},
			{TrackID: upper, OldTitle: "Hit [WWW.SITE.COM] Mix", NewTitle: "Hit Mix"},
		},
		Unchanged: []uint64{plain, only},
		Refused:   []library.RenameRefusal{{TrackID: 999999, Err: library.ErrTrackNotFound}},
	}
	require.Equal(t, want, preview)
	got, err := f.tracks.Get(ctx, site)
	require.NoError(t, err)
	require.Equal(t, "Song [www.site.com]", got.Title, "a dry run writes nothing")
	require.NotNil(t, got.ReviewedAt)

	// The title changed since the preview: the applied result follows the
	// stored title.
	f.exec(`UPDATE tracks SET title = 'Hit Mix' WHERE id = $1`, upper)
	req.DryRun = false
	applied, err := svc.RenameTracks(ctx, req)
	require.NoError(t, err)
	require.Equal(t, []library.TitleChange{{TrackID: site, OldTitle: "Song [www.site.com]", NewTitle: "Song"}}, applied.Changed)
	require.Equal(t, []uint64{upper, plain, only}, applied.Unchanged)

	got, err = f.tracks.Get(ctx, site)
	require.NoError(t, err)
	require.Equal(t, "Song", got.Title)
	require.Nil(t, got.ReviewedAt, "a renamed track loses its mark")
	require.NotNil(t, f.reviewedAt(ctx, plain), "an unchanged track keeps it")
	require.NotNil(t, f.reviewedAt(ctx, upper))
}

func TestRenameLocksTheRows(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	id := f.track("Locked", nil)
	err := f.d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		titles, err := f.tracks.WithTx(tx).Titles(ctx, []uint64{id}, true)
		require.NoError(t, err)
		require.Equal(t, map[uint64]string{id: "Locked"}, titles)

		// Another session cannot change the row until the transaction ends.
		lctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		_, uerr := f.d.Pool.Exec(lctx, `UPDATE tracks SET title = 'Other' WHERE id = $1`, id)
		require.Error(t, uerr)
		return f.tracks.WithTx(tx).SetTitles(ctx, map[uint64]string{id: `Quote "and" 'apostrophe' \ slash`})
	})
	require.NoError(t, err)
	got, err := f.tracks.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, `Quote "and" 'apostrophe' \ slash`, got.Title)
}
