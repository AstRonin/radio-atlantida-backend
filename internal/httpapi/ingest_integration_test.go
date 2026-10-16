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

// folderCounts returns the counts of the folders by path.
func (a *libraryAPI) folderCounts() (dto.ImportCounts, map[string]dto.ImportCounts) {
	a.t.Helper()
	var fs dto.ImportFolders
	a.ok(http.StatusOK, http.MethodGet, "/api/import/folders", nil, &fs)
	out := map[string]dto.ImportCounts{}
	for _, f := range fs.Folders {
		out[f.Path] = f.ImportCounts
	}
	return fs.Root, out
}

func TestImportFolder(t *testing.T) {
	t.Parallel()
	a := newLibraryAPI(t)
	music, jingle := a.typeID("music"), a.typeID("jingle")

	root, folders := a.folderCounts()
	require.Equal(t, dto.ImportCounts{}, root, "an empty media root")
	require.Empty(t, folders)
	a.fieldError("folder", errcodes.Invalid, http.MethodGet, "/api/import/files?folder=missing", nil)
	a.fieldError("folder", errcodes.Invalid, http.MethodGet, "/api/import/files?folder=..%2Fetc", nil)
	a.problem(http.StatusNotFound, errcodes.NotFound, http.MethodGet, "/api/import/inbox", nil)
	var settingsList dto.SettingList
	a.ok(http.StatusOK, http.MethodGet, "/api/settings", nil, &settingsList)
	for _, s := range settingsList.Items {
		require.NotContains(t, []string{"import.inbox_dir", "import.library_dir"}, s.Key)
	}

	if !a.tools {
		t.Log("ffmpeg not installed: only the refusals are checked")
		a.problem(http.StatusServiceUnavailable, errcodes.MediaToolsUnavailable, http.MethodPost, "/api/import", map[string]any{"type_id": music})
		a.problem(http.StatusServiceUnavailable, errcodes.MediaToolsUnavailable, http.MethodPost, "/api/tracks/analyse", map[string]any{})
		return
	}

	// Three songs and two jingles, each with its own content. A mono,
	// 22 kHz, 64 kbps, quiet MP3 with tags, and stereo 44.1 kHz 320 kbps
	// ones at about the target loudness (the lavfi sine is at -18 dBFS, and
	// the upmix to stereo takes 3 dB: +5.8 dB gives about -16 LUFS).
	good := []string{"-ac", "2", "-ar", "44100", "-b:a", "320k", "-af", "volume=5.8dB"}
	a.tone("music/talk/low.mp3", "-ac", "1", "-ar", "22050", "-b:a", "64k", "-af", "volume=-20dB",
		"-metadata", "title=Low Tone", "-metadata", "artist=Test Artist")
	a.tone("music/Morning - DJ Ann.mp3", append(good, "-metadata", "comment=1")...)
	a.tone("music/Jay-Z - Song.mp3", append(good, "-metadata", "comment=2")...)
	a.tone("jingles/id1.mp3", append(good, "-metadata", "comment=3")...)
	a.tone("jingles/id2.mp3", append(good, "-metadata", "comment=4")...)
	require.NoError(t, os.WriteFile(filepath.Join(a.mediaRoot, "music", "readme.txt"), []byte("x"), 0o600))

	root, folders = a.folderCounts()
	require.Equal(t, dto.ImportCounts{AudioFiles: 5, NewFiles: 5}, root)
	require.Equal(t, map[string]dto.ImportCounts{
		"jingles":    {AudioFiles: 2, NewFiles: 2},
		"music":      {AudioFiles: 3, NewFiles: 3},
		"music/talk": {AudioFiles: 1, NewFiles: 1},
	}, folders)

	a.fieldError("folder", errcodes.Invalid, http.MethodPost, "/api/import", map[string]any{"folder": "missing", "type_id": jingle})
	a.fieldError("type_id", errcodes.UnknownID, http.MethodPost, "/api/import", map[string]any{"type_id": 999999})
	a.fieldError("playlist_ids", errcodes.UnknownID, http.MethodPost, "/api/import", map[string]any{"type_id": music, "playlist_ids": []uint64{999999}})
	a.fieldError("artist_id", errcodes.UnknownID, http.MethodPost, "/api/import", map[string]any{"type_id": music, "artist_id": 999999})

	// The jingles where they lie.
	var accepted dto.OperationAccepted
	a.ok(http.StatusAccepted, http.MethodPost, "/api/import", map[string]any{"folder": "jingles", "type_id": jingle}, &accepted)
	op := a.waitOperation(&accepted.OperationID)
	require.Equal(t, "folder jingles: imported 2", op.ResultSummary)
	id1 := a.trackByTitle("id1")
	require.Equal(t, "jingles/id1.mp3", id1.FilePath)
	require.Equal(t, jingle, id1.Type.ID)
	_, err := os.Stat(filepath.Join(a.mediaRoot, "jingles", "id1.mp3"))
	require.NoError(t, err, "the file has not moved")
	_, folders = a.folderCounts()
	require.Equal(t, dto.ImportCounts{AudioFiles: 2, NewFiles: 0}, folders["jingles"])

	var files dto.ImportFiles
	a.ok(http.StatusOK, http.MethodGet, "/api/import/files?folder=jingles", nil, &files)
	require.Equal(t, "jingles", files.Folder)
	require.Len(t, files.Items, 2)
	require.Equal(t, "in_library", files.Items[0].Status)
	require.Equal(t, id1.ID, *files.Items[0].TrackID)

	// A third jingle: the next import adds exactly that one.
	a.tone("jingles/id3.mp3", append(good, "-metadata", "comment=5")...)
	a.ok(http.StatusAccepted, http.MethodPost, "/api/import", map[string]any{"folder": "jingles", "type_id": jingle}, &accepted)
	op = a.waitOperation(&accepted.OperationID)
	require.Equal(t, "folder jingles: imported 1, in_library 2", op.ResultSummary)

	// The music: title and artist from the file name when there are no
	// tags.
	a.ok(http.StatusAccepted, http.MethodPost, "/api/import", map[string]any{"folder": "music", "type_id": music}, &accepted)
	op = a.waitOperation(&accepted.OperationID)
	require.Equal(t, "folder music: imported 3, skipped 1", op.ResultSummary)
	morning := a.trackByTitle("Morning")
	require.Equal(t, "music/Morning - DJ Ann.mp3", morning.FilePath)
	require.Equal(t, "DJ Ann", morning.Artist.Name)
	jay := a.trackByTitle("Jay-Z - Song")
	require.Nil(t, jay.Artist)

	low := a.trackByTitle("Low Tone")
	require.Equal(t, "music/talk/low.mp3", low.FilePath)
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

	// With artist_id, every new track of the run gets that artist; a copy
	// of a track's file is a duplicate and stays where it is.
	var band dto.Artist
	a.ok(http.StatusCreated, http.MethodPost, "/api/artists", map[string]any{"name": "House Band"}, &band)
	a.tone("promos/Spot - Somebody.mp3", append(good, "-metadata", "comment=6")...)
	a.tone("promos/again.mp3", append(good, "-metadata", "comment=1")...)
	a.ok(http.StatusAccepted, http.MethodPost, "/api/import", map[string]any{"type_id": music, "artist_id": band.ID}, &accepted)
	op = a.waitOperation(&accepted.OperationID)
	require.Equal(t, "whole media folder: imported 1, in_library 6, duplicates 1, skipped 1", op.ResultSummary)
	spot := a.trackByTitle("Spot")
	require.Equal(t, "House Band", spot.Artist.Name)
	_, err = os.Stat(filepath.Join(a.mediaRoot, "promos", "again.mp3"))
	require.NoError(t, err)

	// Optimize the low track into a copy: everything but the bitrate of
	// its source is fixed. The new file clears the reviewed mark.
	var reviewed dto.ReviewTracksResult
	a.ok(http.StatusOK, http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": []uint64{low.ID}, "reviewed": true}, &reviewed)
	a.ok(http.StatusAccepted, http.MethodPost, "/api/tracks/transcode",
		map[string]any{"track_ids": []uint64{low.ID}, "fix": "optimize", "output": "copy"}, &accepted)
	op = a.waitOperation(&accepted.OperationID)
	require.Equal(t, "transcoded 1", op.ResultSummary)
	var fixed dto.Track
	a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/tracks/%d", low.ID), nil, &fixed)
	require.Equal(t, "music/talk/low.optimal.mp3", fixed.FilePath)
	require.Equal(t, 2, *fixed.Channels)
	require.Equal(t, 44100, *fixed.SampleRate)
	require.InDelta(t, -16, *fixed.LoudnessLUFS, 1.5)
	require.LessOrEqual(t, *fixed.TruePeakDBTP, -0.5)
	require.InDelta(t, 64000, *fixed.SourceBitrate, 2000, "the measured rate of the 64 kbps source")
	require.Equal(t, []string{"low_bitrate"}, fixed.QualityIssues)
	require.Empty(t, fixed.Fixes)
	require.Nil(t, fixed.ReviewedAt, "a transcode clears the reviewed mark")
	a.ok(http.StatusOK, http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": []uint64{low.ID}, "reviewed": true}, &reviewed)
	_, err = os.Stat(filepath.Join(a.mediaRoot, "music", "talk", "low.mp3"))
	require.NoError(t, err, "the original stays next to the copy")
	a.ok(http.StatusOK, http.MethodGet, "/api/import/files?folder=music/talk", nil, &files)
	require.Len(t, files.Items, 2)
	require.Equal(t, "original", files.Items[0].Status, "the original is not new")
	require.Equal(t, low.ID, *files.Items[0].TrackID)
	_, folders = a.folderCounts()
	require.Equal(t, dto.ImportCounts{AudioFiles: 2, NewFiles: 0}, folders["music/talk"])

	// The fix covers nothing now.
	a.ok(http.StatusAccepted, http.MethodPost, "/api/tracks/transcode",
		map[string]any{"track_ids": []uint64{low.ID}, "fix": "loudness", "output": "replace"}, &accepted)
	op = a.waitOperation(&accepted.OperationID)
	require.Equal(t, "nothing to fix 1", op.ResultSummary)

	// A deleted file is found by the analysis.
	require.NoError(t, os.Remove(filepath.Join(a.mediaRoot, "jingles", "id1.mp3")))
	a.ok(http.StatusAccepted, http.MethodPost, "/api/tracks/analyse", map[string]any{"track_ids": []uint64{id1.ID, low.ID}}, &accepted)
	op = a.waitOperation(&accepted.OperationID)
	require.Equal(t, "rechecked 1, file missing 1", op.ResultSummary)
	a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/tracks/%d", id1.ID), nil, &id1)
	require.Equal(t, []string{"file_missing"}, id1.QualityIssues)
	require.Equal(t, "jingles/id1.mp3", id1.FilePath, "nothing else changes")
	a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/tracks/%d", low.ID), nil, &fixed)
	require.NotNil(t, fixed.ReviewedAt, "an analysis of the unchanged file keeps the mark")

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
