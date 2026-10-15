package ingest

import (
	"strings"
	"unicode/utf8"
)

// isHyphen reports the hyphens of a file name: the hyphen-minus, the en
// dash, and the em dash, because names copied from the web use all three.
func isHyphen(r rune) bool {
	return r == '-' || r == '–' || r == '—'
}

// nameParts reads the title and the artist from a file's base name
// without the extension (docs/phase-18-plan.md, decision 6a). A name with
// exactly one hyphen and text on both sides is "Title - Artist"; any other
// name is the title alone, trimmed, and gives no artist: with several
// hyphens ("Jay-Z - Song") the split is not obvious.
func nameParts(base string) (title, artist string) {
	whole := strings.TrimSpace(base)
	at, hyphens := -1, 0
	for i, r := range base {
		if isHyphen(r) {
			at = i
			hyphens++
		}
	}
	if hyphens != 1 {
		return whole, ""
	}
	_, size := utf8.DecodeRuneInString(base[at:])
	title, artist = strings.TrimSpace(base[:at]), strings.TrimSpace(base[at+size:])
	if title == "" || artist == "" {
		return whole, ""
	}
	return title, artist
}
