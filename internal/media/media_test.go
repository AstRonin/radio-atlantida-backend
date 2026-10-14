package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidatePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		ok   bool
	}{
		{"song.mp3", true},
		{"music/2026/song one.mp3", true},
		{"music/..song.mp3", true},
		{"", false},
		{".", false},
		{"/music/song.mp3", false},
		{"C:/music/song.mp3", false},
		{"c:song.mp3", false},
		{`music\song.mp3`, false},
		{"../song.mp3", false},
		{"music/../../song.mp3", false},
		{"music/./song.mp3", false},
		{"music//song.mp3", false},
		{"music/", false},
		{"music/song\x00.mp3", false},
		{strings.Repeat("a", MaxPathLength+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			err := ValidatePath(tt.path)
			if tt.ok {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, ErrInvalidPath)
		})
	}
}

func TestOpen(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "music"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "music", "song.mp3"), []byte("audio"), 0o600))
	outside := filepath.Join(t.TempDir(), "secret.mp3")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "music", "escape.mp3")))
	require.NoError(t, os.Symlink("song.mp3", filepath.Join(root, "music", "inside.mp3")))
	lib := New(root)

	for _, ok := range []string{"music/song.mp3", "music/inside.mp3"} {
		f, err := lib.Open(ok)
		require.NoError(t, err, ok)
		require.NoError(t, f.Close())
	}

	tests := map[string]error{
		"music/missing.mp3": ErrFileNotFound,
		"music":             ErrFileNotFound, // a directory
		"music/escape.mp3":  ErrFileNotFound, // a link out of the root (pins the os.OpenInRoot error text)
		"../secret.mp3":     ErrInvalidPath,
	}
	for path, want := range tests {
		_, err := lib.Open(path)
		require.ErrorIs(t, err, want, path)
	}
}

func TestContentType(t *testing.T) {
	t.Parallel()
	require.Equal(t, "audio/mpeg", ContentType(FormatMP3, "x.bin"))
	require.Equal(t, "audio/ogg", ContentType(FormatOgg, "x.opus"))
	require.Equal(t, "audio/flac", ContentType(FormatFLAC, "x"))
	require.Equal(t, "audio/mpeg", ContentType("", "music/x.mp3"), "from the extension")
	require.Equal(t, "application/octet-stream", ContentType("", "music/x"))
	for _, f := range Formats() {
		require.Contains(t, contentTypes, f)
	}
}

func TestParseProbe(t *testing.T) {
	t.Parallel()
	mp3 := `{"streams":[{"codec_type":"audio","sample_rate":"44100"}],
		"format":{"format_name":"mp3","duration":"180.026122","bit_rate":"128000"}}`
	info, err := parseProbe([]byte(mp3))
	require.NoError(t, err)
	require.Equal(t, 180026, *info.DurationMS)
	require.Equal(t, FormatMP3, *info.Format)
	require.Equal(t, 128000, *info.Bitrate)
	require.Equal(t, 44100, *info.SampleRate)

	// Cover art comes first; the sample rate is from the audio stream.
	m4a := `{"streams":[{"codec_type":"video"},{"codec_type":"audio","sample_rate":"48000"}],
		"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"N/A"}}`
	info, err = parseProbe([]byte(m4a))
	require.NoError(t, err)
	require.Equal(t, FormatM4A, *info.Format)
	require.Equal(t, 48000, *info.SampleRate)
	require.Nil(t, info.DurationMS)
	require.Nil(t, info.Bitrate)

	unknown := `{"streams":[{"codec_type":"audio"}],"format":{"format_name":"matroska,webm"}}`
	info, err = parseProbe([]byte(unknown))
	require.NoError(t, err)
	require.Nil(t, info.Format)

	_, err = parseProbe([]byte(`{"streams":[{"codec_type":"video"}],"format":{"format_name":"png_pipe"}}`))
	require.ErrorContains(t, err, "no audio stream")
	_, err = parseProbe([]byte(`not json`))
	require.Error(t, err)
}

// The testdata files are the real output of ffprobe 9.0 (linuxserver/ffmpeg
// 9.0-cli-ls84, the image of dev/scripts/gen-media.sh) with the arguments
// of Probe, for a stereo MP3 with tags in the container and a mono Ogg
// Vorbis file with a tag in the stream.
func TestParseProbeRealOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		file       string
		format     string
		codec      string
		channels   int
		durationMS int
		bitrate    int
		title      string
		artist     string
	}{
		{"ffprobe-mp3.json", FormatMP3, "mp3", 2, 3000, 130634, "Test Tone", "Test Artist"},
		{"ffprobe-ogg.json", FormatOgg, "vorbis", 1, 2000, 27732, "Station ID", ""},
	}
	for _, tt := range tests {
		out, err := os.ReadFile(filepath.Join("testdata", tt.file))
		require.NoError(t, err)
		info, err := parseProbe(out)
		require.NoError(t, err, tt.file)
		require.Equal(t, tt.format, *info.Format, tt.file)
		require.Equal(t, tt.codec, *info.Codec, tt.file)
		require.Equal(t, tt.channels, *info.Channels, tt.file)
		require.Equal(t, tt.durationMS, *info.DurationMS, tt.file)
		require.Equal(t, tt.bitrate, *info.Bitrate, tt.file)
		require.Equal(t, 44100, *info.SampleRate, tt.file)
		require.Equal(t, tt.title, info.Title, tt.file)
		require.Equal(t, tt.artist, info.Artist, tt.file)
	}
}

func TestProbeWithoutFFprobe(t *testing.T) {
	t.Parallel()
	p := &Prober{lib: New(t.TempDir())}
	_, err := p.Probe(t.Context(), "song.mp3")
	require.ErrorIs(t, err, ErrProbeUnavailable)
}
