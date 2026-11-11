//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
)

// GET /api/artists pages and sorts (docs/artists-paging-plan.md).
func TestArtistsListPage(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")

	// 105 artists without tracks, more than the default page.
	_, err := a.db.Pool.Exec(t.Context(),
		`INSERT INTO artists (name) SELECT format('Artist %s', lpad(n::text, 3, '0')) FROM generate_series(1, 105) n`)
	require.NoError(t, err)
	withTracks := map[string]int{"Zed": 3, "Yan": 2, "Xi": 1}
	for n, count := range withTracks {
		var ar dto.Artist
		a.ok(http.StatusCreated, http.MethodPost, "/api/artists", map[string]any{"name": n}, &ar)
		for i := range count {
			a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
				"title": fmt.Sprintf("%s %d", n, i), "artist_id": ar.ID, "file_path": fmt.Sprintf("music/%s-%d.mp3", n, i),
				"type_id": music, "duration_ms": 200000, "playlist_ids": []uint64{},
			}, nil)
		}
	}

	var page dto.ArtistList
	a.ok(http.StatusOK, http.MethodGet, "/api/artists", nil, &page)
	require.Len(t, page.Items, 100)
	require.Equal(t, 108, page.Total)
	names := make([]string, 0, len(page.Items))
	for _, ar := range page.Items {
		names = append(names, ar.Name)
	}
	require.True(t, slices.IsSorted(names), "name order")
	require.Equal(t, "Artist 001", names[0])

	a.ok(http.StatusOK, http.MethodGet, "/api/artists?sort=track_count&order=desc&limit=2", nil, &page)
	require.Equal(t, []dto.Artist{{ID: page.Items[0].ID, Name: "Zed", TrackCount: 3}, {ID: page.Items[1].ID, Name: "Yan", TrackCount: 2}}, page.Items)
	require.Equal(t, 108, page.Total)

	a.ok(http.StatusOK, http.MethodGet, "/api/artists?q=artist%2010&offset=5", nil, &page)
	require.Len(t, page.Items, 1, "Artist 100 to 105, skipping 5")
	require.Equal(t, "Artist 105", page.Items[0].Name)
	require.Equal(t, 6, page.Total)

	a.ok(http.StatusOK, http.MethodGet, "/api/artists?offset=1000", nil, &page)
	require.Empty(t, page.Items)
	require.Equal(t, 108, page.Total)

	a.fieldError("query.sort", errcodes.Enum, http.MethodGet, "/api/artists?sort=name_desc", nil)
	a.fieldError("query.order", errcodes.Enum, http.MethodGet, "/api/artists?order=up", nil)
	a.fieldError("query.limit", errcodes.Range, http.MethodGet, "/api/artists?limit=501", nil)
	a.fieldError("query.offset", errcodes.Range, http.MethodGet, "/api/artists?offset=1000001", nil)
	a.fieldError("query.q", errcodes.Length, http.MethodGet, "/api/artists?q="+strings.Repeat("a", 201), nil)
}
