// Package logsearch searches the backend's log lines in Loki for the Logs
// page (logs-loki-plan §4.2). It validates a query, builds the LogQL, pages
// with an opaque cursor, and counts the matches. It has no HTTP types: the
// handler decodes the request and encodes the page.
package logsearch

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Limits of a search.
const (
	// DefaultLimit is the page size when the query does not set one.
	DefaultLimit = 100
	// MaxLimit caps a page. It stays well below Loki's default maximum of
	// 5000 lines per query.
	MaxLimit = 500
	// MaxSearchLength caps the text of a line filter, in characters.
	MaxSearchLength = 200
)

// levelNames are the levels the logger writes, lowest first.
var levelNames = logging.LevelNames()

// requestIDRe matches the request IDs of the HTTP middleware: the hostname
// and a base64 prefix (which may hold +, / and =), then a counter.
var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9+/=._-]{1,200}$`)

// Query is a search as the manager made it. The zero value of a field means
// "not set".
type Query struct {
	// From and To bound the search: From is inclusive, To is exclusive. From
	// must be before To.
	From, To time.Time
	// MinLevel keeps entries at this level or above: DEBUG, INFO, WARN or
	// ERROR.
	MinLevel string
	// Component keeps the entries of one component (see logging.Components).
	Component string
	// Search keeps the lines whose msg contains this text (1 to 200
	// characters), case-insensitively. It is a plain text match, never a
	// regular expression.
	Search string
	// RequestID keeps the entries of one request.
	RequestID string
	// Limit is the page size, 1 to 500. Zero means DefaultLimit.
	Limit int
	// Cursor is the NextCursor of the previous page; empty for the first.
	Cursor string
}

// Validate returns the rules the query breaks as a *validation.Error, or nil.
// Field names are the query.<parameter> names of GET /api/logs, the form the
// other endpoints use.
func (q Query) Validate() error {
	var e validation.Error
	if q.From.IsZero() {
		e.Add("query.from", validation.Required, "from is required")
	}
	if q.To.IsZero() {
		e.Add("query.to", validation.Required, "to is required")
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.From.Before(q.To) {
		e.Add("query.to", validation.Order, "to must be after from")
	}
	if q.MinLevel != "" && !slices.Contains(levelNames, q.MinLevel) {
		e.Add("query.level", validation.Enum, "level must be one of %s", strings.Join(levelNames, ", "))
	}
	if q.Component != "" && !slices.Contains(componentNames(), q.Component) {
		e.Add("query.component", validation.Enum, "component is not a logging component")
	}
	if n := utf8.RuneCountInString(q.Search); n > MaxSearchLength {
		e.Add("query.search", validation.Length, "search is %d characters; the maximum is %d", n, MaxSearchLength)
	}
	if q.RequestID != "" && !requestIDRe.MatchString(q.RequestID) {
		e.Add("query.request_id", validation.Pattern, "request_id has characters that a request ID does not have")
	}
	if q.Limit != 0 && (q.Limit < 1 || q.Limit > MaxLimit) {
		e.Add("query.limit", validation.Range, "limit must be between 1 and %d", MaxLimit)
	}
	if q.Cursor != "" {
		if _, err := decodeCursor(q.Cursor); err != nil {
			e.Add("query.cursor", validation.Invalid, "cursor is not one returned by this API")
		}
	}
	return e.Err()
}

// pageSize returns the page size, with the default applied.
func (q Query) pageSize() int {
	if q.Limit == 0 {
		return DefaultLimit
	}
	return q.Limit
}

// levelRegex matches the level label of the minimum level and every level
// above it, for example WARN|ERROR.
func levelRegex(minLevel string) string {
	i := slices.Index(levelNames, minLevel)
	return strings.Join(levelNames[i:], "|")
}

// componentNames lists the components a search may name.
func componentNames() []string {
	var names []string
	for _, c := range logging.Components() {
		names = append(names, string(c))
	}
	return names
}

// quote renders s as a LogQL string literal. Go's quoting escapes the
// backslash, the double quote and every control character, which LogQL
// reads the same way.
func quote(s string) string { return strconv.Quote(s) }

// durationMillis renders d as a LogQL range, rounded up to whole
// milliseconds (Loki reads ms, not ns).
func durationMillis(d time.Duration) string {
	ms := int64((d + time.Millisecond - 1) / time.Millisecond)
	return fmt.Sprintf("%dms", max(ms, 1))
}
