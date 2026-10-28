package handlers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// inUseLibrary implements the track operations of Library; the embedded nil
// interface panics on any other call, so a test fails loudly if an operation
// reaches a service method it should not.
type inUseLibrary struct {
	Library
	inUse      bool
	inUseCalls int
}

func (f *inUseLibrary) GetTrack(context.Context, uint64) (library.Track, error) {
	return library.Track{ID: 7, Title: "Song", FilePath: "music/song.mp3", Type: library.TypeRef{ID: 1, Code: "music"}}, nil
}

func (f *inUseLibrary) TrackInUse(context.Context, uint64) (bool, error) {
	f.inUseCalls++
	return f.inUse, nil
}

func (f *inUseLibrary) ListTracks(context.Context, library.TrackFilter) (library.TrackPage, error) {
	return library.TrackPage{Items: []library.Track{{ID: 7, Title: "Song", Type: library.TypeRef{ID: 1}}}, Total: 1}, nil
}

// The track detail maps the in-use check to in_use, true and false alike.
func TestTrackDetailMapsInUse(t *testing.T) {
	t.Parallel()
	for _, inUse := range []bool{true, false} {
		_, api := humatest.New(t)
		lib := &inUseLibrary{inUse: inUse}
		registerTracks(api, slog.New(slog.DiscardHandler), lib, nil, nil, nil)

		resp := api.Get("/api/tracks/7")
		require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
		var body map[string]any
		require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
		require.Equal(t, inUse, body["in_use"])
		require.Equal(t, 1, lib.inUseCalls)
	}
}

// A list leaves in_use null for every item and runs no in-use lookup.
func TestTrackListLeavesInUseNull(t *testing.T) {
	t.Parallel()
	_, api := humatest.New(t)
	lib := &inUseLibrary{inUse: true}
	registerTracks(api, slog.New(slog.DiscardHandler), lib, nil, nil, nil)

	resp := api.Get("/api/tracks")
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var body struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	v, ok := body.Items[0]["in_use"]
	require.True(t, ok, "in_use is present, as null")
	require.Nil(t, v)
	require.Zero(t, lib.inUseCalls)
}
