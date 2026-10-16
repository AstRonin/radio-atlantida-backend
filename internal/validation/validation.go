// Package validation provides the error a domain service returns when a
// request breaks a rule that the API schema cannot express (a blank name, a
// path outside the media root, a name already taken). The API turns it into
// per-field errors with stable codes (§6.2 of the design).
package validation

import (
	"fmt"
	"strings"
)

// Rule names the rule a field breaks. The API maps it to a field error code.
type Rule string

// Rules of a FieldError.
const (
	// Required: the field is missing or blank.
	Required Rule = "required"
	// Range: a number is outside its allowed range.
	Range Rule = "range"
	// Format: a value is not in the required form.
	Format Rule = "format"
	// Taken: another entity already uses the value.
	Taken Rule = "taken"
	// UnknownID: the field refers to an entity that does not exist.
	UnknownID Rule = "unknown_id"
	// Invalid: any other problem with the value.
	Invalid Rule = "invalid"
	// Overlap: the value collides with another one (two fixed clock
	// elements whose start windows overlap).
	Overlap Rule = "overlap"
	// DoesNotFit: the content does not fit into its time (material before
	// a fixed clock element, a fixed element past the end of the hour).
	DoesNotFit Rule = "does_not_fit"
	// Order: the items are out of the required order (fixed clock elements
	// not in minute order).
	Order Rule = "order"
	// Blocked: the track breaks separation or a play rule at the time it
	// would play.
	Blocked Rule = "blocked"
	// Length: a text is too long once cleaned (a title over 500
	// characters).
	Length Rule = "length"
	// InvalidCharacters: a text contains characters it must not have
	// (control characters such as line breaks and tabs in a title).
	InvalidCharacters Rule = "invalid_characters"
)

// FieldError is the error of one request field.
type FieldError struct {
	// Field is the path of the field in the request body, for example
	// window_to or playlist_ids.
	Field   string
	Rule    Rule
	Message string
}

// Error is a refused request with one error per broken field.
type Error struct {
	Fields []FieldError
}

func (e *Error) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Message
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// Add records a broken field.
func (e *Error) Add(field string, rule Rule, format string, args ...any) {
	e.Fields = append(e.Fields, FieldError{Field: field, Rule: rule, Message: fmt.Sprintf(format, args...)})
}

// Err returns e as an error, or nil when no field is broken.
func (e *Error) Err() error {
	if len(e.Fields) == 0 {
		return nil
	}
	return e
}

// Field returns an error with one broken field.
func Field(field string, rule Rule, format string, args ...any) error {
	e := &Error{}
	e.Add(field, rule, format, args...)
	return e
}
