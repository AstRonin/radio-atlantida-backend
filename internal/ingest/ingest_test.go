package ingest

import (
	"context"
	"errors"
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
	f.tools.audio("incoming/a/song.mp3", "A", mp3Info("Song", "ABBA"), loud(-16, -2))
	f.tools.audio("incoming/b.mp3", "B", mp3Info("", "abba"), loud(-10, 0.3)) // no title tag; artist differs in case
	f.tools.audio("incoming/known.mp3", "K", mp3Info("Known", ""), nil)
	f.files.put("incoming/notes.txt", "text")
	f.files.put("incoming/broken.mp3", "garbage")
	f.files.put("incoming/.hidden.mp3", "hidden")
	f.files.put("library/b.mp3", "taken") // a name collision in the library folder
	f.store.add(Track{ID: 1, FilePath: "library/known.mp3", Checksum: new(sha("K"))})

	_, err := f.svc.Import(t.Context(), ImportRequest{TypeID: 1, PlaylistIDs: []uint64{10}})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "imported 2, duplicates 1, unreadable 1, skipped 1", summary)

	created := f.store.created()
	require.Len(t, created, 2)
	byPath := map[string]storedTrack{}
	for _, c := range created {
		byPath[c.FilePath] = c
	}
	a := byPath["library/a/song.mp3"]
	require.Equal(t, "Song", a.Title)
	require.Equal(t, uint64(1), a.TypeID)
	require.Equal(t, []uint64{10}, a.PlaylistIDs)
	require.Empty(t, a.Issues)
	b := byPath["library/b (2).mp3"]
	require.Equal(t, "b", b.Title, "title from the file name")
	require.Equal(t, *a.ArtistID, *b.ArtistID, "one artist, whatever the case")
	require.Equal(t, []library.QualityIssue{library.IssueLoudness, library.IssueTruePeak}, b.Issues)

	// Imported files left the inbox; the rest stays.
	require.Equal(t, []string{
		"incoming/.hidden.mp3", "incoming/broken.mp3", "incoming/known.mp3", "incoming/notes.txt",
		"library/a/song.mp3", "library/b (2).mp3", "library/b.mp3",
	}, f.files.paths())
	require.Equal(t, 5, f.ops.progress)
}

func TestImportTwoCopiesInTheInbox(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("incoming/a.mp3", "A", mp3Info("Song", ""), loud(-16, -2))
	f.tools.audio("incoming/b.mp3", "A", mp3Info("Song", ""), loud(-16, -2))
	_, err := f.svc.Import(t.Context(), ImportRequest{TypeID: 1})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "imported 1, duplicates 1", summary)
	require.Len(t, f.store.created(), 1)
	// Which copy wins depends on the workers; one stays in the inbox.
	inbox, _, err := f.files.Walk(t.Context(), "incoming", 10)
	require.NoError(t, err)
	require.Len(t, inbox, 1)
}

func TestImportMovesBackWhenTheTrackIsNotStored(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("incoming/song.mp3", "A", mp3Info("Song", ""), loud(-16, -2))
	f.store.createErr = errors.New("database down")

	_, err := f.svc.Import(t.Context(), ImportRequest{TypeID: 1})
	require.NoError(t, err)
	summary, err := f.runQueued(t)
	require.NoError(t, err)
	require.Equal(t, "failed 1; failed: [incoming/song.mp3] (see the log)", summary)
	require.Equal(t, []string{"incoming/song.mp3"}, f.files.paths())
}

func TestImportRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := f.svc.Import(t.Context(), ImportRequest{TypeID: 9, PlaylistIDs: []uint64{10, 77}})
	var verr *validation.Error
	require.ErrorAs(t, err, &verr)
	require.Equal(t, "type_id", verr.Fields[0].Field)
	require.Equal(t, "playlist_ids", verr.Fields[1].Field)
	require.Empty(t, f.ops.started)

	f.tools.missing = true
	_, err = f.svc.Import(t.Context(), ImportRequest{TypeID: 1})
	require.ErrorIs(t, err, ErrToolsUnavailable)
}

func TestImportFoldersMustNotOverlap(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.set.LibraryDir = "incoming/done"
	_, err := f.svc.Import(t.Context(), ImportRequest{TypeID: 1})
	require.NoError(t, err)
	_, err = f.runQueued(t)
	require.ErrorContains(t, err, "must not contain each other")
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

func TestInbox(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.files.put("incoming/b.mp3", "B")
	f.files.put("incoming/a.mp3", "AA")
	f.files.put("library/c.mp3", "C")
	in, err := f.svc.Inbox(t.Context())
	require.NoError(t, err)
	require.Equal(t, "incoming", in.Dir)
	require.Equal(t, []media.File{
		{Path: "incoming/a.mp3", Size: 2, ModTime: testNow}, {Path: "incoming/b.mp3", Size: 1, ModTime: testNow},
	}, in.Files)
	require.False(t, in.More)
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

func TestOverlap(t *testing.T) {
	t.Parallel()
	require.True(t, overlap("incoming", "incoming"))
	require.True(t, overlap("incoming", "incoming/done"))
	require.True(t, overlap("media/library", "media"))
	require.False(t, overlap("incoming", "incoming2"))
	require.False(t, overlap("incoming", "library"))
}
