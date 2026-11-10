package dto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// The playlist of a planned item is the object {id, name, type}, or null.
func TestTrackPlanItemPlaylistJSON(t *testing.T) {
	t.Parallel()
	fill := TrackPlanItem{ID: 1, Source: "fill", Playlist: TrackPlanPlaylistField{V: &TrackPlanPlaylist{ID: 7, Name: "Music", Type: "music"}}}
	b, err := json.Marshal(fill)
	require.NoError(t, err)
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &got))
	require.JSONEq(t, `{"id":7,"name":"Music","type":"music"}`, string(got["playlist"]))

	general := TrackPlanItem{ID: 2, Source: "general"}
	b, err = json.Marshal(general)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &got))
	require.Equal(t, "null", string(got["playlist"]), "a general item has no playlist: null, not absent")

	var back TrackPlanItem
	require.NoError(t, json.Unmarshal([]byte(`{"id":3,"source":"fill","playlist":{"id":7,"name":"Music","type":"music"}}`), &back))
	require.NotNil(t, back.Playlist.V)
	require.Equal(t, TrackPlanPlaylist{ID: 7, Name: "Music", Type: "music"}, *back.Playlist.V)
	require.NoError(t, json.Unmarshal([]byte(`{"id":4,"playlist":null}`), &back))
	require.Nil(t, back.Playlist.V)
}
