package liquidsoap

import (
	"strings"
	"unicode"
)

// Pair is one annotation of a request: a metadata key and its value.
type Pair struct {
	// Key is a fixed identifier chosen by the code (item_id, title,
	// liq_cue_out); it is not quoted.
	Key   string
	Value string
}

// Annotate returns the request for uri with the annotations of pairs:
// annotate:key="value",...:uri (the annotate protocol). Every value is
// quoted, with backslashes and double quotes escaped and control
// characters removed, so a title cannot end the value, break the command
// line, or add an annotation. Without pairs it returns uri.
//
// Annotations win over the tags of the file (checked with the pinned
// Liquidsoap on the dev stack, docs/phase-21-plan.md §3.1): an empty value
// hides the tag.
func Annotate(uri string, pairs ...Pair) string {
	if len(pairs) == 0 {
		return uri
	}
	var b strings.Builder
	b.WriteString("annotate:")
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p.Key)
		b.WriteString(`="`)
		writeQuoted(&b, p.Value)
		b.WriteByte('"')
	}
	b.WriteByte(':')
	b.WriteString(uri)
	return b.String()
}

// writeQuoted writes the inside of a quoted annotation value. Invalid UTF-8
// is dropped with the control characters: the command line goes to
// Liquidsoap as UTF-8 text.
func writeQuoted(b *strings.Builder, v string) {
	for _, r := range strings.ToValidUTF8(v, "") {
		switch {
		case r == '\\' || r == '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
}
