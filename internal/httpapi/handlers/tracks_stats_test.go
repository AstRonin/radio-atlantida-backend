package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// statsLibrary answers TrackStats and records the filter; the embedded nil
// interface panics on any other call, so /api/tracks/stats reaching the
// operation of /api/tracks/{id} fails the test.
type statsLibrary struct {
	Library
	got   library.TrackFilter
	calls int
	err   error
}

func (f *statsLibrary) TrackStats(_ context.Context, filter library.TrackFilter) (library.TrackStats, error) {
	f.got = filter
	f.calls++
	return library.TrackStats{Total: 5, Reviewed: 4, WithIssues: 3, Unanalysed: 2, AirDurationMS: 5_000_000_000}, f.err
}

func TestTrackStatsFilters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		query string
		want  library.TrackFilter
	}{
		{"no filter", "", library.TrackFilter{}},
		{
			"every filter",
			"?type_id=1&artist_id=2&playlist_id=3&q=abc&quality=issues&reviewed=true&in_plan=false&in_playlist=true",
			library.TrackFilter{
				TypeID: 1, ArtistID: 2, PlaylistID: 3, Query: "abc", Quality: library.QualityIssuesFound,
				Reviewed: new(true), InPlan: new(false), InPlaylist: new(true),
			},
		},
		{
			"the sort and the page take no part",
			"?quality=ok&sort=nonsense&order=sideways&offset=-1&cursor=x&limit=0",
			library.TrackFilter{Quality: library.QualityOK},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, api := humatest.New(t)
			lib := &statsLibrary{}
			registerTracks(api, slog.New(slog.DiscardHandler), lib, nil, nil, nil)

			resp := api.Get("/api/tracks/stats" + tt.query)
			require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
			require.Equal(t, 1, lib.calls)
			require.Equal(t, tt.want, lib.got)
			var body map[string]any
			require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
			require.Equal(t, map[string]any{
				"total": 5.0, "reviewed": 4.0, "with_issues": 3.0, "unanalysed": 2.0, "air_duration_ms": 5e9,
			}, body)
		})
	}
}

func TestTrackStatsRefusesABadFilter(t *testing.T) {
	t.Parallel()
	_, api := humatest.New(t)
	lib := &statsLibrary{}
	registerTracks(api, slog.New(slog.DiscardHandler), lib, nil, nil, nil)

	resp := api.Get("/api/tracks/stats?quality=nonsense")
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	require.Zero(t, lib.calls)
}

func TestTrackStatsMapsServiceError(t *testing.T) {
	t.Parallel()
	_, api := humatest.New(t)
	lib := &statsLibrary{err: errors.New("database gone")}
	registerTracks(api, slog.New(slog.DiscardHandler), lib, nil, nil, nil)

	resp := api.Get("/api/tracks/stats")
	require.Equal(t, http.StatusInternalServerError, resp.Code, resp.Body.String())
	require.NotContains(t, resp.Body.String(), "database gone", "the cause stays in the log")
}
