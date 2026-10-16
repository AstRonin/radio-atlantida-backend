package liquidsoap_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
)

func TestAnnotate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"plain", "Song", `Song`},
		{"double quote", `Say "hi"`, `Say \"hi\"`},
		{"backslash", `AC\DC`, `AC\\DC`},
		{"quote after backslash", `a\"b`, `a\\\"b`},
		{"newline and carriage return", "one\r\ntwo", `onetwo`},
		{"tab and other controls", "a\tb\x00c\x7fd\u0085e", `abcde`},
		{"separators stay inside the quotes", `x",title="y:z`, `x\",title=\"y:z`},
		{"Cyrillic", "Привет, мир", `Привет, мир`},
		{"Armenian", "Բարև", `Բարև`},
		{"emoji", "Hit 🎵🔥", `Hit 🎵🔥`},
		{"invalid UTF-8", "a\xffb", `ab`},
		{"empty", "", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := liquidsoap.Annotate("/media/a.mp3", liquidsoap.Pair{Key: "title", Value: tt.value})
			require.Equal(t, `annotate:title="`+tt.want+`":/media/a.mp3`, got)
			require.NotContains(t, got, "\n")
		})
	}
}

func TestAnnotateSeveralAndNone(t *testing.T) {
	t.Parallel()
	require.Equal(t, `annotate:item_id="7",title="T",artist="":/media/a.mp3`,
		liquidsoap.Annotate("/media/a.mp3",
			liquidsoap.Pair{Key: "item_id", Value: "7"},
			liquidsoap.Pair{Key: "title", Value: "T"},
			liquidsoap.Pair{Key: "artist", Value: ""}))
	require.Equal(t, "/media/a.mp3", liquidsoap.Annotate("/media/a.mp3"))
}
