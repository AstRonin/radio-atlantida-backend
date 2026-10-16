//go:build integration

package library_test

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// sortRow is what the sort of a fixture track depends on; nil is missing.
type sortRow struct {
	id       uint64
	title    string
	artist   *string
	typeName string
	duration *int
	format   *string // "" is stored and sorts as missing
	analysed bool
	issues   int
	reviewed *int // hours after the fixture's base time
}

func (r sortRow) missing(s library.TrackSort) bool {
	switch s {
	case library.SortArtist:
		return r.artist == nil
	case library.SortDuration:
		return r.duration == nil
	case library.SortFormat:
		return r.format == nil || *r.format == ""
	case library.SortQuality:
		return !r.analysed
	case library.SortReviewed:
		return r.reviewed == nil
	default:
		return false
	}
}

// compareKey compares the sort keys of two rows that both have them, in
// ascending order. The fixture names are lower-case ASCII letters and
// digits, so byte order matches the database collation.
func compareKey(s library.TrackSort, a, b sortRow) int {
	switch s {
	case library.SortArtist:
		return strings.Compare(*a.artist, *b.artist)
	case library.SortType:
		return strings.Compare(a.typeName, b.typeName)
	case library.SortDuration:
		return cmp.Compare(*a.duration, *b.duration)
	case library.SortFormat:
		return strings.Compare(*a.format, *b.format)
	case library.SortQuality:
		// No issues first, then more issues first.
		return cmp.Or(cmp.Compare(min(a.issues, 1), min(b.issues, 1)), cmp.Compare(b.issues, a.issues))
	case library.SortReviewed:
		return cmp.Compare(*a.reviewed, *b.reviewed)
	default:
		return strings.Compare(a.title, b.title)
	}
}

// expectedOrder sorts rows as decisions 2–4 of docs/phase-20-plan.md say:
// missing values last in both directions, ties by title and ID ascending.
func expectedOrder(rows []sortRow, s library.TrackSort, desc bool) []uint64 {
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a, b sortRow) int {
		ma, mb := a.missing(s), b.missing(s)
		if ma != mb {
			if ma {
				return 1
			}
			return -1
		}
		if !ma {
			c := compareKey(s, a, b)
			if desc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		if s != library.SortTitle {
			if c := strings.Compare(a.title, b.title); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.id, b.id)
	})
	ids := make([]uint64, len(sorted))
	for i, r := range sorted {
		ids[i] = r.id
	}
	return ids
}

// sortFixture adds 30 tracks with equal titles, missing artists, durations,
// formats, and reviewed marks, equal reviewed times, and all three quality
// states.
func sortFixture(t *testing.T, f *fixture) []sortRow {
	t.Helper()
	ctx := t.Context()
	var jingle uint64
	var musicName, jingleName string
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT id, name FROM playlist_types WHERE code = 'jingle'`).Scan(&jingle, &jingleName))
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT name FROM playlist_types WHERE id = $1`, f.music).Scan(&musicName))
	artists := map[string]uint64{}
	for _, name := range []string{"ccc", "aaa", "bbb"} {
		artists[name] = f.id(`INSERT INTO artists (name) VALUES ($1) RETURNING id`, name)
	}
	var codes []string
	for _, c := range library.QualityIssues() {
		codes = append(codes, string(c))
	}

	rows := make([]sortRow, 30)
	for i := range rows {
		r := sortRow{title: fmt.Sprintf("t%02d", (i*7)%30), typeName: musicName, analysed: i%3 != 0}
		if i%5 == 0 {
			r.title = "same"
		}
		in := library.TrackInput{Title: r.title, FilePath: fmt.Sprintf("music/%02d.mp3", i), TypeID: f.music}
		if i%2 == 1 {
			in.TypeID, r.typeName = jingle, jingleName
		}
		if i%4 != 0 {
			name := []string{"ccc", "aaa", "bbb"}[i%3]
			r.artist, in.ArtistID = &name, new(artists[name])
		}
		if i%6 != 0 {
			r.duration = new(1000 * ((i*11)%17 + 1))
			in.DurationMS = r.duration
		}
		switch i % 7 {
		case 0:
		case 1:
			r.format = new("")
		default:
			r.format = new([]string{"mp3", "flac", "ogg"}[i%3])
		}
		if r.analysed && i%3 == 2 {
			r.issues = i%4 + 1
		}
		if i%4 != 1 {
			r.reviewed = new(i % 5)
		}
		id, err := f.tracks.Create(ctx, in)
		require.NoError(t, err)
		r.id = id
		var analysedAt any
		if r.analysed {
			analysedAt = "2026-10-01T00:00:00Z"
		}
		var reviewedAt any
		if r.reviewed != nil {
			reviewedAt = time.Date(2026, 10, 1, *r.reviewed, 0, 0, 0, time.UTC)
		}
		_, err = f.d.Pool.Exec(ctx, `UPDATE tracks SET format = $2, analyzed_at = $3, quality_issues = $4, reviewed_at = $5
WHERE id = $1`, id, r.format, analysedAt, codes[:r.issues], reviewedAt)
		require.NoError(t, err)
		rows[i] = r
	}
	return rows
}

// readPages reads every page of f by offset and checks total on each.
func readPages(ctx context.Context, t *testing.T, fx *fixture, f library.TrackFilter, total int) []uint64 {
	t.Helper()
	var ids []uint64
	for f.Offset = 0; ; f.Offset += f.Limit {
		page, n, err := fx.tracks.List(ctx, f)
		require.NoError(t, err)
		require.Equal(t, total, n, "total at offset %d", f.Offset)
		for _, tr := range page {
			ids = append(ids, tr.ID)
		}
		if len(page) < f.Limit {
			return ids
		}
	}
}

func TestTrackSortAndPages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rows := sortFixture(t, f)

	for _, s := range library.TrackSorts() {
		for _, desc := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s desc=%v", s, desc), func(t *testing.T) {
				want := expectedOrder(rows, s, desc)
				got := readPages(t.Context(), t, f, library.TrackFilter{Sort: s, Desc: desc, Limit: 7}, len(rows))
				require.Equal(t, want, got, "every track exactly once, in order")
			})
		}
	}

	byID := map[uint64]sortRow{}
	for _, r := range rows {
		byID[r.id] = r
	}
	// The quality order in plain words, not through the comparator.
	got := readPages(t.Context(), t, f, library.TrackFilter{Sort: library.SortQuality, Limit: 100}, len(rows))
	var states []string
	prev := 0
	for _, id := range got {
		r := byID[id]
		state := "unanalysed"
		switch {
		case r.analysed && r.issues == 0:
			state = "ok"
		case r.analysed:
			state = "issues"
			if len(states) > 0 && states[len(states)-1] == "issues" {
				require.LessOrEqual(t, r.issues, prev, "more issues first")
			}
			prev = r.issues
		}
		if len(states) == 0 || states[len(states)-1] != state {
			states = append(states, state)
		}
	}
	require.Equal(t, []string{"ok", "issues", "unanalysed"}, states)
	for _, desc := range []bool{false, true} {
		got := readPages(t.Context(), t, f, library.TrackFilter{Sort: library.SortDuration, Desc: desc, Limit: 100}, len(rows))
		require.Nil(t, byID[got[len(got)-1]].duration, "missing durations last, desc=%v", desc)
	}
}

func TestTrackSortWithFilters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rows := sortFixture(t, f)
	ctx := t.Context()
	pop := f.playlist("Pop")
	var inPop []uint64
	for i, r := range rows {
		if i%2 == 0 {
			inPop = append(inPop, r.id)
		}
	}
	for _, id := range inPop {
		_, err := f.d.Pool.Exec(ctx, `INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2)`, id, pop)
		require.NoError(t, err)
	}

	keep := func(match func(sortRow) bool) []sortRow {
		var out []sortRow
		for _, r := range rows {
			if match(r) {
				out = append(out, r)
			}
		}
		return out
	}
	tests := []struct {
		name   string
		filter library.TrackFilter
		rows   []sortRow
	}{
		{
			"issues by duration desc",
			library.TrackFilter{Quality: library.QualityIssuesFound, Sort: library.SortDuration, Desc: true},
			keep(func(r sortRow) bool { return r.issues > 0 }),
		},
		{
			"playlist by artist",
			library.TrackFilter{PlaylistID: pop, Sort: library.SortArtist},
			keep(func(r sortRow) bool { return slices.Contains(inPop, r.id) }),
		},
		{
			"query by format desc",
			library.TrackFilter{Query: "AA", Sort: library.SortFormat, Desc: true},
			keep(func(r sortRow) bool { return r.artist != nil && *r.artist == "aaa" }),
		},
		{
			"type by quality",
			library.TrackFilter{TypeID: f.music, Sort: library.SortQuality},
			keep(func(r sortRow) bool { return r.typeName == rows[0].typeName }),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			require.NotEmpty(t, tt.rows)
			tt.filter.Limit = 3
			want := expectedOrder(tt.rows, tt.filter.Sort, tt.filter.Desc)
			require.Equal(t, want, readPages(ctx, t, f, tt.filter, len(tt.rows)))

			tt.filter.Offset = 1000
			page, total, err := f.tracks.List(ctx, tt.filter)
			require.NoError(t, err)
			require.Empty(t, page, "an offset past the end is not an error")
			require.Equal(t, len(tt.rows), total)
		})
	}
}

// The cursor in the default order reads the same pages as the offset, and
// the total counts the tracks before the cursor too.
func TestTrackCursorDefaultOrder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rows := sortFixture(t, f)
	ctx := t.Context()

	var got []uint64
	filter := library.TrackFilter{Limit: 7}
	for {
		page, total, err := f.tracks.List(ctx, filter)
		require.NoError(t, err)
		require.Equal(t, len(rows), total)
		for _, tr := range page {
			got = append(got, tr.ID)
		}
		if len(page) < filter.Limit {
			break
		}
		last := page[len(page)-1]
		filter.After = &library.TrackCursor{Title: last.Title, ID: last.ID}
	}
	require.Equal(t, expectedOrder(rows, library.SortTitle, false), got)
}
