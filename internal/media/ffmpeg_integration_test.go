//go:build integration

package media

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The tools against real files: ffmpeg is installed in CI and in the
// runtime image; a development host without it skips.
func TestFFmpegMeasureAndEncode(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "m"), 0o750))
	//nolint:gosec // G204: fixed arguments and a path in a temp dir
	gen := exec.CommandContext(t.Context(), "ffmpeg", "-hide_banner", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=5", "-af", "volume=-18dB",
		"-ac", "1", "-ar", "48000", "-c:a", "pcm_s16le", "-metadata", "title=Tone",
		filepath.Join(root, "m", "tone.wav"))
	out, err := gen.CombinedOutput()
	require.NoError(t, err, string(out))
	lib := New(root)
	ff, prober := NewFFmpeg(lib), NewProber(lib)
	require.True(t, ff.Available())

	info, err := prober.Probe(t.Context(), "m/tone.wav")
	require.NoError(t, err)
	require.Equal(t, FormatWAV, *info.Format)
	require.Equal(t, "pcm_s16le", *info.Codec)
	require.Equal(t, 1, *info.Channels)
	require.Equal(t, "Tone", info.Title)

	before, err := ff.Measure(t.Context(), "m/tone.wav")
	require.NoError(t, err)
	require.Less(t, before.IntegratedLUFS, -25.0)

	out1, ok := OutputFor(FormatMP3, "mp3")
	require.True(t, ok)
	norm := &Normalize{TargetLUFS: -16, TruePeakDBTP: -1, Measured: before}
	require.NoError(t, ff.Encode(t.Context(), "m/tone.wav", "m/.tone.tmp.mp3",
		Encoding{Output: out1, BitrateKbps: 320, SampleRate: 44100, Channels: 2}, norm))
	require.NoError(t, lib.Commit("m/.tone.tmp.mp3", "m/tone.mp3"))

	info, err = prober.Probe(t.Context(), "m/tone.mp3")
	require.NoError(t, err)
	require.Equal(t, FormatMP3, *info.Format)
	require.Equal(t, 44100, *info.SampleRate)
	require.Equal(t, 2, *info.Channels)
	require.Equal(t, "Tone", info.Title, "tags are copied")
	require.InDelta(t, 5000, *info.DurationMS, 100)
	after, err := ff.Measure(t.Context(), "m/tone.mp3")
	require.NoError(t, err)
	require.InDelta(t, -16, after.IntegratedLUFS, 1)
	require.LessOrEqual(t, after.TruePeakDBTP, -0.5)

	// Silence has no loudness.
	//nolint:gosec // G204: fixed arguments and a path in a temp dir
	gen = exec.CommandContext(t.Context(), "ffmpeg", "-hide_banner", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo", "-t", "2", filepath.Join(root, "m", "silence.wav"))
	out, err = gen.CombinedOutput()
	require.NoError(t, err, string(out))
	_, err = ff.Measure(t.Context(), "m/silence.wav")
	require.ErrorIs(t, err, ErrSilent)

	// A file that is not audio.
	require.NoError(t, os.WriteFile(filepath.Join(root, "m", "fake.mp3"), []byte("not audio"), 0o600))
	_, err = ff.Measure(t.Context(), "m/fake.mp3")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrSilent)
}

// The waveform of a real file: a mono tone at half scale (-6 dBFS) for
// 2 s, then 1 s of silence.
func TestFFmpegPeaks(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	root := t.TempDir()
	//nolint:gosec // G204: fixed arguments and a path in a temp dir
	gen := exec.CommandContext(t.Context(), "ffmpeg", "-hide_banner", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "aevalsrc=0.5*sin(2*PI*440*t):s=44100:d=2", "-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono:d=1",
		"-filter_complex", "[0][1]concat=n=2:v=0:a=1",
		"-c:a", "libmp3lame", "-b:a", "192k", filepath.Join(root, "tone.mp3"))
	out, err := gen.CombinedOutput()
	require.NoError(t, err, string(out))
	ff := NewFFmpeg(New(root))

	peaks, duration, err := ff.Peaks(t.Context(), "tone.mp3", 50)
	require.NoError(t, err)
	require.InDelta(t, 3000, duration, 100)
	require.InDelta(t, 2*60, len(peaks), 4)
	// The tone peaks at half scale (-6 dBFS); the silence is flat.
	require.InDelta(t, 63, peaks[21], 6)
	require.InDelta(t, -63, peaks[20], 6)
	require.Zero(t, peaks[len(peaks)-4])

	_, _, err = ff.Peaks(t.Context(), "missing.mp3", 50)
	require.ErrorIs(t, err, ErrFileNotFound)
}
