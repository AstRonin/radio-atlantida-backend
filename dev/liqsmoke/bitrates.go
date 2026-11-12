package main

import (
	"slices"

	"github.com/AstRonin/radio-atlantida-backend/internal/streamformat"
)

// bitrates are the values of the setting stream.bitrate_kbps.
var bitrates = []int{64, 96, 128, 160, 192, 256, 320}

// testBitrates are the bitrates the smoke test sends for a combination:
// the lowest and the highest, and both sides of its encoder limit
// (internal/streamformat, ENCODER_LIMITS of dev/liquidsoap/radio.liq).
func testBitrates(codec string, channels, rateHz int) []int {
	out := []int{64, 320}
	for _, l := range streamformat.Limits() {
		if l.Codec == codec && l.Channels == channels && l.SampleRateHz == rateHz {
			i := slices.Index(bitrates, l.MaxKbps)
			out = append(out, l.MaxKbps, bitrates[i+1])
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
