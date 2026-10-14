//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
)

// tone writes a sine tone with ffmpeg to rel under the media root.
func (a *libraryAPI) tone(rel string, args ...string) {
	a.t.Helper()
	dst := filepath.Join(a.mediaRoot, filepath.FromSlash(rel))
	require.NoError(a.t, os.MkdirAll(filepath.Dir(dst), 0o750))
	full := append([]string{"-hide_banner", "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=3"}, args...)
	full = append(full, dst)
	out, err := exec.CommandContext(a.t.Context(), "ffmpeg", full...).CombinedOutput() //nolint:gosec // G204: arguments written by the test
	require.NoError(a.t, err, string(out))
}

func (a *libraryAPI) trackByTitle(title string) dto.Track {
	a.t.Helper()
	var page dto.TrackPage
	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?q="+url.QueryEscape(title), nil, &page)
	require.Len(a.t, page.Items, 1, "%+v", page.Items)
	return page.Items[0]
}

func TestImportInbox(t *testing.T) {
	t.Parallel()
	a := newLibraryAPI(t)
	music := a.typeID("music")

	var inbox dto.Inbox
	a.ok(http.StatusOK, http.MethodGet, "/api/import/inbox", nil, &inbox)
	require.Equal(t, "incoming", inbox.Dir)
	require.Empty(t, inbox.Items, "no inbox folder yet")

	if !a.tools {
		t.Log("ffmpeg not installed: only the refusals are checked")
		a.problem(http.StatusServiceUnavailable, errcodes.MediaToolsUnavailable, http.MethodPost, "/api/import", map[string]any{"type_id": music})
		a.problem(http.StatusServiceUnavailable, errcodes.MediaToolsUnavailable, http.MethodPost, "/api/tracks/analyse", map[string]any{})
		return
	}

	a.fieldError("type_id", errcodes.UnknownID, http.MethodPost, "/api/import", map[string]any{"type_id": 999999})
	a.fieldError("playlist_ids", errcodes.UnknownID, http.MethodPost, "/api/import", map[string]any{"type_id": music, "playlist_ids": []uint64{999999}})

	// A mono, 22 kHz, 64 kbps, quiet MP3 with tags, and a stereo 44.1 kHz
	// 320 kbps one at about the target loudness (the lavfi sine is at
	// -18 dBFS, and the upmix to stereo takes 3 dB: +5.8 dB gives about
	// -16 LUFS).
	a.tone("incoming/talk/low.mp3", "-ac", "1", "-ar", "22050", "-b:a", "64k", "-af", "volume=-20dB",
		"-metadata", "title=Low Tone", "-metadata", "artist=Test Artist")
	a.tone("incoming/good.mp3", "-ac", "2", "-ar", "44100", "-b:a", "320k", "-af", "volume=5.8dB")
	require.NoError(t, os.WriteFile(filepath.Join(a.mediaRoot, "incoming", "readme.txt"), []byte("x"), 0o600))
	a.ok(http.StatusOK, http.MethodGet, "/api/import/inbox", nil, &inbox)
	require.Len(t, inbox.Items, 3)

	var accepted dto.OperationAccepted
	a.ok(http.StatusAccepted, http.MethodPost, "/api/import", map[string]any{"type_id": music}, &accepted)
	op := a.waitOperation(&accepted.OperationID)
	require.Equal(t, "imported 2, skipped 1", op.ResultSummary)

	a.ok(http.StatusOK, http.MethodGet, "/api/import/inbox", nil, &inbox)
	require.Len(t, inbox.Items, 1, "only the text file stays")

	low := a.trackByTitle("Low Tone")
	require.Equal(t, "library/talk/low.mp3", low.FilePath)
	require.Equal(t, "Test Artist", low.Artist.Name)
	require.Equal(t, 1, *low.Channels)
	require.Equal(t, "mp3", *low.Codec)
	require.NotNil(t, low.LoudnessLUFS)
	require.NotNil(t, low.AnalyzedAt)
	require.False(t, *low.QualityOK)
	require.Equal(t, []string{"sample_rate", "channels", "loudness", "low_bitrate"}, low.QualityIssues)
	require.Equal(t, []string{"loudness", "optimize"}, low.Fixes)

	var page dto.TrackPage
	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?quality=issues", nil, &page)
	require.Len(t, page.Items, 1)

	// The same file again is a duplicate and stays in the inbox.
	a.tone("incoming/again.mp3", "-ac", "2", "-ar", "44100", "-b:a", "320k", "-af", "volume=5.8dB")
	a.ok(http.StatusAccepted, http.MethodPost, "/api/import", map[string]any{"type_id": music}, &accepted)
	op = a.waitOperation(&accepted.OperationID)
	require.Equal(t, "duplicates 1, skipped 1", op.ResultSummary)

	// Optimize the low track into a copy: everything but the bitrate of
	// its source is fixed.
	a.ok(http.StatusAccepted, http.MethodPost, "/api/tracks/transcode",
		map[string]any{"track_ids": []uint64{low.ID}, "fix": "optimize", "output": "copy"}, &accepted)
	op = a.waitOperation(&accepted.OperationID)
	require.Equal(t, "transcoded 1", op.ResultSummary)
	var fixed dto.Track
	a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/tracks/%d", low.ID), nil, &fixed)
	require.Equal(t, "library/talk/low.optimal.mp3", fixed.FilePath)
	require.Equal(t, 2, *fixed.Channels)
	require.Equal(t, 44100, *fixed.SampleRate)
	require.InDelta(t, -16, *fixed.LoudnessLUFS, 1.5)
	require.LessOrEqual(t, *fixed.TruePeakDBTP, -0.5)
	require.InDelta(t, 64000, *fixed.SourceBitrate, 2000, "the measured rate of the 64 kbps source")
	require.Equal(t, []string{"low_bitrate"}, fixed.QualityIssues)
	require.Empty(t, fixed.Fixes)
	_, err := os.Stat(filepath.Join(a.mediaRoot, "library", "talk", "low.mp3"))
	require.NoError(t, err, "the original stays next to the copy")

	// The fix covers nothing now.
	a.ok(http.StatusAccepted, http.MethodPost, "/api/tracks/transcode",
		map[string]any{"track_ids": []uint64{low.ID}, "fix": "loudness", "output": "replace"}, &accepted)
	op = a.waitOperation(&accepted.OperationID)
	require.Equal(t, "nothing to fix 1", op.ResultSummary)

	// A deleted file is found by the analysis.
	require.NoError(t, os.Remove(filepath.Join(a.mediaRoot, "library", "good.mp3")))
	good := a.trackByTitle("good")
	a.ok(http.StatusAccepted, http.MethodPost, "/api/tracks/analyse", map[string]any{"track_ids": []uint64{good.ID, low.ID}}, &accepted)
	op = a.waitOperation(&accepted.OperationID)
	require.Equal(t, "rechecked 1, file missing 1", op.ResultSummary)
	a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/tracks/%d", good.ID), nil, &good)
	require.Equal(t, []string{"file_missing"}, good.QualityIssues)

	a.fieldError("track_ids", errcodes.UnknownID, http.MethodPost, "/api/tracks/transcode",
		map[string]any{"track_ids": []uint64{999999}, "fix": "optimize", "output": "copy"})
}

func TestTranscodeBodyIsValidated(t *testing.T) {
	t.Parallel()
	a := newLibraryAPI(t)
	p := a.problem(http.StatusUnprocessableEntity, errcodes.ValidationFailed, http.MethodPost, "/api/tracks/transcode",
		map[string]any{"track_ids": []uint64{}, "fix": "louder", "output": "copy"})
	require.Len(t, p.Errors, 2, "%+v", p.Errors)
}
