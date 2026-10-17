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
		{"DJ Ann - Morning", "Morning", "DJ Ann"},
		{"DJ Ann–Morning", "Morning", "DJ Ann"},
		{"DJ Ann — Morning", "Morning", "DJ Ann"},
		{"Aira V - Ты нужен мне любой", "Ты нужен мне любой", "Aira V"},
		{"  DJ Ann   -   Morning  ", "Morning", "DJ Ann"},
		{"Morning", "Morning", ""},
		{"  Morning  ", "Morning", ""},
		{"Jay-Z - Song", "Jay-Z - Song", ""},
		{"A - B - C", "A - B - C", ""},
		{"A – B — C", "A – B — C", ""},
		{"-intro", "-intro", ""},
		{"Morning -", "Morning -", ""},
		{" - ", "-", ""},
		{"Анна - Утро", "Утро", "Анна"},
	}
	for _, tt := range tests {
		title, artist := nameParts(tt.base)
		require.Equal(t, tt.title, title, tt.base)
		require.Equal(t, tt.artist, artist, tt.base)
	}
}
