package ingest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

func TestFileInfoNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, path          string
		titleTag, artistTag string
		want                Names
	}{
		{
			"tags win", "music/DJ Ann - Morning.mp3", "  Real\tTitle ", " Real Artist ",
			Names{Title: "Real Title", TitleSource: SourceTag, Artist: "Real Artist", ArtistSource: SourceTag},
		},
		{
			"file name fallback", "music/DJ Ann - Morning.mp3", "", "",
			Names{Title: "Morning", TitleSource: SourceFileName, Artist: "DJ Ann", ArtistSource: SourceFileName},
		},
		{
			"each on its own", "music/DJ Ann - Morning.mp3", "Tagged", "",
			Names{Title: "Tagged", TitleSource: SourceTag, Artist: "DJ Ann", ArtistSource: SourceFileName},
		},
		{
			"no artist", "music/Jay-Z - Song.mp3", "", "",
			Names{Title: "Jay-Z - Song", TitleSource: SourceFileName, ArtistSource: SourceNone},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.tools.audio(tt.path, "A", mp3Info(tt.titleTag, tt.artistTag), nil)
			got, err := f.svc.FileInfo(t.Context(), tt.path)
			require.NoError(t, err)
			require.Equal(t, tt.want, got.Names)
			require.Equal(t, importNames(tt.path, mp3Info(tt.titleTag, tt.artistTag)), got.Names, "the rule of the import")
		})
	}
}

func TestFileInfo(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	info := media.Info{
		DurationMS: new(3000), Format: new(media.FormatMP3), Codec: new("mp3"),
		Bitrate: new(64000), SampleRate: new(22050), Channels: new(1), Title: "Low Tone",
	}
	f.tools.audio("music/talk/low.mp3", "LOW", info, loud(-40, -20))

	got, err := f.svc.FileInfo(t.Context(), "music/talk/low.mp3")
	require.NoError(t, err)
	require.Equal(t, media.File{Path: "music/talk/low.mp3", Size: 3, ModTime: testNow}, got.File)
	require.Equal(t, info, got.Probe)
	require.Equal(t, []library.QualityIssue{library.IssueSampleRate, library.IssueChannels, library.IssueLowBitrate}, got.Issues,
		"no loudness or true peak without a decode")

	f.tools.audio("music/good.mp3", "GOOD", mp3Info("", ""), nil)
	got, err = f.svc.FileInfo(t.Context(), "music/good.mp3")
	require.NoError(t, err)
	require.Empty(t, got.Issues)
	require.NotNil(t, got.Issues)
	f.ops.mu.Lock()
	require.Empty(t, f.ops.started, "not a media operation")
	f.ops.mu.Unlock()
}

func TestFileInfoRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.files.put("music/notes.txt", "N")
	f.files.put("music/broken.mp3", "B") // no probe result: not audio
	f.files.put("music/song.mp3.part", "P")

	for _, p := range []string{"", "../a.mp3", "/a.mp3", `music\a.mp3`, "music/./a.mp3", ".trash/a.mp3", "music/.a.mp3"} {
		var verr *validation.Error
		require.ErrorAs(t, fileInfoErr(t, f, p), &verr, p)
		require.Equal(t, "path", verr.Fields[0].Field, p)
		require.Equal(t, validation.Invalid, verr.Fields[0].Rule, p)
	}
	for _, p := range []string{"music/notes.txt", "music/song.mp3.part", "music"} {
		var verr *validation.Error
		require.ErrorAs(t, fileInfoErr(t, f, p), &verr, p)
		require.Equal(t, validation.NotAudio, verr.Fields[0].Rule, p)
	}
	require.ErrorIs(t, fileInfoErr(t, f, "music/missing.mp3"), ErrFileNotFound)
	require.ErrorIs(t, fileInfoErr(t, f, "music/broken.mp3"), ErrProbeFailed)

	// A media operation runs: the probe neither waits nor is refused.
	f.svc.running.Store(true)
	f.tools.audio("music/ok.mp3", "OK", mp3Info("", ""), nil)
	_, err := f.svc.FileInfo(t.Context(), "music/ok.mp3")
	require.NoError(t, err)
	f.svc.running.Store(false)

	f.tools.missing = true
	require.ErrorIs(t, fileInfoErr(t, f, "music/ok.mp3"), ErrToolsUnavailable)
}

func TestFileInfoNoDurationIsProbeFailed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	info := mp3Info("", "")
	info.DurationMS = nil
	f.tools.audio("music/a.mp3", "A", info, nil)
	require.ErrorIs(t, fileInfoErr(t, f, "music/a.mp3"), ErrProbeFailed, "an import skips it as unreadable")
}

func TestFileInfoWaitsForASlot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("music/a.mp3", "A", mp3Info("", ""), nil)
	for range fileInfoProbes {
		f.svc.probes <- struct{}{}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.svc.FileInfo(ctx, "music/a.mp3")
	require.ErrorIs(t, err, context.Canceled)
	<-f.svc.probes
	_, err = f.svc.FileInfo(t.Context(), "music/a.mp3")
	require.NoError(t, err, "a free slot")
}

// fileInfoErr returns the error of FileInfo for p.
func fileInfoErr(t *testing.T, f *fixture, p string) error {
	t.Helper()
	_, err := f.svc.FileInfo(t.Context(), p)
	return err
}
