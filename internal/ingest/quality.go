package ingest

import (
	"math"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
)

// Measurements are what the analysis reads from a file. A field is nil
// when the tools do not report it; the loudness fields are nil for a
// silent file.
type Measurements struct {
	DurationMS *int
	// Format is a media.Format* constant; Codec is the ffprobe codec name.
	Format *string
	Codec  *string
	// Bitrate is in bits per second.
	Bitrate         *int
	SampleRate      *int
	Channels        *int
	LoudnessLUFS    *float64
	TruePeakDBTP    *float64
	LoudnessRangeLU *float64
	// SourceBitrate is the bitrate of the lossy file this one was
	// transcoded from; nil for an original.
	SourceBitrate *int
}

func measurementsOf(info media.Info, l *media.Loudness, source *int) Measurements {
	m := Measurements{
		DurationMS: info.DurationMS, Format: info.Format, Codec: info.Codec,
		Bitrate: info.Bitrate, SampleRate: info.SampleRate, Channels: info.Channels,
		SourceBitrate: source,
	}
	if l != nil {
		m.LoudnessLUFS = new(l.IntegratedLUFS)
		m.TruePeakDBTP = new(l.TruePeakDBTP)
		m.LoudnessRangeLU = new(l.RangeLU)
	}
	return m
}

// lossless reports whether the file stores its audio without loss.
func (m Measurements) lossless() bool { return m.Codec != nil && media.LosslessCodec(*m.Codec) }

// Standard is the broadcast standard of track files (settings import.*
// and stream.loudness_target_lufs).
type Standard struct {
	// Format is a media.Format* constant, Codec the ffprobe codec name
	// (mp3, aac, vorbis).
	Format      string
	Codec       string
	BitrateKbps int
	SampleRate  int
	Channels    int
	// MinBitrateKbps is the lowest acceptable bitrate of a lossy file.
	MinBitrateKbps      int
	LoudnessLUFS        float64
	LoudnessToleranceLU float64
	MaxTruePeakDBTP     float64
}

// StandardFormat returns the container of a target codec (setting
// import.target_codec): mp3 in MP3, aac in M4A, vorbis in Ogg.
func StandardFormat(codec string) string {
	switch codec {
	case "aac":
		return media.FormatM4A
	case "vorbis":
		return media.FormatOgg
	default:
		return media.FormatMP3
	}
}

// output is how ffmpeg writes a file of the standard.
func (s Standard) output() (media.Output, bool) { return media.OutputFor(s.Format, s.Codec) }

// Check returns the quality issues of a file with m against std, in the
// order of library.QualityIssues (decision 5 of docs/phase-11-plan.md).
func Check(m Measurements, std Standard) []library.QualityIssue {
	out := []library.QualityIssue{}
	if m.Format == nil || *m.Format != std.Format || (m.Codec != nil && *m.Codec != std.Codec) {
		out = append(out, library.IssueFormat)
	}
	if m.SampleRate == nil || *m.SampleRate != std.SampleRate {
		out = append(out, library.IssueSampleRate)
	}
	if m.Channels == nil || *m.Channels != std.Channels {
		out = append(out, library.IssueChannels)
	}
	if m.LoudnessLUFS != nil && math.Abs(*m.LoudnessLUFS-std.LoudnessLUFS) > std.LoudnessToleranceLU {
		out = append(out, library.IssueLoudness)
	}
	if m.TruePeakDBTP != nil && *m.TruePeakDBTP > std.MaxTruePeakDBTP {
		out = append(out, library.IssueTruePeak)
	}
	if b, ok := m.effectiveBitrate(); ok && b < std.MinBitrateKbps*1000 {
		out = append(out, library.IssueLowBitrate)
	}
	return out
}

// effectiveBitrate is the bitrate that limits the quality of the file: the
// lowest of its own (when lossy) and that of the lossy file it was made
// from. ok is false when neither is known.
func (m Measurements) effectiveBitrate() (int, bool) {
	var b *int
	if !m.lossless() {
		b = m.Bitrate
	}
	if m.SourceBitrate != nil && (b == nil || *m.SourceBitrate < *b) {
		b = m.SourceBitrate
	}
	if b == nil {
		return 0, false
	}
	return *b, true
}

// standardBitratesKbps are the bitrates a lossy file is written with when
// only its loudness is fixed: its own, rounded up to one of these.
var standardBitratesKbps = []int{64, 96, 128, 160, 192, 224, 256, 320}

// roundUpBitrate returns the smallest standard bitrate not below bps (bits
// per second), at most 320 kbps; 0 bps gives 320.
func roundUpBitrate(bps int) int {
	for _, k := range standardBitratesKbps {
		if bps > 0 && k*1000 >= bps {
			return k
		}
	}
	return standardBitratesKbps[len(standardBitratesKbps)-1]
}
