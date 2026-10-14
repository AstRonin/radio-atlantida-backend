package ingest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
)

func testStandard() Standard {
	return Standard{
		Format: media.FormatMP3, Codec: "mp3", BitrateKbps: 320, SampleRate: 44100, Channels: 2,
		MinBitrateKbps: 128, LoudnessLUFS: -16, LoudnessToleranceLU: 2, MaxTruePeakDBTP: -1,
	}
}

// goodMP3 meets testStandard.
func goodMP3() Measurements {
	return Measurements{
		DurationMS: new(180000), Format: new(media.FormatMP3), Codec: new("mp3"),
		Bitrate: new(320000), SampleRate: new(44100), Channels: new(2),
		LoudnessLUFS: new(-16.0), TruePeakDBTP: new(-1.5), LoudnessRangeLU: new(6.0),
	}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(m *Measurements)
		want   []library.QualityIssue
	}{
		{"meets the standard", func(*Measurements) {}, nil},
		{"other container", func(m *Measurements) { m.Format, m.Codec = new(media.FormatOgg), new("vorbis") }, []library.QualityIssue{library.IssueFormat}},
		{"aac in m4a against mp3", func(m *Measurements) { m.Format, m.Codec = new(media.FormatM4A), new("aac") }, []library.QualityIssue{library.IssueFormat}},
		{"unknown format", func(m *Measurements) { m.Format = nil }, []library.QualityIssue{library.IssueFormat}},
		{"48 kHz", func(m *Measurements) { m.SampleRate = new(48000) }, []library.QualityIssue{library.IssueSampleRate}},
		{"mono", func(m *Measurements) { m.Channels = new(1) }, []library.QualityIssue{library.IssueChannels}},
		{"loudness at the tolerance", func(m *Measurements) { m.LoudnessLUFS = new(-18.0) }, nil},
		{"too quiet", func(m *Measurements) { m.LoudnessLUFS = new(-18.1) }, []library.QualityIssue{library.IssueLoudness}},
		{"too loud", func(m *Measurements) { m.LoudnessLUFS = new(-13.5) }, []library.QualityIssue{library.IssueLoudness}},
		{"silent: no loudness issues", func(m *Measurements) { m.LoudnessLUFS, m.TruePeakDBTP = nil, nil }, nil},
		{"true peak at the limit", func(m *Measurements) { m.TruePeakDBTP = new(-1.0) }, nil},
		{"true peak above", func(m *Measurements) { m.TruePeakDBTP = new(-0.2) }, []library.QualityIssue{library.IssueTruePeak}},
		{"bitrate at the minimum", func(m *Measurements) { m.Bitrate = new(128000) }, nil},
		{"bitrate below", func(m *Measurements) { m.Bitrate = new(96000) }, []library.QualityIssue{library.IssueLowBitrate}},
		{"carried from the source", func(m *Measurements) { m.SourceBitrate = new(96000) }, []library.QualityIssue{library.IssueLowBitrate}},
		{
			"lossless: own bitrate does not count", func(m *Measurements) {
				m.Format, m.Codec, m.Bitrate = new(media.FormatFLAC), new("flac"), new(900)
			},
			[]library.QualityIssue{library.IssueFormat},
		},
		{
			"lossless made from a low lossy source", func(m *Measurements) {
				m.Format, m.Codec, m.SourceBitrate = new(media.FormatWAV), new("pcm_s16le"), new(64000)
			},
			[]library.QualityIssue{library.IssueFormat, library.IssueLowBitrate},
		},
		{
			"everything, in code order", func(m *Measurements) {
				m.Format, m.Codec = new(media.FormatWAV), new("pcm_s16le")
				m.SampleRate, m.Channels = new(22050), new(1)
				m.LoudnessLUFS, m.TruePeakDBTP, m.SourceBitrate = new(-9.0), new(0.5), new(64000)
			},
			[]library.QualityIssue{
				library.IssueFormat, library.IssueSampleRate, library.IssueChannels,
				library.IssueLoudness, library.IssueTruePeak, library.IssueLowBitrate,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := goodMP3()
			tt.change(&m)
			want := tt.want
			if want == nil {
				want = []library.QualityIssue{}
			}
			require.Equal(t, want, Check(m, testStandard()))
		})
	}
}

func TestStandardFormat(t *testing.T) {
	t.Parallel()
	for codec, format := range map[string]string{"mp3": media.FormatMP3, "aac": media.FormatM4A, "vorbis": media.FormatOgg} {
		require.Equal(t, format, StandardFormat(codec))
		out, ok := Standard{Format: format, Codec: codec}.output()
		require.True(t, ok, codec)
		require.False(t, out.Lossless, codec)
	}
}

func TestRoundUpBitrate(t *testing.T) {
	t.Parallel()
	tests := map[int]int{0: 320, 1: 64, 64000: 64, 64001: 96, 130517: 160, 320000: 320, 500000: 320}
	for bps, want := range tests {
		require.Equal(t, want, roundUpBitrate(bps), bps)
	}
}

func TestFixes(t *testing.T) {
	t.Parallel()
	require.Empty(t, library.FixesFor(nil))
	require.Empty(t, library.FixesFor([]library.QualityIssue{library.IssueLowBitrate, library.IssueFileMissing}))
	require.Equal(t, []library.Fix{library.FixOptimize}, library.FixesFor([]library.QualityIssue{library.IssueChannels}))
	require.Equal(t, []library.Fix{library.FixLoudness, library.FixOptimize},
		library.FixesFor([]library.QualityIssue{library.IssueLowBitrate, library.IssueTruePeak}))
}
