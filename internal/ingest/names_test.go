package ingest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNameParts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		base, title, artist string
	}{
		{"Morning - DJ Ann", "Morning", "DJ Ann"},
		{"Morning–DJ Ann", "Morning", "DJ Ann"},
		{"Morning — DJ Ann", "Morning", "DJ Ann"},
		{"  Morning   -   DJ Ann  ", "Morning", "DJ Ann"},
		{"Morning", "Morning", ""},
		{"  Morning  ", "Morning", ""},
		{"Jay-Z - Song", "Jay-Z - Song", ""},
		{"A - B - C", "A - B - C", ""},
		{"A – B — C", "A – B — C", ""},
		{"-intro", "-intro", ""},
		{"Morning -", "Morning -", ""},
		{" - ", "-", ""},
		{"Утро - Анна", "Утро", "Анна"},
	}
	for _, tt := range tests {
		title, artist := nameParts(tt.base)
		require.Equal(t, tt.title, title, tt.base)
		require.Equal(t, tt.artist, artist, tt.base)
	}
}
