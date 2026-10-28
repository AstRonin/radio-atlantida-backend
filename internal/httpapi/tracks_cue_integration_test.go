//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
)

func cueBody(in int, out any, fadeIn, fadeOut int) map[string]any {
	return map[string]any{"cue_in_ms": in, "cue_out_ms": out, "fade_in_ms": fadeIn, "fade_out_ms": fadeOut}
}

// The cue of a planned track rebuilds its hours with the new cut; queued
// items keep theirs, and the usage marks follow the plan
// (docs/phase-22-plan.md §3.3, §3.6).
func TestTrackCueRebuildsPlannedHours(t *testing.T) {
	f := newClockFixture(t)
	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", f.musicClock("Day"), &saved)
	f.waitOperation(saved.BuildOperationID)
	date, weekday, _ := tomorrowNoon(t)
	var assigned dto.GridAssignResult
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": saved.Clock.ID, "weekdays": []int{weekday}, "hours": []int{12}, "valid_from": date, "valid_to": date,
	}, &assigned)
	f.waitOperation(assigned.BuildOperationID)
	items := f.plan().Items[0].Items
	require.Len(t, items, 4)
	planned := items[0].TrackID

	// Usage marks: the four songs are planned, all from a playlist.
	var page dto.TrackPage
	f.ok(http.StatusOK, http.MethodGet, "/api/tracks?in_plan=true&sort=next_planned_at", nil, &page)
	require.Equal(t, 4, page.Total)
	require.Equal(t, planned, page.Items[0].ID, "the earliest planned first")
	require.Equal(t, items[0].PlannedStartUTC, *page.Items[0].NextPlannedAt)
	f.ok(http.StatusOK, http.MethodGet, "/api/tracks?in_playlist=false&in_plan=true", nil, &page)
	require.Zero(t, page.Total)
	f.ok(http.StatusOK, http.MethodGet, "/api/tracks?in_plan=false", nil, &page)
	require.Equal(t, 1, page.Total, "the jingle")

	// A 20 s cut at the start: 18:40 on air.
	f.exec(`UPDATE tracks SET reviewed_at = now() WHERE id = $1 RETURNING id`, planned)
	var res dto.TrackCueResult
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/tracks/%d/cue", planned), cueBody(20000, nil, 1000, 0), &res)
	require.Equal(t, 20000, res.Track.CueInMS)
	require.Nil(t, res.Track.CueOutMS)
	require.Equal(t, 1120000, *res.Track.AirDurationMS)
	require.NotNil(t, res.Track.ReviewedAt, "the cue edit keeps the reviewed mark")
	op := f.waitOperation(res.BuildOperationID)
	require.Contains(t, op.ResultSummary, "built 1 hours")
	for _, it := range f.plan().Items[0].Items {
		if it.TrackID == planned {
			require.Equal(t, 20000, it.PlannedInMS, "planned with the new cut")
			if !it.Truncated {
				require.Equal(t, 1140000, it.PlannedOutMS)
			}
		}
	}

	// Fades only: no rebuild.
	res = dto.TrackCueResult{}
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/tracks/%d/cue", planned), cueBody(20000, nil, 2000, 3000), &res)
	require.Nil(t, res.BuildOperationID)
	require.Equal(t, 3000, res.Track.FadeOutMS)

	// Queued: refused, and the item keeps its cut.
	item := f.plan().Items[0].Items[1]
	f.exec(`UPDATE scheduled_items SET status = 'queued' WHERE id = $1 RETURNING id`, item.ID)
	f.problem(http.StatusConflict, errcodes.TrackOnAir, http.MethodPut, fmt.Sprintf("/api/tracks/%d/cue", item.TrackID), cueBody(1000, nil, 0, 0))
	var tr dto.Track
	f.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/tracks/%d", item.TrackID), nil, &tr)
	require.Equal(t, "queued", tr.PlayoutState)
	require.Zero(t, tr.CueInMS)
}

func TestTrackCueRefusals(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")
	id := a.exec(`INSERT INTO tracks (title, file_path, type_id, duration_ms) VALUES ('Live', 'live.mp3', $1, 60000) RETURNING id`, music)
	unknown := a.exec(`INSERT INTO tracks (title, file_path, type_id) VALUES ('New', 'new.mp3', $1) RETURNING id`, music)
	path := fmt.Sprintf("/api/tracks/%d/cue", id)

	a.problem(http.StatusNotFound, errcodes.TrackNotFound, http.MethodPut, "/api/tracks/999999/cue", cueBody(0, nil, 0, 0))
	a.problem(http.StatusConflict, errcodes.TrackNotAnalysed, http.MethodPut, fmt.Sprintf("/api/tracks/%d/cue", unknown), cueBody(0, nil, 0, 0))
	a.fieldError("cue_out_ms", errcodes.CueBeyondEnd, http.MethodPut, path, cueBody(0, 60001, 0, 0))
	a.fieldError("cue_out_ms", errcodes.CueOrder, http.MethodPut, path, cueBody(10000, 10000, 0, 0))
	a.fieldError("cue_in_ms", errcodes.CueBeyondEnd, http.MethodPut, path, cueBody(60000, nil, 0, 0))
	a.fieldError("cue_out_ms", errcodes.AirTooShort, http.MethodPut, path, cueBody(10000, 14000, 0, 0))
	a.fieldError("fade_out_ms", errcodes.FadesTooLong, http.MethodPut, path, cueBody(10000, 18000, 4000, 5000))
	a.fieldError("fade_in_ms", errcodes.Range, http.MethodPut, path, cueBody(0, nil, 5001, 0))
	a.fieldError("cue_out_ms", errcodes.Required, http.MethodPut, path,
		map[string]any{"cue_in_ms": 0, "fade_in_ms": 0, "fade_out_ms": 0})

	var res dto.TrackCueResult
	a.ok(http.StatusOK, http.MethodPut, path, cueBody(10000, 15000, 0, 0), &res)
	require.Equal(t, 5000, *res.Track.AirDurationMS, "exactly 5 s")
	require.Nil(t, res.BuildOperationID, "not planned")
	// PUT /api/tracks/{id} keeps the cue.
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/tracks/%d", id), map[string]any{
		"title": "Live", "type_id": music, "playlist_ids": []uint64{},
	}, &res.Track)
	require.Equal(t, 10000, res.Track.CueInMS)
	require.Equal(t, 15000, *res.Track.CueOutMS)
}

func TestTrackWaveform(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")
	id := a.exec(`INSERT INTO tracks (title, file_path, type_id, file_checksum) VALUES ('Tone', 'tone.mp3', $1, 'sum-1') RETURNING id`, music)
	gone := a.exec(`INSERT INTO tracks (title, file_path, type_id, file_checksum) VALUES ('Gone', 'gone.mp3', $1, 'g') RETURNING id`, music)
	path := fmt.Sprintf("/api/tracks/%d/waveform", id)
	if !a.tools {
		a.problem(http.StatusServiceUnavailable, errcodes.MediaToolsUnavailable, http.MethodGet, path, nil)
		return
	}
	//nolint:gosec // G204: fixed arguments and a path in a temp dir
	gen := exec.CommandContext(t.Context(), "ffmpeg", "-hide_banner", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "aevalsrc=0.5*sin(2*PI*440*t):s=44100:d=2", "-c:a", "libmp3lame", filepath.Join(a.mediaRoot, "tone.mp3"))
	out, err := gen.CombinedOutput()
	require.NoError(t, err, string(out))

	var w dto.TrackWaveform
	res := a.call(http.MethodGet, path, nil, &w)
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	require.Equal(t, `"sum-1"`, res.header.Get("ETag"))
	require.Equal(t, "no-cache", res.header.Get("Cache-Control"))
	require.Equal(t, 50, w.BucketMS)
	require.InDelta(t, 2000, w.DurationMS, 100)
	require.InDelta(t, 80, len(w.Peaks), 4)
	require.InDelta(t, 63, w.MaxPeak, 6, "a half-scale tone")

	// The client holds it: 304, from the stored row even with the file gone.
	require.NoError(t, os.Remove(filepath.Join(a.mediaRoot, "tone.mp3")))
	res = a.call(http.MethodGet, path, nil, nil, "If-None-Match", `"sum-1"`)
	require.Equal(t, http.StatusNotModified, res.status)
	require.Empty(t, res.body)
	res = a.call(http.MethodGet, path, nil, &w)
	require.Equal(t, http.StatusOK, res.status, "stored")

	a.problem(http.StatusConflict, errcodes.WaveformUnavailable, http.MethodGet, fmt.Sprintf("/api/tracks/%d/waveform", gone), nil)
	a.problem(http.StatusNotFound, errcodes.TrackNotFound, http.MethodGet, "/api/tracks/999999/waveform", nil)
}
