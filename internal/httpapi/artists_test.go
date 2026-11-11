package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/handlers"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

// artistsLibrary answers ListArtists and records the filter; the embedded
// nil interface panics on any other call.
type artistsLibrary struct {
	handlers.Library
	got   library.ArtistFilter
	calls int
}

func (f *artistsLibrary) ListArtists(_ context.Context, filter library.ArtistFilter) (library.ArtistPage, error) {
	f.got = filter
	f.calls++
	return library.ArtistPage{Items: []library.Artist{{ID: 7, Name: "ABBA", TrackCount: 3}}, Total: 42}, nil
}

func newArtistsRouter(t *testing.T, lib *artistsLibrary) http.Handler {
	t.Helper()
	log := slog.New(logging.NewHandler(&syncBuffer{}, slog.LevelDebug))
	return NewRouter(Deps{Log: log, DB: fakePinger{}, Library: lib, Now: func() time.Time { return testNow }})
}

func TestListArtistsPassesTheQuery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, query string
		want        library.ArtistFilter
	}{
		{"defaults", "", library.ArtistFilter{Sort: library.ArtistSortName, Limit: 100}},
		{
			"every parameter", "?q=ab&sort=track_count&order=desc&offset=1000000&limit=500",
			library.ArtistFilter{Query: "ab", Sort: library.ArtistSortTrackCount, Desc: true, Offset: 1_000_000, Limit: 500},
		},
		{"smallest page", "?order=asc&offset=0&limit=1", library.ArtistFilter{Sort: library.ArtistSortName, Limit: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lib := &artistsLibrary{}
			rec := do(t, newArtistsRouter(t, lib), http.MethodGet, "/api/artists"+tt.query)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, tt.want, lib.got)
			require.JSONEq(t, `{"items":[{"id":7,"name":"ABBA","track_count":3}],"total":42}`, rec.Body.String())
		})
	}
}

func TestListArtistsRefusesBadParametersWithQueryNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, query, field string
		code               errcodes.Field
	}{
		{"unknown sort", "?sort=created_at", "query.sort", errcodes.Enum},
		{"unknown order", "?order=up", "query.order", errcodes.Enum},
		{"limit 0", "?limit=0", "query.limit", errcodes.Range},
		{"limit 501", "?limit=501", "query.limit", errcodes.Range},
		{"negative offset", "?offset=-1", "query.offset", errcodes.Range},
		{"offset over the maximum", "?offset=1000001", "query.offset", errcodes.Range},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lib := &artistsLibrary{}
			rec := do(t, newArtistsRouter(t, lib), http.MethodGet, "/api/artists"+tt.query)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			p := decodeProblem(t, rec)
			require.Equal(t, errcodes.ValidationFailed, p.Code)
			require.Len(t, p.Errors, 1, rec.Body.String())
			require.Equal(t, tt.field, p.Errors[0].Field)
			require.Equal(t, tt.code, p.Errors[0].Code)
			require.Zero(t, lib.calls)
		})
	}
}
