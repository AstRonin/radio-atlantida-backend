package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"1.0.0", Version{1, 0, 0}, true},
		{"0.0.1", Version{0, 0, 1}, true},
		{"10.20.300", Version{10, 20, 300}, true},
		{"dev", Version{}, false},
		{"v1.0.0", Version{}, false},
		{"1.0", Version{}, false},
		{"1.0.0.0", Version{}, false},
		{"1.0.0-rc1", Version{}, false},
		{"01.0.0", Version{}, false},
		{"1..0", Version{}, false},
		{"1.-1.0", Version{}, false},
		{"1.+1.0", Version{}, false},
		{"v1.0.0-3-gabcdef-dirty", Version{}, false},
		{"", Version{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseVersion(tt.in)
			if !tt.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.in, got.String())
		})
	}
}

func TestVersionCompare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.10", "1.0.9", 1},
		{"1.4.0", "1.3.9", 1},
		{"1.9.0", "1.10.0", -1},
		{"2.0.0", "1.99.99", 1},
	}
	for _, tt := range tests {
		a, err := ParseVersion(tt.a)
		require.NoError(t, err)
		b, err := ParseVersion(tt.b)
		require.NoError(t, err)
		require.Equal(t, tt.want, a.Compare(b), "%s vs %s", tt.a, tt.b)
		require.Equal(t, -tt.want, b.Compare(a), "%s vs %s", tt.b, tt.a)
	}
}

func TestStatusCompatible(t *testing.T) {
	t.Parallel()
	st := Status{SchemaVersion: 30, MinAppVersion: "1.4.0"}
	tests := []struct {
		version string
		want    bool
	}{
		{"1.3.9", false},
		{"1.0.0", false},
		{"1.4.0", true},
		{"1.4.1", true},
		{"2.0.0", true},
		// Not a release: a developer's build.
		{"dev", true},
		{"v1.0.0-5-gabcdef", true},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, st.Compatible(tt.version), tt.version)
	}
	require.False(t, Status{MinAppVersion: "bad"}.Compatible("1.0.0"))
}
