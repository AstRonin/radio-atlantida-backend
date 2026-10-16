package media

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func pcm(samples ...int16) *bytes.Reader {
	var b bytes.Buffer
	for _, s := range samples {
		_ = binary.Write(&b, binary.LittleEndian, s)
	}
	return bytes.NewReader(b.Bytes())
}

func repeat(v int16, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestPeaksOf(t *testing.T) {
	t.Parallel()
	// rate 1000 Hz and 10 ms buckets: 10 samples per bucket.
	tests := []struct {
		name     string
		samples  []int16
		trailing bool
		want     []int8
		duration int
	}{
		{name: "empty", want: nil, duration: 0},
		{name: "silence", samples: repeat(0, 20), want: []int8{0, 0, 0, 0}, duration: 20},
		{
			name:    "full scale",
			samples: append(append(repeat(32767, 5), repeat(-32768, 5)...), repeat(16384, 10)...),
			want:    []int8{-127, 127, 63, 63}, duration: 20,
		},
		{
			name: "bucket edges", samples: append(append(repeat(0, 9), 1000), append([]int16{-1000}, repeat(0, 9)...)...),
			want: []int8{0, 3, -3, 0}, duration: 20,
		},
		{name: "last partial bucket", samples: append(repeat(0, 10), 32767, -32767, 0), want: []int8{0, 0, -127, 127}, duration: 13},
		{name: "odd trailing byte ignored", samples: repeat(32767, 3), trailing: true, want: []int8{127, 127}, duration: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := pcm(tc.samples...)
			src := r
			if tc.trailing {
				b := make([]byte, r.Len())
				_, _ = r.Read(b)
				src = bytes.NewReader(append(b, 0x7f))
			}
			peaks, duration, err := PeaksOf(src, 1000, 10)
			require.NoError(t, err)
			require.Equal(t, tc.want, peaks)
			require.Equal(t, tc.duration, duration)
		})
	}
}

// 22050 Hz and 50 ms buckets do not divide evenly: buckets alternate 1102
// and 1103 samples, and five minutes give 6,000 buckets.
func TestPeaksOfUnevenBuckets(t *testing.T) {
	t.Parallel()
	peaks, duration, err := PeaksOf(pcm(repeat(100, 22050*300)...), 22050, 50)
	require.NoError(t, err)
	require.Len(t, peaks, 2*6000)
	require.Equal(t, 300_000, duration)
}

func TestPeaksOfRefusesBadArguments(t *testing.T) {
	t.Parallel()
	_, _, err := PeaksOf(pcm(), 0, 50)
	require.Error(t, err)
}
