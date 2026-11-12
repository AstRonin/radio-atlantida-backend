// Package streamformat holds the encoder limits of the pinned Liquidsoap
// (savonet/liquidsoap:v2.4.6, design §8.8): the stream formats whose
// encoder raises when the output starts, which makes Liquidsoap exit. The
// settings check refuses them at save (cmd/backend), the script refuses
// them on output.set (ENCODER_LIMITS in dev/liquidsoap/radio.liq), and
// dev/liqsmoke sends both sides of every limit. A unit test in dev/liqsmoke
// compares this table with the script's list.
package streamformat

import (
	"fmt"
	"slices"
)

// Limit is the highest bitrate an encoder accepts for one codec, channel
// count, and sample rate.
type Limit struct {
	Codec        string
	Channels     int
	SampleRateHz int
	MaxKbps      int
}

// limits lists the combinations whose limit is below 320 kbps, the highest
// value of the setting stream.bitrate_kbps. Found by running every
// combination of the settings against the pinned image (2026-11-12).
var limits = []Limit{
	{"vorbis", 1, 22050, 64},
	{"vorbis", 1, 32000, 160},
	{"vorbis", 1, 44100, 192},
	{"vorbis", 1, 48000, 192},
	{"vorbis", 2, 22050, 160},
}

// Limits returns every limit.
func Limits() []Limit { return slices.Clone(limits) }

// Check returns the limit the combination exceeds, or nil when the encoder
// accepts it.
func Check(codec string, channels, sampleRateHz, bitrateKbps int) *Limit {
	for _, l := range limits {
		if l.Codec == codec && l.Channels == channels && l.SampleRateHz == sampleRateHz {
			if bitrateKbps > l.MaxKbps {
				return &l
			}
			return nil
		}
	}
	return nil
}

// String describes the limit for the manager, for example "Vorbis mono at
// 22050 Hz allows at most 64 kbps".
func (l Limit) String() string {
	return fmt.Sprintf("%s %s at %d Hz allows at most %d kbps", codecName(l.Codec), channelsName(l.Channels), l.SampleRateHz, l.MaxKbps)
}

func codecName(codec string) string {
	switch codec {
	case "mp3":
		return "MP3"
	case "aac":
		return "AAC"
	case "vorbis":
		return "Vorbis"
	default:
		return codec
	}
}

func channelsName(n int) string {
	switch n {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	default:
		return fmt.Sprintf("%d channels", n)
	}
}
