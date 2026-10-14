package media

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseProbeTagsCodecChannels(t *testing.T) {
	t.Parallel()
	// MP3: tags in the container.
	mp3 := `{"streams":[{"codec_type":"audio","codec_name":"mp3","sample_rate":"44100","channels":2}],
		"format":{"format_name":"mp3","duration":"3.0","bit_rate":"128000","tags":{"title":" Song ","artist":"ABBA"}}}`
	info, err := parseProbe([]byte(mp3))
	require.NoError(t, err)
	require.Equal(t, "mp3", *info.Codec)
	require.Equal(t, 2, *info.Channels)
	require.Equal(t, "Song", info.Title)
	require.Equal(t, "ABBA", info.Artist)

	// Ogg: Vorbis comments, upper case, in the stream.
	ogg := `{"streams":[{"codec_type":"audio","codec_name":"vorbis","sample_rate":"48000","channels":1,
		"tags":{"TITLE":"Jingle","ARTIST":"Station"}}],"format":{"format_name":"ogg","duration":"2.0"}}`
	info, err = parseProbe([]byte(ogg))
	require.NoError(t, err)
	require.Equal(t, "vorbis", *info.Codec)
	require.Equal(t, 1, *info.Channels)
	require.Equal(t, "Jingle", info.Title)
	require.Equal(t, "Station", info.Artist)

	// No tags, no channel count.
	info, err = parseProbe([]byte(`{"streams":[{"codec_type":"audio"}],"format":{"format_name":"wav"}}`))
	require.NoError(t, err)
	require.Empty(t, info.Title)
	require.Nil(t, info.Channels)
	require.Nil(t, info.Codec)
}

func TestLosslessCodec(t *testing.T) {
	t.Parallel()
	for codec, want := range map[string]bool{
		"flac": true, "alac": true, "pcm_s16le": true, "pcm_s24le": true, "mp3": false, "aac": false, "vorbis": false, "": false,
	} {
		require.Equal(t, want, LosslessCodec(codec), codec)
	}
}

// loudnormStderr is the end of ffmpeg's stderr after a loudnorm first pass
// (ffmpeg 9.0, print_format=json).
const loudnormStderr = `Input #0, mp3, from 'file:/media/song.mp3':
  Duration: 00:00:03.00, start: 0.025057, bitrate: 130 kb/s
[Parsed_loudnorm_0 @ 0x55d2c1c8e2c0]
{
	"input_i" : "-23.54",
	"input_tp" : "-4.25",
	"input_lra" : "1.30",
	"input_thresh" : "-33.68",
	"output_i" : "-23.98",
	"output_tp" : "-6.08",
	"output_lra" : "1.10",
	"output_thresh" : "-34.08",
	"normalization_type" : "dynamic",
	"target_offset" : "-0.02"
}
`

func TestParseLoudnorm(t *testing.T) {
	t.Parallel()
	l, err := parseLoudnorm([]byte(loudnormStderr))
	require.NoError(t, err)
	require.Equal(t, Loudness{IntegratedLUFS: -23.54, TruePeakDBTP: -4.25, RangeLU: 1.3, ThresholdLUFS: -33.68, Offset: -0.02}, l)

	silent := `{ "input_i" : "-inf", "input_tp" : "-inf", "input_lra" : "0.00", "input_thresh" : "-70.00", "target_offset" : "inf" }`
	_, err = parseLoudnorm([]byte(silent))
	require.ErrorIs(t, err, ErrSilent)

	_, err = parseLoudnorm([]byte("Error opening input"))
	require.ErrorContains(t, err, "no loudnorm output")
	_, err = parseLoudnorm([]byte("} {"))
	require.Error(t, err)
}

func TestOutputFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		format, codec string
		want          Output
		ok            bool
	}{
		{FormatMP3, "mp3", Output{Encoder: "libmp3lame", Muxer: "mp3", Ext: ".mp3"}, true},
		{FormatM4A, "aac", Output{Encoder: "aac", Muxer: "ipod", Ext: ".m4a"}, true},
		{FormatM4A, "alac", Output{Encoder: "alac", Muxer: "ipod", Ext: ".m4a", Lossless: true}, true},
		{FormatAAC, "aac", Output{Encoder: "aac", Muxer: "adts", Ext: ".aac"}, true},
		{FormatOgg, "vorbis", Output{Encoder: "libvorbis", Muxer: "ogg", Ext: ".ogg"}, true},
		{FormatOgg, "opus", Output{Encoder: "libopus", Muxer: "ogg", Ext: ".ogg"}, true},
		{FormatFLAC, "flac", Output{Encoder: "flac", Muxer: "flac", Ext: ".flac", Lossless: true}, true},
		{FormatWAV, "pcm_s24le", Output{Encoder: "pcm_s24le", Muxer: "wav", Ext: ".wav", Lossless: true}, true},
		{FormatWAV, "adpcm_ms", Output{}, false},
		{FormatOgg, "speex", Output{}, false},
		{"", "mp3", Output{}, false},
	}
	for _, tt := range tests {
		got, ok := OutputFor(tt.format, tt.codec)
		require.Equal(t, tt.ok, ok, tt.format+"/"+tt.codec)
		require.Equal(t, tt.want, got, tt.format+"/"+tt.codec)
	}
}

func TestEncodeArgs(t *testing.T) {
	t.Parallel()
	mp3, _ := OutputFor(FormatMP3, "mp3")
	norm := &Normalize{TargetLUFS: -16, TruePeakDBTP: -1, Measured: Loudness{
		IntegratedLUFS: -23.54, TruePeakDBTP: -4.25, RangeLU: 1.3, ThresholdLUFS: -33.68, Offset: -0.02,
	}}
	got := encodeArgs("/m/a.wav", "/m/.a.tmp.mp3", Encoding{Output: mp3, BitrateKbps: 320, SampleRate: 44100, Channels: 2}, norm)
	require.Equal(t, []string{
		"-hide_banner", "-nostdin", "-nostats", "-v", "error", "-y", "-threads", "1", "-filter_threads", "1",
		"-i", "file:/m/a.wav", "-map", "0:a:0", "-map_metadata", "0",
		"-af", "loudnorm=I=-16.00:TP=-1.00:LRA=7.00:measured_I=-23.54:measured_TP=-4.25:measured_LRA=1.30:measured_thresh=-33.68:offset=-0.02:linear=true",
		"-c:a", "libmp3lame", "-b:a", "320k", "-ar", "44100", "-ac", "2", "-id3v2_version", "3",
		"-f", "mp3", "file:/m/.a.tmp.mp3",
	}, got)

	flac, _ := OutputFor(FormatFLAC, "flac")
	got = encodeArgs("/m/a.flac", "/m/b.flac", Encoding{Output: flac, BitrateKbps: 320, SampleRate: 48000, Channels: 1}, nil)
	require.NotContains(t, got, "-b:a", "lossless takes no bitrate")
	require.NotContains(t, got, "-af")
	require.Equal(t, []string{"-c:a", "flac", "-ar", "48000", "-ac", "1", "-f", "flac", "file:/m/b.flac"}, got[len(got)-9:])
}

func TestLoudnormFilterKeepsWideRangesLinear(t *testing.T) {
	t.Parallel()
	f := loudnormFilter(Normalize{TargetLUFS: -16, TruePeakDBTP: -1, Measured: Loudness{RangeLU: 14.2}})
	require.Contains(t, f, ":LRA=15.00:")
	f = loudnormFilter(Normalize{Measured: Loudness{RangeLU: 80}})
	require.Contains(t, f, ":LRA=50.00:")
}

func TestFFmpegWithoutBinary(t *testing.T) {
	t.Parallel()
	f := &FFmpeg{lib: New(t.TempDir())}
	require.False(t, f.Available())
	_, err := f.Measure(t.Context(), "a.mp3")
	require.ErrorIs(t, err, ErrFFmpegUnavailable)
	require.ErrorIs(t, f.Encode(t.Context(), "a.mp3", "b.mp3", Encoding{}, nil), ErrFFmpegUnavailable)
}

func TestLastLine(t *testing.T) {
	t.Parallel()
	require.Equal(t, "file:/x: No such file or directory", lastLine("banner\nmore\nfile:/x: No such file or directory\n"))
	require.Len(t, lastLine(string(make([]byte, 1000))), 300)
}
