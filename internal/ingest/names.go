package ingest

import (
	"path"
	"strings"
	"unicode/utf8"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
)

// isHyphen reports the hyphens of a file name: the hyphen-minus, the en
// dash, and the em dash, because names copied from the web use all three.
func isHyphen(r rune) bool {
	return r == '-' || r == '–' || r == '—'
}

// nameParts reads the title and the artist from a file's base name
// without the extension (docs/phase-18-plan.md, decision 6a). A name with
// exactly one hyphen and text on both sides is "Artist - Title"; any other
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
	artist, title = strings.TrimSpace(base[:at]), strings.TrimSpace(base[at+size:])
	if title == "" || artist == "" {
		return whole, ""
	}
	return title, artist
}

// NameSource is where an import takes the title or the artist of a new
// track from.
type NameSource string

// Name sources.
const (
	// SourceTag: the title or artist tag of the file.
	SourceTag NameSource = "tag"
	// SourceFileName: the file name (nameParts).
	SourceFileName NameSource = "file_name"
	// SourceNone: no artist (the file has no artist tag, and its name is
	// not "Artist - Title").
	SourceNone NameSource = "none"
)

// Names are the title and the artist an import gives a file, and where
// each comes from. The artist_id of an import request, when set, wins over
// Artist.
type Names struct {
	Title        string
	TitleSource  NameSource
	Artist       string
	ArtistSource NameSource
}

// fileNames is the file-name part of the import's naming rule: the title
// and the artist read from the base name of rel without its extension,
// cleaned and cut as the import stores them. The import listing shows them
// for a new file without reading it; importNames falls back to them.
func fileNames(rel string) (title, artist string) {
	base := path.Base(rel)
	t, a := nameParts(strings.TrimSuffix(base, path.Ext(base)))
	return truncate(library.CleanTitle(t), library.MaxTitleLength), truncate(a, maxArtistLength)
}

// importNames returns the title and the artist an import gives the file at
// rel whose probe is info (docs/phase-18-plan.md, decision 6a), each
// decided on its own: the title from the tag, else from the file name; the
// artist from the tag, else the file name, else none. The title rule
// (docs/phase-21-plan.md §2, decision 5) removes what the API refuses:
// control characters and runs of white space.
func importNames(rel string, info media.Info) Names {
	nameTitle, nameArtist := fileNames(rel)
	n := Names{Title: nameTitle, TitleSource: SourceFileName, Artist: nameArtist, ArtistSource: SourceFileName}
	if t := library.CleanTitle(info.Title); t != "" {
		n.Title, n.TitleSource = truncate(t, library.MaxTitleLength), SourceTag
	}
	switch a := strings.TrimSpace(info.Artist); {
	case a != "":
		n.Artist, n.ArtistSource = truncate(a, maxArtistLength), SourceTag
	case nameArtist == "":
		n.ArtistSource = SourceNone
	}
	return n
}
