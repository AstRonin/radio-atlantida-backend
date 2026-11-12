package main

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/streamformat"
)

// The test stack runs the images of the dev stack, so the smoke test checks
// the script with the Liquidsoap and Icecast the stack uses.
func TestImagePins(t *testing.T) {
	t.Parallel()
	images := func(path string) []string {
		b, err := os.ReadFile(path) //nolint:gosec // G304: the two compose files of this repository
		require.NoError(t, err)
		var out []string
		for _, m := range regexp.MustCompile(`(?m)^\s+image:\s*(\S+)\s*$`).FindAllStringSubmatch(string(b), -1) {
			out = append(out, m[1])
		}
		return out
	}
	dev := images("../../docker-compose.dev.yml")
	smoke := images("compose.yml")
	require.Contains(t, smoke, "savonet/liquidsoap:v2.4.6")
	for _, img := range smoke {
		if regexp.MustCompile(`^nginxinc/nginx-unprivileged:`).MatchString(img) {
			continue // the TLS proxy exists only here
		}
		require.Contains(t, dev, img, "dev/liqsmoke/compose.yml pins %s; docker-compose.dev.yml does not", img)
	}
}

func TestTestBitrates(t *testing.T) {
	t.Parallel()
	require.Equal(t, []int{64, 320}, testBitrates("mp3", 2, 44100))
	require.Equal(t, []int{64, 96, 320}, testBitrates("vorbis", 1, 22050))
	require.Equal(t, []int{64, 160, 192, 320}, testBitrates("vorbis", 2, 22050))
	require.False(t, refused(output{Codec: "vorbis", Channels: 2, SampleRateHz: 22050, BitrateKbps: 160}))
	require.True(t, refused(output{Codec: "vorbis", Channels: 2, SampleRateHz: 22050, BitrateKbps: 192}))
}

// The encoder limits of the script (ENCODER_LIMITS in radio.liq) are the
// table of internal/streamformat, so the settings check refuses what the
// script refuses, and nothing else.
func TestEncoderLimitsMatchScript(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../liquidsoap/radio.liq")
	require.NoError(t, err)
	list := regexp.MustCompile(`(?s)\nencoder_limits\s*=\s*\[(.*?)\]`).FindSubmatch(b)
	require.NotNil(t, list, "encoder_limits list not found in radio.liq")
	var script []streamformat.Limit
	for _, m := range regexp.MustCompile(`\(\s*"(\w+)"\s*,\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*\)`).FindAllSubmatch(list[1], -1) {
		n := func(s []byte) int {
			v, err := strconv.Atoi(string(s))
			require.NoError(t, err)
			return v
		}
		script = append(script, streamformat.Limit{Codec: string(m[1]), Channels: n(m[2]), SampleRateHz: n(m[3]), MaxKbps: n(m[4])})
	}
	require.NotEmpty(t, script)
	require.ElementsMatch(t, streamformat.Limits(), script)
}
