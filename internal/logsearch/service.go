package logsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"time"
)

// Deadlines of the calls to Loki (logs-loki-plan §4.2). The count is a second
// call with a shorter deadline: when it misses it, the page is still shown.
const (
	queryTimeout = 5 * time.Second
	countTimeout = 3 * time.Second
)

// Entry is one log line of a page.
type Entry struct {
	Time      time.Time
	Level     string
	Message   string
	Component string
	RequestID string
	Error     string
	// Attrs holds the other fields of the line.
	Attrs map[string]any
}

// Page is one page of entries, newest first.
type Page struct {
	Items []Entry
	// NextCursor resumes the search after this page. Empty on the last page.
	NextCursor string
	// Total is the number of lines that match the query over the whole
	// range. Nil when the count failed or missed its deadline.
	Total *int64
}

// Service searches the log lines.
type Service struct {
	loki Loki
	log  *slog.Logger
}

// NewService returns a search over loki. log is the logger of the caller's
// component; it logs a failed count, the one failure that is not returned.
func NewService(loki Loki, log *slog.Logger) *Service {
	return &Service{loki: loki, log: log}
}

// Search returns one page of the lines that match q. A query that breaks a
// rule returns a *validation.Error. A Loki that does not answer returns
// ErrUnavailable, and a range Loki refuses returns ErrRangeTooLong.
func (s *Service) Search(ctx context.Context, q Query) (Page, error) {
	if err := q.Validate(); err != nil {
		return Page{}, err
	}
	limit := q.pageSize()

	// The page is the newest lines before end. A cursor moves end back to the
	// time of the last entry of the previous page (Loki's end is exclusive).
	end := q.To
	if q.Cursor != "" {
		t, err := decodeCursor(q.Cursor)
		if err != nil {
			return Page{}, fmt.Errorf("resume search: %w", err)
		}
		if t.Before(end) {
			end = t
		}
	}

	page := Page{Items: []Entry{}}
	if end.After(q.From) {
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		streams, err := s.loki.QueryRange(qctx, RangeQuery{
			Expr: lineExpr(q), Start: q.From, End: end,
			// One more than the page tells whether a next page exists.
			Limit: limit + 1,
		})
		cancel()
		if err != nil {
			return Page{}, fmt.Errorf("search log lines: %w", err)
		}
		items := toEntries(streams)
		if len(items) > limit {
			items = items[:limit]
			// Entries with exactly the same time as the last one of the page
			// are not resumed past; log lines carry nanoseconds, so this is
			// rare.
			page.NextCursor = encodeCursor(items[limit-1].Time)
		}
		page.Items = items
	}

	// The total covers the whole range, not the rest after the cursor, so
	// it is the same on every page.
	cctx, cancel := context.WithTimeout(ctx, countTimeout)
	n, err := s.loki.Count(cctx, CountQuery{Expr: countExpr(q), At: countAt(q)})
	cancel()
	if err != nil {
		s.log.WarnContext(ctx, "count of log lines failed; the page has no total", slog.String("err", err.Error()))
	} else {
		page.Total = &n
	}
	return page, nil
}

// toEntries turns the lines of all streams into entries, newest first.
func toEntries(streams []Stream) []Entry {
	var out []Entry
	for _, st := range streams {
		for _, ln := range st.Lines {
			out = append(out, parseEntry(ln, st.Labels["level"]))
		}
	}
	slices.SortStableFunc(out, func(a, b Entry) int { return b.Time.Compare(a.Time) })
	return out
}

// parseEntry reads a JSON line of the logger. A line that is not JSON keeps
// its text as the message and the level label of its stream.
func parseEntry(ln Line, level string) Entry {
	e := Entry{Time: ln.Time, Level: level, Message: ln.Text}
	var raw map[string]any
	if err := json.Unmarshal([]byte(ln.Text), &raw); err != nil {
		return e
	}
	str := func(key string) string {
		v, _ := raw[key].(string)
		delete(raw, key)
		return v
	}
	if l := str("level"); l != "" {
		e.Level = l
	}
	e.Message = str("msg")
	e.Component = str("component")
	e.RequestID = str("request_id")
	e.Error = str("err")
	delete(raw, "time")
	if len(raw) > 0 {
		e.Attrs = raw
	}
	return e
}
