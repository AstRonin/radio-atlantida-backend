package logsearch

import (
	"regexp"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

// selector returns the stream selector of q. Its labels come only from
// validated enums and the backend's own job label: user text never goes
// into a label matcher (logs-loki-plan §4.2).
func selector(q Query) string {
	matchers := []string{"job=" + quote(logging.LokiJob)}
	if q.MinLevel != "" {
		matchers = append(matchers, "level=~"+quote(levelRegex(q.MinLevel)))
	}
	if q.Component != "" {
		matchers = append(matchers, "component="+quote(q.Component))
	}
	return "{" + strings.Join(matchers, ",") + "}"
}

// pipeline returns the field filters of q. The lines are parsed as JSON once;
// the request ID is compared with its field, and the search text with the
// decoded msg field (so a quote in the text matches the message's quote, not
// its escaped form in the raw line).
func pipeline(q Query) string {
	var b strings.Builder
	if q.RequestID != "" || q.Search != "" {
		b.WriteString(" | json")
	}
	if q.RequestID != "" {
		b.WriteString(" | request_id=" + quote(q.RequestID))
	}
	if q.Search != "" {
		b.WriteString(" | msg=~" + quote(messageRegex(q.Search)))
	}
	return b.String()
}

// messageRegex returns the regular expression that matches a msg containing
// search as plain text, case-insensitively. Label matchers in Loki are fully
// anchored, so the text is wrapped in .* on both sides; (?s) lets .* cross a
// line break inside the message.
func messageRegex(search string) string {
	return "(?is).*" + regexp.QuoteMeta(search) + ".*"
}

// lineExpr is the log query of a page: the lines of q, newest first when
// Loki returns them backward.
func lineExpr(q Query) string {
	return selector(q) + pipeline(q)
}

// countExpr counts the lines of q as a single number. Loki evaluates a range
// as (at-range, at]: countAt(q) is the instant that makes the window [from,
// to), and the range covers from to the millisecond.
func countExpr(q Query) string {
	return "sum(count_over_time(" + selector(q) + pipeline(q) + " [" + durationMillis(q.To.Sub(q.From)) + "]))"
}

// countAt is the instant of the count: the last nanosecond before to, so
// that a line at to is not counted (the window is closed at its end).
func countAt(q Query) time.Time { return q.To.Add(-time.Nanosecond) }
