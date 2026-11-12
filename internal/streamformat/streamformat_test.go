package streamformat_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/streamformat"
)

func TestCheck(t *testing.T) {
	t.Parallel()
	// Every limit is allowed at its maximum and refused above it.
	for _, l := range streamformat.Limits() {
		require.Nil(t, streamformat.Check(l.Codec, l.Channels, l.SampleRateHz, l.MaxKbps), "%+v at the maximum", l)
		got := streamformat.Check(l.Codec, l.Channels, l.SampleRateHz, l.MaxKbps+1)
		require.NotNil(t, got, "%+v above the maximum", l)
		require.Equal(t, l, *got)
	}
	// Other combinations allow the highest bitrate of the settings.
	for _, codec := range []string{"mp3", "aac", "vorbis"} {
		for _, ch := range []int{1, 2} {
			for _, sr := range []int{22050, 32000, 44100, 48000} {
				if codec == "vorbis" && (ch == 1 || sr == 22050) {
					continue
				}
				require.Nil(t, streamformat.Check(codec, ch, sr, 320), "%s %d ch %d Hz", codec, ch, sr)
			}
		}
	}
	require.Len(t, streamformat.Limits(), 5)
}

func TestLimitString(t *testing.T) {
	t.Parallel()
	require.Equal(t, "Vorbis mono at 22050 Hz allows at most 64 kbps",
		streamformat.Check("vorbis", 1, 22050, 128).String())
	require.Equal(t, "Vorbis stereo at 22050 Hz allows at most 160 kbps",
		streamformat.Check("vorbis", 2, 22050, 192).String())
}

// Limits returns a copy: a caller cannot change the table.
func TestLimitsIsACopy(t *testing.T) {
	t.Parallel()
	l := streamformat.Limits()
	l[0].MaxKbps = 320
	require.NotNil(t, streamformat.Check("vorbis", 1, 22050, 128))
}
