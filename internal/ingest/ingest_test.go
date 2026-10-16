package ingest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

func TestImport(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("music/a/song.mp3", "A", mp3Info("Song", "ABBA"), loud(-16, -2))
	f.tools.audio("music/b.mp3", "B", mp3Info("", "abba"), loud(-10, 0.3)) // no title tag; artist differs in case
	f.tools.audio("music/copied.mp3", "K", mp3Info("Known", ""), nil)
	f.tools.audio("jingles/id.mp3", "J", mp3Info("ID", ""), nil)
	f.files.put("music/notes.txt", "text")
	f.files.put("music/broken.mp3", "garbage")
	f.files.put("music/.hidden.mp3", "hidden")
	f.store.add(Track{ID: 1, FilePath: "old/known.mp3", Checksum: new(sha("K"))})

	_, err := f.svc.Import(t.Context(), ImportRequest{Folder: "music", TypeID: 1, PlaylistIDs: []uint64{10}})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "folder music: imported 2, duplicates 1, unreadable 1, skipped 1", summary)

	created := f.store.created()
	require.Len(t, created, 2)
	byPath := map[string]storedTrack{}
	for _, c := range created {
		byPath[c.FilePath] = c
	}
	a := byPath["music/a/song.mp3"]
	require.Equal(t, "Song", a.Title)
	require.Equal(t, uint64(1), a.TypeID)
	require.Equal(t, []uint64{10}, a.PlaylistIDs)
	require.Empty(t, a.Issues)
	b := byPath["music/b.mp3"]
	require.Equal(t, "b", b.Title, "title from the file name, without the folder and the extension")
	require.Equal(t, *a.ArtistID, *b.ArtistID, "one artist, whatever the case")
	require.Equal(t, []library.QualityIssue{library.IssueLoudness, library.IssueTruePeak}, b.Issues)

	// Nothing moved; the other folder was not imported.
	require.Equal(t, []string{
		"jingles/id.mp3", "music/.hidden.mp3", "music/a/song.mp3", "music/b.mp3", "music/broken.mp3",
		"music/copied.mp3", "music/notes.txt",
	}, f.files.paths())
	require.Equal(t, 5, f.ops.progress)

	// A second run reads no file: the new tracks are in the library by
	// path. The duplicate and the unreadable file are read again.
	reads := f.files.reads
	_, err = f.svc.Import(t.Context(), ImportRequest{Folder: "music", TypeID: 1})
	require.NoError(t, err)
	summary, err = f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "folder music: in_library 2, duplicates 1, unreadable 1, skipped 1", summary)
	require.Equal(t, reads+2, f.files.reads)
	require.Len(t, f.store.created(), 2)
}

func TestImportNewFileOnly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("jingles/a.mp3", "A", mp3Info("A", ""), nil)
	f.tools.audio("jingles/b.mp3", "B", mp3Info("B", ""), nil)
	_, err := f.svc.Import(t.Context(), ImportRequest{Folder: "jingles", TypeID: 1, PlaylistIDs: []uint64{10}})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "folder jingles: imported 2", summary)

	f.tools.audio("jingles/c.mp3", "C", mp3Info("C", ""), nil)
	_, err = f.svc.Import(t.Context(), ImportRequest{Folder: "jingles", TypeID: 1, PlaylistIDs: []uint64{11}})
	require.NoError(t, err)
	summary, err = f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "folder jingles: imported 1, in_library 2", summary)
	created := f.store.created()
	require.Len(t, created, 3)
	require.Equal(t, []uint64{10}, created[0].PlaylistIDs, "existing tracks keep their playlists")
	require.Equal(t, []uint64{11}, created[2].PlaylistIDs)
}

func TestImportWholeMediaFolder(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("music/a.mp3", "A", mp3Info("A", ""), nil)
	f.tools.audio("jingles/b.mp3", "B", mp3Info("B", ""), nil)
	f.tools.audio("top.mp3", "T", mp3Info("T", ""), nil)
	_, err := f.svc.Import(t.Context(), ImportRequest{TypeID: 1})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "whole media folder: imported 3", summary)
}

func TestImportSkipsOriginalsOfCopies(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("music/a.flac", "A", mp3Info("A", ""), nil)
	f.tools.audio("music/a.optimal.mp3", "A2", mp3Info("A", ""), nil)
	f.store.add(Track{ID: 1, FilePath: "music/a.optimal.mp3", Checksum: new(sha("A2"))})
	_, err := f.svc.Import(t.Context(), ImportRequest{Folder: "music", TypeID: 1})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "folder music: in_library 2", summary)
	require.Zero(t, f.files.reads)
}

func TestImportTitleAndArtist(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("m/Morning - DJ Ann.mp3", "1", mp3Info("", ""), nil)
	f.tools.audio("m/Evening – DJ Ann.mp3", "2", mp3Info("", ""), nil)
	f.tools.audio("m/Jay-Z - Song.mp3", "3", mp3Info("", ""), nil)
	f.tools.audio("m/Tagged - Name Artist.mp3", "4", mp3Info("Tag Title", "Tag Artist"), nil)
	f.tools.audio("m/Only Title - Name Artist.mp3", "5", mp3Info("Tag Title 2", ""), nil)
	f.tools.audio("m/Name Title - Name Artist.mp3", "6", mp3Info("", "Tag Artist"), nil)

	_, err := f.svc.Import(t.Context(), ImportRequest{Folder: "m", TypeID: 1})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "folder m: imported 6", summary)

	type got struct{ title, artist string }
	byPath := map[string]got{}
	for _, c := range f.store.created() {
		g := got{title: c.Title}
		if c.ArtistID != nil {
			g.artist = f.store.artists[*c.ArtistID]
		}
		byPath[c.FilePath] = g
	}
	require.Equal(t, map[string]got{
		"m/Morning - DJ Ann.mp3":         {"Morning", "DJ Ann"},
		"m/Evening – DJ Ann.mp3":         {"Evening", "DJ Ann"},
		"m/Jay-Z - Song.mp3":             {"Jay-Z - Song", ""},
		"m/Tagged - Name Artist.mp3":     {"Tag Title", "Tag Artist"},
		"m/Only Title - Name Artist.mp3": {"Tag Title 2", "Name Artist"},
		"m/Name Title - Name Artist.mp3": {"Name Title", "Tag Artist"},
	}, byPath)
	require.Len(t, f.store.artists, 3, "DJ Ann created once for two files")
}

func TestImportArtistOfTheRequestWins(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.store.artists[7] = "House Band"
	f.tools.audio("m/Morning - DJ Ann.mp3", "1", mp3Info("", "Tag Artist"), nil)
	f.tools.audio("m/plain.mp3", "2", mp3Info("", ""), nil)
	_, err := f.svc.Import(t.Context(), ImportRequest{Folder: "m", TypeID: 1, ArtistID: new(uint64(7))})
	require.NoError(t, err)
	_, err = f.runQueued(t)
	require.NoError(t, err)
	var titles []string
	for _, c := range f.store.created() {
		require.Equal(t, uint64(7), *c.ArtistID, c.FilePath)
		titles = append(titles, c.Title)
	}
	require.ElementsMatch(t, []string{"Morning", "plain"}, titles, "the title still comes from the name")
	require.Len(t, f.store.artists, 1, "no artist created")
}

func TestImportTwoCopiesInOneRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("music/a.mp3", "A", mp3Info("Song", ""), loud(-16, -2))
	f.tools.audio("music/b.mp3", "A", mp3Info("Song", ""), loud(-16, -2))
	_, err := f.svc.Import(t.Context(), ImportRequest{Folder: "music", TypeID: 1})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "folder music: imported 1, duplicates 1", summary)
	require.Len(t, f.store.created(), 1)
}

func TestImportFailureLeavesTheFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("music/song.mp3", "A", mp3Info("Song", ""), loud(-16, -2))
	f.store.createErr = errors.New("database down")

	_, err := f.svc.Import(t.Context(), ImportRequest{Folder: "music", TypeID: 1})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "folder music: failed 1; failed: [music/song.mp3] (see the log)", summary)
	require.Equal(t, []string{"music/song.mp3"}, f.files.paths())
}

func TestImportRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.files.put("music/a.mp3", "A")
	_, err := f.svc.Import(t.Context(), ImportRequest{
		Folder: "jingles", TypeID: 9, PlaylistIDs: []uint64{10, 77}, ArtistID: new(uint64(5)),
	})
	var verr *validation.Error
	require.ErrorAs(t, err, &verr)
	fields := make([]string, len(verr.Fields))
	for i, fe := range verr.Fields {
		fields[i] = fe.Field
	}
	require.Equal(t, []string{"folder", "type_id", "playlist_ids", "artist_id"}, fields)
	require.Equal(t, "no such folder in the media folder", verr.Fields[0].Message)
	require.Empty(t, f.ops.started)

	f.tools.missing = true
	_, err = f.svc.Import(t.Context(), ImportRequest{TypeID: 1})
	require.ErrorIs(t, err, ErrToolsUnavailable)
}

func TestFolderValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.files.put("music/a.mp3", "A")
	f.files.put(".hidden/a.mp3", "A")
	for _, folder := range []string{"..", "music/..", "/music", `music\rock`, "music/", "./music", ".hidden", "missing", "music/a.mp3"} {
		_, err := f.svc.Files(t.Context(), folder)
		var verr *validation.Error
		require.ErrorAs(t, err, &verr, folder)
		require.Equal(t, "folder", verr.Fields[0].Field, folder)
		require.Equal(t, validation.Invalid, verr.Fields[0].Rule, folder)
	}
	for _, folder := range []string{"", "music"} {
		_, err := f.svc.Files(t.Context(), folder)
		require.NoError(t, err, folder)
	}
}

func TestFolders(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.files.put("music/a.mp3", "A")
	f.files.put("music/notes.txt", "N")
	f.files.put("music/rock/b.mp3", "B")
	f.files.put("music/rock/c.flac", "C")
	f.files.put("music/rock/c.optimal.mp3", "C2")
	f.files.put("jingles/id.mp3", "J")
	f.files.put("top.mp3", "T")
	f.files.put(".trash/x.mp3", "X")
	f.files.mkdir("empty")
	f.store.add(Track{ID: 1, FilePath: "music/rock/b.mp3"})
	f.store.add(Track{ID: 2, FilePath: "music/rock/c.optimal.mp3"})

	got, err := f.svc.Folders(t.Context())
	require.NoError(t, err)
	require.Equal(t, Folders{
		Root: Counts{AudioFiles: 6, NewFiles: 3},
		Folders: []Folder{
			{Path: "empty"},
			{Path: "jingles", Counts: Counts{AudioFiles: 1, NewFiles: 1}},
			{Path: "music", Counts: Counts{AudioFiles: 4, NewFiles: 1}},
			{Path: "music/rock", Counts: Counts{AudioFiles: 3, NewFiles: 0}},
		},
	}, got)
}

func TestFoldersLimit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for i := range maxFolders + 1 {
		f.files.mkdir(fmt.Sprintf("f%04d", i))
	}
	got, err := f.svc.Folders(t.Context())
	require.NoError(t, err)
	require.Len(t, got.Folders, maxFolders)
	require.True(t, got.More)
}

func TestFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.files.put("music/a.mp3", "A")
	f.files.put("music/notes.txt", "N")
	f.files.put("music/rock/b.mp3", "BB")
	f.files.put("music/rock/c.flac", "C")
	f.files.put("music/rock/c.optimal.mp3", "C2")
	f.files.put("music/d.flac", "D")
	f.files.put("other/d.optimal.mp3", "D2")
	f.store.add(Track{ID: 1, FilePath: "music/rock/b.mp3"})
	f.store.add(Track{ID: 2, FilePath: "music/rock/c.optimal.mp3"})
	f.store.add(Track{ID: 3, FilePath: "other/d.optimal.mp3"})

	got, err := f.svc.Files(t.Context(), "music")
	require.NoError(t, err)
	require.Equal(t, "music", got.Folder)
	require.False(t, got.More)
	type row struct {
		path    string
		status  FileStatus
		trackID uint64
	}
	rows := make([]row, len(got.Files))
	for i, file := range got.Files {
		rows[i] = row{file.Path, file.Status, file.TrackID}
	}
	require.Equal(t, []row{
		{"music/a.mp3", FileNew, 0},
		{"music/d.flac", FileNew, 0}, // its copy is in another folder
		{"music/notes.txt", FileNotAudio, 0},
		{"music/rock/b.mp3", FileInLibrary, 1},
		{"music/rock/c.flac", FileOriginal, 2},
		{"music/rock/c.optimal.mp3", FileInLibrary, 2},
	}, rows)
	require.Equal(t, int64(2), got.Files[3].Size)
}

func TestOneOperationAtATime(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.svc.Import(t.Context(), ImportRequest{TypeID: 1})
	require.NoError(t, err)
	_, err = f.svc.Analyse(t.Context(), AnalyseRequest{})
	require.ErrorIs(t, err, ErrRunning)
	_, err = f.runQueued(t)
	require.NoError(t, err)
	_, err = f.svc.Analyse(t.Context(), AnalyseRequest{})
	require.NoError(t, err, "accepted again once the first one finished")
	require.Equal(t, []operations.Kind{operations.KindImport, operations.KindAnalyse}, f.ops.started)
}

func TestRunFailsTheQueuedOperationAtShutdown(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.svc.Analyse(t.Context(), AnalyseRequest{})
	require.NoError(t, err)
	ctx, cancel := contextCancelled(t)
	defer cancel()
	require.NoError(t, f.svc.Run(ctx))
	// Whether Run takes the job or the shutdown first, the operation fails.
	require.ErrorIs(t, f.ops.failure[1], context.Canceled)
}

func TestAnalyse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Never analysed.
	f.tools.audio("library/new.mp3", "N", mp3Info("", ""), loud(-20, -3))
	f.store.add(Track{ID: 1, FilePath: "library/new.mp3"})
	// Analysed, unchanged since: rechecked without decoding.
	f.tools.audio("library/same.mp3", "S", mp3Info("", ""), loud(-16, -2))
	stale := goodMP3()
	stale.LoudnessLUFS = new(-30.0) // what the last analysis measured
	f.store.add(Track{ID: 2, FilePath: "library/same.mp3", Checksum: new(sha("S")), Measurements: stale, AnalyzedAt: new(testNow)})
	// Replaced on disk since its analysis: decoded, an original again.
	f.tools.audio("library/changed.mp3", "C2", mp3Info("", ""), loud(-16, -2))
	f.store.add(Track{
		ID: 3, FilePath: "library/changed.mp3", Checksum: new(sha("C1")),
		Measurements: Measurements{SourceBitrate: new(64000)}, AnalyzedAt: new(testNow),
	})
	// Gone.
	f.store.add(Track{ID: 4, FilePath: "library/gone.mp3"})

	_, err := f.svc.Analyse(t.Context(), AnalyseRequest{TrackIDs: []uint64{1, 2, 3, 4}})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "analysed 2, rechecked 1, file missing 1", summary)

	require.Equal(t, []library.QualityIssue{library.IssueLoudness}, f.store.track(1).Issues)
	require.Equal(t, sha("N"), *f.store.track(1).Checksum)
	require.Equal(t, -30.0, *f.store.track(2).Measurements.LoudnessLUFS, "stored measurements kept")
	require.Equal(t, []library.QualityIssue{library.IssueLoudness}, f.store.track(2).Issues, "checked against the current settings")
	require.Nil(t, f.store.track(3).Measurements.SourceBitrate)
	require.Empty(t, f.store.track(3).Issues)
	require.Equal(t, []library.QualityIssue{library.IssueFileMissing}, f.store.track(4).Issues)

	// Force decodes the unchanged file.
	_, err = f.svc.Analyse(t.Context(), AnalyseRequest{TrackIDs: []uint64{2}, Force: true})
	require.NoError(t, err)
	summary, err = f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "analysed 1", summary)
	require.Empty(t, f.store.track(2).Issues)
}

func TestAnalyseAllUnanalysed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("library/a.mp3", "A", mp3Info("", ""), loud(-16, -2))
	f.store.add(Track{ID: 1, FilePath: "library/a.mp3"})
	f.store.add(Track{ID: 2, FilePath: "library/b.mp3", AnalyzedAt: new(testNow)})
	_, err := f.svc.Analyse(t.Context(), AnalyseRequest{})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "analysed 1", summary)
}

func TestAnalyseUnknownTracks(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.store.add(Track{ID: 1, FilePath: "a.mp3"})
	_, err := f.svc.Analyse(t.Context(), AnalyseRequest{TrackIDs: []uint64{1, 5, 6}})
	var verr *validation.Error
	require.ErrorAs(t, err, &verr)
	require.Equal(t, "track_ids", verr.Fields[0].Field)
	require.Contains(t, verr.Fields[0].Message, "[5 6]")
}

// A track deleted while the analysis runs is skipped, not failed
// (docs/phase-19-plan.md decision 8).
func TestAnalyseTrackDeletedMeanwhile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("library/a.mp3", "A", mp3Info("", ""), loud(-16, -2))
	f.tools.audio("library/b.mp3", "B", mp3Info("", ""), loud(-16, -2))
	f.store.add(Track{ID: 1, FilePath: "library/a.mp3"})
	f.store.add(Track{ID: 2, FilePath: "library/b.mp3"})
	f.store.add(Track{ID: 3, FilePath: "library/gone.mp3"})
	f.store.deletedMidRun[2], f.store.deletedMidRun[3] = true, true

	_, err := f.svc.Analyse(t.Context(), AnalyseRequest{TrackIDs: []uint64{1, 2, 3}})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "analysed 1, track deleted 2", summary)
}

// A transcode of a track deleted meanwhile is skipped: a copy is removed,
// a file replaced in place stays (files are never deleted).
func TestTranscodeTrackDeletedMeanwhile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		output  OutputMode
		content string
	}{
		{"copy", OutputCopy, "J"},
		{"replace", OutputReplace, "J|libmp3lame"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.tools.audio("library/jingle.mp3", "J", mp3Info("", ""), loud(-9, 0.4))
			f.store.add(Track{ID: 1, FilePath: "library/jingle.mp3", Issues: []library.QualityIssue{library.IssueLoudness}})
			f.store.deletedMidRun[1] = true

			_, err := f.svc.Transcode(t.Context(), TranscodeRequest{TrackIDs: []uint64{1}, Fix: library.FixLoudness, Output: tt.output})
			require.NoError(t, err)
			summary, err := f.runQueued(t)
			require.NoError(t, err)
			require.Equal(t, "track deleted 1", summary)
			require.Equal(t, []string{"library/jingle.mp3"}, f.files.paths(), "no copy left")
			content, _ := f.files.get("library/jingle.mp3")
			require.Equal(t, tt.content, content)
		})
	}
}

func TestTranscodeOptimizeCopy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	info := mp3Info("", "")
	info.Format, info.Codec, info.Bitrate = new(media.FormatWAV), new("pcm_s16le"), new(1411200)
	info.SampleRate = new(48000)
	f.tools.audio("library/talk.wav", "W", info, loud(-24, -6))
	f.store.add(Track{ID: 1, FilePath: "library/talk.wav", Issues: []library.QualityIssue{library.IssueFormat}})

	_, err := f.svc.Transcode(t.Context(), TranscodeRequest{TrackIDs: []uint64{1}, Fix: library.FixOptimize, Output: OutputCopy})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "transcoded 1", summary)

	call := f.tools.encodes[0]
	require.Equal(t, "library/talk.wav", call.src)
	require.Equal(t, "library/.talk.optimal.transcode-1.mp3", call.dst)
	require.Equal(t, media.Encoding{
		Output: media.Output{Encoder: "libmp3lame", Muxer: "mp3", Ext: ".mp3"}, BitrateKbps: 320, SampleRate: 44100, Channels: 2,
	}, call.enc)
	require.NotNil(t, call.norm, "the loudness issue is fixed too")
	require.Equal(t, -16.0, call.norm.TargetLUFS)
	require.Equal(t, -24.0, call.norm.Measured.IntegratedLUFS)

	tr := f.store.track(1)
	require.Equal(t, "library/talk.optimal.mp3", tr.FilePath)
	require.Empty(t, tr.Issues)
	require.Nil(t, tr.Measurements.SourceBitrate, "a lossless source limits nothing")
	require.Equal(t, []string{"library/talk.optimal.mp3", "library/talk.wav"}, f.files.paths(), "the original stays")
}

func TestTranscodeLoudnessReplace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	info := mp3Info("", "")
	info.Bitrate = new(96000) // low: the issue stays with the new file
	f.tools.audio("library/jingle.mp3", "J", info, loud(-9, 0.4))
	f.store.add(Track{ID: 1, FilePath: "library/jingle.mp3", Issues: []library.QualityIssue{library.IssueLoudness}})

	_, err := f.svc.Transcode(t.Context(), TranscodeRequest{TrackIDs: []uint64{1}, Fix: library.FixLoudness, Output: OutputReplace})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "transcoded 1", summary)

	call := f.tools.encodes[0]
	require.Equal(t, 96, call.enc.BitrateKbps, "own bitrate, rounded up to a standard one")
	require.Equal(t, "libmp3lame", call.enc.Output.Encoder)
	tr := f.store.track(1)
	require.Equal(t, "library/jingle.mp3", tr.FilePath)
	require.Equal(t, 96000, *tr.Measurements.SourceBitrate)
	require.Equal(t, []library.QualityIssue{library.IssueLowBitrate}, tr.Issues)
	require.Equal(t, []string{"library/jingle.mp3"}, f.files.paths())
	content, _ := f.files.get("library/jingle.mp3")
	require.Equal(t, "J|libmp3lame", content, "replaced in place")
}

func TestTranscodeReplaceWithOtherExtensionRemovesTheOriginal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	info := mp3Info("", "")
	info.Format, info.Codec = new(media.FormatOgg), new("vorbis")
	f.tools.audio("library/a.ogg", "O", info, loud(-16, -2))
	f.store.add(Track{ID: 1, FilePath: "library/a.ogg"})

	_, err := f.svc.Transcode(t.Context(), TranscodeRequest{TrackIDs: []uint64{1}, Fix: library.FixOptimize, Output: OutputReplace})
	require.NoError(t, err)
	_, err = f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "library/a.mp3", f.store.track(1).FilePath)
	require.Equal(t, []string{"library/a.mp3"}, f.files.paths())
	require.Nil(t, f.tools.encodes[0].norm, "loudness already fine")
}

func TestTranscodeSkips(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Fine already.
	f.tools.audio("library/ok.mp3", "OK", mp3Info("", ""), loud(-16, -2))
	f.store.add(Track{ID: 1, FilePath: "library/ok.mp3"})
	// Busy.
	ogg := mp3Info("", "")
	ogg.Format, ogg.Codec = new(media.FormatOgg), new("vorbis")
	f.tools.audio("library/busy.ogg", "BUSY", ogg, loud(-16, -2))
	f.store.add(Track{ID: 2, FilePath: "library/busy.ogg"})
	f.store.busy[2] = true
	// The target path is another track's.
	f.tools.audio("library/x.ogg", "X", ogg, loud(-16, -2))
	f.store.add(Track{ID: 3, FilePath: "library/x.ogg"})
	f.store.add(Track{ID: 4, FilePath: "library/x.mp3"})
	// Only the loudness fix asked, but the issue is the format.
	f.tools.audio("library/fmt.ogg", "F", ogg, loud(-16, -2))
	f.store.add(Track{ID: 5, FilePath: "library/fmt.ogg"})
	// Missing file.
	f.store.add(Track{ID: 6, FilePath: "library/gone.ogg"})

	_, err := f.svc.Transcode(t.Context(), TranscodeRequest{TrackIDs: []uint64{1, 2, 3, 6}, Fix: library.FixOptimize, Output: OutputReplace})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "nothing to fix 1, busy 1, path taken 1, file missing 1", summary)

	_, err = f.svc.Transcode(t.Context(), TranscodeRequest{TrackIDs: []uint64{5}, Fix: library.FixLoudness, Output: OutputReplace})
	require.NoError(t, err)
	summary, err = f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "nothing to fix 1", summary)
	require.Empty(t, f.tools.encodes)
}

func TestTranscodeEncodeFailureLeavesTheTrack(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ogg := mp3Info("", "")
	ogg.Format, ogg.Codec = new(media.FormatOgg), new("vorbis")
	f.tools.audio("library/a.ogg", "O", ogg, loud(-16, -2))
	f.store.add(Track{ID: 1, FilePath: "library/a.ogg"})
	f.tools.encodeErr = errors.New("encoder crashed")

	_, err := f.svc.Transcode(t.Context(), TranscodeRequest{TrackIDs: []uint64{1}, Fix: library.FixOptimize, Output: OutputCopy})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "failed 1; failed: [library/a.ogg] (see the log)", summary)
	require.Equal(t, "library/a.ogg", f.store.track(1).FilePath)
	require.Equal(t, []string{"library/a.ogg"}, f.files.paths())
}

func TestTargetPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		src, ext   string
		mode       OutputMode
		final, tmp string
	}{
		{"music/a.wav", ".mp3", OutputCopy, "music/a.optimal.mp3", "music/.a.optimal.transcode-7.mp3"},
		{"music/a.optimal.mp3", ".mp3", OutputCopy, "music/a.optimal.mp3", "music/.a.optimal.transcode-7.mp3"},
		{"music/a.wav", ".mp3", OutputReplace, "music/a.mp3", "music/.a.transcode-7.mp3"},
		{"a.mp3", ".mp3", OutputReplace, "a.mp3", ".a.transcode-7.mp3"},
	}
	for _, tt := range tests {
		final, tmp := targetPaths(tt.src, tt.ext, tt.mode, 7)
		require.Equal(t, tt.final, final, tt.src)
		require.Equal(t, tt.tmp, tmp, tt.src)
	}
}

func TestImportable(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"song.mp3": true, "Song.FLAC": true, "x.m4a": true, "x.oga": true, "x.wav": true,
		".song.mp3": false, "~$song.mp3": false, "song.mp3.part": false, "song.tmp": false,
		"cover.jpg": false, "song": false,
	} {
		require.Equal(t, want, importable(name), name)
	}
}
