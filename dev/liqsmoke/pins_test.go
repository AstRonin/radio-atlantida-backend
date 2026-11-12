package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
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
	require.False(t, encoderLimit("vorbis", 2, 22050, 160))
	require.True(t, encoderLimit("vorbis", 2, 22050, 192))
}
