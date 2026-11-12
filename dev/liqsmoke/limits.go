package main

import "slices"

// encoderLimits is ENCODER_LIMITS of dev/liquidsoap/radio.liq: the highest
// bitrate the encoders accept per codec, channel count, and sample rate,
// where it is below 320 kbps. The smoke test sends both sides of every
// limit, so the two tables cannot drift apart unnoticed.
var encoderLimits = []struct {
	codec            string
	channels, rateHz int
	maxKbps          int
}{
	{"vorbis", 1, 22050, 64},
	{"vorbis", 1, 32000, 160},
	{"vorbis", 1, 44100, 192},
	{"vorbis", 1, 48000, 192},
	{"vorbis", 2, 22050, 160},
}

// bitrates are the values of the setting stream.bitrate_kbps.
var bitrates = []int{64, 96, 128, 160, 192, 256, 320}

// encoderLimit reports whether the script refuses the combination.
func encoderLimit(codec string, channels, rateHz, kbps int) bool {
	for _, l := range encoderLimits {
		if l.codec == codec && l.channels == channels && l.rateHz == rateHz {
			return kbps > l.maxKbps
		}
	}
	return false
}

// testBitrates are the bitrates the smoke test sends for a combination:
// the lowest and the highest, and both sides of its limit.
func testBitrates(codec string, channels, rateHz int) []int {
	out := []int{64, 320}
	for _, l := range encoderLimits {
		if l.codec == codec && l.channels == channels && l.rateHz == rateHz {
			i := slices.Index(bitrates, l.maxKbps)
			out = append(out, l.maxKbps, bitrates[i+1])
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
