//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// PUT refuses the fields of the file: they change only through replace-file,
// and an update without them keeps the stored analysis.
func TestUpdateTrackKeepsTheFile(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")
	var tr dto.Track
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Song", "file_path": "music/song.mp3", "type_id": music, "duration_ms": 200000,
		"format": "mp3", "bitrate": 320000, "sample_rate": 44100, "playlist_ids": []uint64{},
	}, &tr)
	path := fmt.Sprintf("/api/tracks/%d", tr.ID)

	for field, value := range map[string]any{
		"file_path": "music/other.mp3", "duration_ms": 1000, "format": "aac", "bitrate": 128000, "sample_rate": 48000,
	} {
		body := map[string]any{"title": "Song", "type_id": music, "playlist_ids": []uint64{}, field: value}
		p := a.problem(http.StatusUnprocessableEntity, errcodes.ValidationFailed, http.MethodPut, path, body)
		require.Len(t, p.Errors, 1, "%s: %+v", field, p.Errors)
		require.Equal(t, errcodes.UnknownField, p.Errors[0].Code, "%s: %+v", field, p.Errors)
	}

	a.ok(http.StatusOK, http.MethodPut, path, map[string]any{"title": "Renamed", "type_id": music, "playlist_ids": []uint64{}}, &tr)
	require.Equal(t, "Renamed", tr.Title)
	require.Equal(t, "music/song.mp3", tr.FilePath)
	require.Equal(t, 200000, *tr.DurationMS)
	require.Equal(t, "mp3", *tr.Format)
	require.Equal(t, 320000, *tr.Bitrate)
	require.Equal(t, 44100, *tr.SampleRate)
}

// The file of a track is replaced by one action: the new file is analysed,
// the reviewed mark is cleared, the old file stays, and the refusals name
// what uses the track.
func TestReplaceTrackFile(t *testing.T) {
	a := newLibraryAPI(t)
	if !a.tools {
		t.Skip("ffmpeg not installed")
	}
	music := a.typeID("music")
	a.tone("music/a.mp3", "-ac", "2", "-ar", "44100", "-b:a", "320k", "-metadata", "comment=a")
	a.tone("music/b.mp3", "-ac", "1", "-ar", "22050", "-b:a", "64k", "-metadata", "comment=b")
	a.tone("music/c.mp3", "-ac", "2", "-ar", "44100", "-b:a", "320k", "-metadata", "comment=c")

	var tr, other dto.Track
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Song", "file_path": "music/a.mp3", "type_id": music, "playlist_ids": []uint64{},
	}, &tr)
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Other", "file_path": "music/c.mp3", "type_id": music, "playlist_ids": []uint64{},
	}, &other)
	require.NotNil(t, tr.DurationMS, "read from the file on create")
	// The track is analysed: its checksum is what the waveform is checked against.
	var accepted dto.OperationAccepted
	a.ok(http.StatusAccepted, http.MethodPost, "/api/tracks/analyse", map[string]any{"track_ids": []uint64{tr.ID, other.ID}}, &accepted)
	a.waitOperation(&accepted.OperationID)
	replacePath := fmt.Sprintf("/api/tracks/%d/replace-file", tr.ID)
	waveformPath := fmt.Sprintf("/api/tracks/%d/waveform", tr.ID)
	a.ok(http.StatusOK, http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": []uint64{tr.ID}, "reviewed": true}, nil)

	// A waveform stored for the old file.
	res := a.call(http.MethodGet, waveformPath, nil, nil)
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	oldETag := res.header.Get("ETag")
	require.NotEmpty(t, oldETag)

	// Refusals first: nothing changes.
	a.problem(http.StatusNotFound, errcodes.TrackNotFound, http.MethodPost, "/api/tracks/999999/replace-file", map[string]any{"file_path": "music/b.mp3"})
	a.fieldError("file_path", errcodes.Format, http.MethodPost, replacePath, map[string]any{"file_path": "../b.mp3"})
	a.problem(http.StatusNotFound, errcodes.TrackFileNotFound, http.MethodPost, replacePath, map[string]any{"file_path": "music/none.mp3"})
	a.fieldError("file_path", errcodes.Taken, http.MethodPost, replacePath, map[string]any{"file_path": "music/c.mp3"})

	clock := a.exec(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	insert := a.exec(`INSERT INTO clock_inserts (clock_id, offset_ms, track_id) VALUES ($1, 1800000, $2) RETURNING id`, clock, tr.ID)
	p := a.problem(http.StatusConflict, errcodes.TrackInUse, http.MethodPost, replacePath, map[string]any{"file_path": "music/b.mp3"})
	require.Equal(t, []string{fmt.Sprintf("clock:%d:Morning", clock)}, refsOf(p))
	a.exec(`DELETE FROM clock_inserts WHERE id = $1 RETURNING id`, insert)

	// A plan item that has not aired: the hour is built, the item planned in it.
	hour := time.Now().UTC().Truncate(time.Hour).Add(3 * time.Hour)
	a.exec(`INSERT INTO plan_hours (hour_start_utc, clock_id, clock_updated_at, seed, built_at) VALUES ($1, $2, now(), 1, now()) RETURNING clock_id`, hour, clock)
	item := a.exec(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status)
VALUES ($1, $2, $3, 180000, 'fill', 'planned') RETURNING id`, clock, tr.ID, hour.Add(10*time.Minute))
	p = a.problem(http.StatusConflict, errcodes.TrackInUse, http.MethodPost, replacePath, map[string]any{"file_path": "music/b.mp3"})
	require.Equal(t, library.KindPlanItem, p.UsedBy[0].Kind)
	a.exec(`DELETE FROM scheduled_items WHERE id = $1 RETURNING id`, item)
	a.exec(`DELETE FROM plan_hours WHERE clock_id = $1 RETURNING clock_id`, clock)

	// Queued in Liquidsoap: refused as in use too.
	queued := a.exec(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status)
VALUES ($1, $2, $3, 180000, 'fill', 'queued') RETURNING id`, clock, tr.ID, time.Now().UTC().Add(time.Hour))
	p = a.problem(http.StatusConflict, errcodes.TrackInUse, http.MethodPost, replacePath, map[string]any{"file_path": "music/b.mp3"})
	require.Equal(t, library.KindPlayout, p.UsedBy[0].Kind)
	a.exec(`DELETE FROM scheduled_items WHERE id = $1 RETURNING id`, queued)

	// The replace: analysed, the reviewed mark gone, the old file kept.
	var replaced dto.Track
	a.ok(http.StatusOK, http.MethodPost, replacePath, map[string]any{"file_path": "music/b.mp3"}, &replaced)
	require.Equal(t, "music/b.mp3", replaced.FilePath)
	require.Equal(t, 22050, *replaced.SampleRate)
	require.NotNil(t, replaced.AnalyzedAt)
	require.Nil(t, replaced.ReviewedAt, "a new file clears the reviewed mark")

	// The stored waveform of the old file is not served any more.
	res = a.call(http.MethodGet, waveformPath, nil, nil)
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	require.NotEqual(t, oldETag, res.header.Get("ETag"))
	require.Equal(t, res.header.Get("ETag"), `"`+*replaced.FileChecksum+`"`)

	// The same path with the same content changes nothing, reviewed mark kept.
	a.ok(http.StatusOK, http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": []uint64{tr.ID}, "reviewed": true}, nil)
	var same dto.Track
	a.ok(http.StatusOK, http.MethodPost, replacePath, map[string]any{"file_path": "music/b.mp3"}, &same)
	require.NotNil(t, same.ReviewedAt)

	// A PUT after the replace keeps the analysis of the new file.
	var kept dto.Track
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/tracks/%d", tr.ID),
		map[string]any{"title": "Renamed", "type_id": music, "playlist_ids": []uint64{}}, &kept)
	require.Equal(t, "music/b.mp3", kept.FilePath)
	require.Equal(t, 22050, *kept.SampleRate)
	require.Equal(t, *replaced.FileChecksum, *kept.FileChecksum)
	require.Nil(t, kept.ReviewedAt, "a new title clears it")
}
