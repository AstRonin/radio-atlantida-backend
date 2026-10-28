//go:build integration

package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
)

// The track detail says whether replace-file would refuse the track; the
// lists leave in_use null, and so do the responses that are not the detail.
func TestTrackInUseOnDetail(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")
	create := func(title, path string) dto.Track {
		t.Helper()
		var tr dto.Track
		a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
			"title": title, "file_path": path, "type_id": music, "duration_ms": 180000, "playlist_ids": []uint64{},
		}, &tr)
		require.Nil(t, tr.InUse, "a create response leaves in_use null")
		return tr
	}
	detail := func(id uint64) dto.Track {
		t.Helper()
		var tr dto.Track
		a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/tracks/%d", id), nil, &tr)
		require.NotNil(t, tr.InUse, "the detail sets in_use")
		return tr
	}

	used := create("Used", "music/used.mp3")
	unused := create("Unused", "music/unused.mp3")
	aired := create("Aired", "music/aired.mp3")

	// Unused: false on the detail.
	require.False(t, *detail(unused.ID).InUse)

	// A clock insert uses the track: true.
	clock := a.exec(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	insert := a.exec(`INSERT INTO clock_inserts (clock_id, offset_ms, track_id) VALUES ($1, 1800000, $2) RETURNING id`, clock, used.ID)
	require.True(t, *detail(used.ID).InUse)
	a.exec(`DELETE FROM clock_inserts WHERE id = $1 RETURNING id`, insert)
	require.False(t, *detail(used.ID).InUse)

	// A plan item that has not aired: true.
	hour := time.Now().UTC().Truncate(time.Hour).Add(3 * time.Hour)
	a.exec(`INSERT INTO plan_hours (hour_start_utc, clock_id, clock_updated_at, seed, built_at) VALUES ($1, $2, now(), 1, now()) RETURNING clock_id`, hour, clock)
	item := a.exec(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status)
VALUES ($1, $2, $3, 180000, 'fill', 'planned') RETURNING id`, clock, used.ID, hour.Add(10*time.Minute))
	require.True(t, *detail(used.ID).InUse)
	a.exec(`DELETE FROM scheduled_items WHERE id = $1 RETURNING id`, item)

	// A plan item that has aired does not count: false.
	airedItem := a.exec(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status)
VALUES ($1, $2, $3, 180000, 'fill', 'aired') RETURNING id`, clock, aired.ID, hour.Add(-2*time.Hour))
	require.False(t, *detail(aired.ID).InUse)
	a.exec(`DELETE FROM scheduled_items WHERE id = $1 RETURNING id`, airedItem)

	// An open missed item (in the play log, not dismissed or rescheduled): true.
	outage := a.exec(`INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component)
VALUES ($1, $2, 'icecast', 'icecast') RETURNING id`, hour.Add(-4*time.Hour), hour.Add(-3*time.Hour))
	missedItem := a.exec(`INSERT INTO play_log (track_id, status, planned_start_utc, planned_in_ms, planned_out_ms, truncated, source, outage_id, resolution)
VALUES ($1, 'missed', $2, 0, 180000, false, 'auto', $3, 'open') RETURNING id`, used.ID, hour.Add(-3*time.Hour+10*time.Minute), outage)
	require.True(t, *detail(used.ID).InUse)
	a.exec(`DELETE FROM play_log WHERE id = $1 RETURNING id`, missedItem)
	require.False(t, *detail(used.ID).InUse)

	// PUT answers with the track without the detail flag: null.
	var put dto.Track
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/tracks/%d", unused.ID),
		map[string]any{"title": "Unused", "type_id": music, "playlist_ids": []uint64{}}, &put)
	require.Nil(t, put.InUse)
}

// A list returns in_use as null for every item, even for a track in use,
// and the field is present in the JSON.
func TestTrackListInUseIsNull(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")
	var tr dto.Track
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Listed", "file_path": "music/listed.mp3", "type_id": music, "duration_ms": 180000, "playlist_ids": []uint64{},
	}, &tr)
	clock := a.exec(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	a.exec(`INSERT INTO clock_inserts (clock_id, offset_ms, track_id) VALUES ($1, 1800000, $2) RETURNING id`, clock, tr.ID)

	res := a.call(http.MethodGet, "/api/tracks", nil, nil)
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	require.NoError(t, json.Unmarshal(res.body, &page))
	require.NotEmpty(t, page.Items)
	for _, item := range page.Items {
		raw, ok := item["in_use"]
		require.True(t, ok, "in_use is present in every list item")
		require.Equal(t, "null", string(raw))
	}
}
