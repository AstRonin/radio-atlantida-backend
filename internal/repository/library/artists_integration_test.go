//go:build integration

package library_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// The artist list: both sorts in both orders, ties on the track count, the
// query with paging, and the total (docs/artists-paging-plan.md). Names are
// unique (uq_artists_name), so only the track count can tie; the name and
// ID tie-break then decides.
func TestArtistsListSortAndPage(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	counts := map[string]int{"Charlie": 2, "Alpha": 2, "Echo": 0, "Delta": 3, "Bravo": 0}
	for _, n := range []string{"Charlie", "Alpha", "Echo", "Delta", "Bravo"} {
		id, err := f.artists.Create(ctx, n)
		require.NoError(t, err)
		for i := range counts[n] {
			f.track(n+string(rune('a'+i)), &id)
		}
	}
	f.track("No artist", nil)

	names := func(as []library.Artist) []string {
		out := []string{}
		for _, a := range as {
			out = append(out, a.Name)
		}
		return out
	}

	tests := []struct {
		name      string
		filter    library.ArtistFilter
		want      []string
		wantTotal int
	}{
		{"default: name asc", library.ArtistFilter{Limit: 100}, []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"}, 5},
		{"name desc", library.ArtistFilter{Sort: library.ArtistSortName, Desc: true, Limit: 100}, []string{"Echo", "Delta", "Charlie", "Bravo", "Alpha"}, 5},
		{
			"track_count asc, ties by name",
			library.ArtistFilter{Sort: library.ArtistSortTrackCount, Limit: 100},
			[]string{"Bravo", "Echo", "Alpha", "Charlie", "Delta"},
			5,
		},
		{
			"track_count desc, ties still by name asc",
			library.ArtistFilter{Sort: library.ArtistSortTrackCount, Desc: true, Limit: 100},
			[]string{"Delta", "Alpha", "Charlie", "Bravo", "Echo"},
			5,
		},
		{"page of the default order", library.ArtistFilter{Offset: 1, Limit: 2}, []string{"Bravo", "Charlie"}, 5},
		{
			"page across a tie",
			library.ArtistFilter{Sort: library.ArtistSortTrackCount, Desc: true, Offset: 2, Limit: 2},
			[]string{"Charlie", "Bravo"},
			5,
		},
		{"query, case-insensitive", library.ArtistFilter{Query: "A", Limit: 100}, []string{"Alpha", "Bravo", "Charlie", "Delta"}, 4},
		{"query with a page", library.ArtistFilter{Query: "a", Offset: 1, Limit: 2}, []string{"Bravo", "Charlie"}, 4},
		{"offset past the end", library.ArtistFilter{Offset: 10, Limit: 100}, []string{}, 5},
		{"query matching nothing", library.ArtistFilter{Query: "zz", Limit: 100}, []string{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			as, total, err := f.artists.List(ctx, tt.filter)
			require.NoError(t, err)
			require.Equal(t, tt.want, names(as))
			require.Equal(t, tt.wantTotal, total)
		})
	}

	as, _, err := f.artists.List(ctx, library.ArtistFilter{Sort: library.ArtistSortTrackCount, Desc: true, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, 3, as[0].TrackCount)

	_, _, err = f.artists.List(ctx, library.ArtistFilter{Sort: "created_at", Limit: 1})
	require.Error(t, err, "an unknown sort key never reaches SQL")
}
