package logsearch

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// fakeLoki answers from fixed data and records the calls.
type fakeLoki struct {
	streams  []Stream
	queryErr error
	count    int64
	countErr error

	queries []RangeQuery
	counts  []CountQuery
	// deadlines of the calls, to check the per-call limits.
	queryDeadline, countDeadline time.Time
}

func (f *fakeLoki) QueryRange(ctx context.Context, q RangeQuery) ([]Stream, error) {
	f.queries = append(f.queries, q)
	f.queryDeadline, _ = ctx.Deadline()
	return f.streams, f.queryErr
}

func (f *fakeLoki) Count(ctx context.Context, q CountQuery) (int64, error) {
	f.counts = append(f.counts, q)
	f.countDeadline, _ = ctx.Deadline()
	return f.count, f.countErr
}

// jsonLine is a line as the logger writes it.
func jsonLine(ts time.Time, level, msg, component string) string {
	s := fmt.Sprintf(`{"time":%q,"level":%q,"msg":%q`, ts.Format(time.RFC3339Nano), level, msg)
	if component != "" {
		s += fmt.Sprintf(`,"component":%q`, component)
	}
	return s + "}"
}

func line(ts time.Time, text string) Line { return Line{Time: ts, Text: text} }

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestSearchFirstPageNewestFirstAcrossStreams(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	fl := &fakeLoki{
		streams: []Stream{
			{Labels: map[string]string{"level": "INFO"}, Lines: []Line{
				line(base.Add(3*time.Second), jsonLine(base.Add(3*time.Second), "INFO", "c", "api")),
				line(base.Add(1*time.Second), jsonLine(base.Add(time.Second), "INFO", "a", "api")),
			}},
			{Labels: map[string]string{"level": "ERROR"}, Lines: []Line{
				line(base.Add(2*time.Second), jsonLine(base.Add(2*time.Second), "ERROR", "b", "db")),
			}},
		},
		count: 3,
	}
	svc := NewService(fl, discard())

	page, err := svc.Search(t.Context(), Query{From: base, To: base.Add(time.Hour), Limit: 10})

	require.NoError(t, err)
	require.Empty(t, page.NextCursor, "3 lines fit one page of 10")
	require.Len(t, page.Items, 3)
	require.Equal(t, []string{"c", "b", "a"}, []string{page.Items[0].Message, page.Items[1].Message, page.Items[2].Message})
	require.Equal(t, "db", page.Items[1].Component)
	require.NotNil(t, page.Total)
	require.Equal(t, int64(3), *page.Total)

	require.Len(t, fl.queries, 1)
	q := fl.queries[0]
	require.Equal(t, base, q.Start)
	require.Equal(t, base.Add(time.Hour), q.End)
	require.Equal(t, 11, q.Limit, "one more than the page, to know whether there is a next page")
	require.Equal(t, `{job="backend"}`, q.Expr)
	require.WithinDuration(t, time.Now().Add(queryTimeout), fl.queryDeadline, 10*time.Second)
	require.WithinDuration(t, time.Now().Add(countTimeout), fl.countDeadline, 10*time.Second)
}

func TestSearchNextCursorResumesBeforeLastEntry(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	var lines []Line
	for i := 5; i >= 1; i-- { // Loki returns backward: newest first
		ts := base.Add(time.Duration(i) * time.Second)
		lines = append(lines, line(ts, jsonLine(ts, "INFO", fmt.Sprintf("m%d", i), "api")))
	}
	fl := &fakeLoki{streams: []Stream{{Labels: map[string]string{"level": "INFO"}, Lines: lines}}}
	svc := NewService(fl, discard())

	page, err := svc.Search(t.Context(), Query{From: base, To: base.Add(time.Hour), Limit: 2})

	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, "m5", page.Items[0].Message)
	require.Equal(t, "m4", page.Items[1].Message)
	require.NotEmpty(t, page.NextCursor)
	cursorTime, err := decodeCursor(page.NextCursor)
	require.NoError(t, err)
	require.True(t, base.Add(4*time.Second).Equal(cursorTime), "the last returned entry's time")
	require.NotNil(t, page.Total)
	require.Zero(t, *page.Total, "the fake count is zero")
}

func TestSearchNextPageEndsBeforeCursor(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	cursorAt := base.Add(4 * time.Second)
	fl := &fakeLoki{streams: []Stream{{Lines: []Line{
		line(base.Add(2*time.Second), jsonLine(base.Add(2*time.Second), "INFO", "m2", "api")),
	}}}}
	svc := NewService(fl, discard())

	page, err := svc.Search(t.Context(), Query{From: base, To: base.Add(time.Hour), Limit: 5, Cursor: encodeCursor(cursorAt)})

	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Empty(t, page.NextCursor, "the last page")
	require.True(t, cursorAt.Equal(fl.queries[0].End), "the page ends at the cursor, exclusive")
	require.Equal(t, base, fl.queries[0].Start)
}

func TestSearchCursorAfterToKeepsTo(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	fl := &fakeLoki{}
	svc := NewService(fl, discard())

	_, err := svc.Search(t.Context(), Query{From: base, To: base.Add(time.Hour), Cursor: encodeCursor(base.Add(9 * time.Hour))})

	require.NoError(t, err)
	require.True(t, base.Add(time.Hour).Equal(fl.queries[0].End))
}

func TestSearchCursorAtOrBeforeFromSkipsLoki(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	fl := &fakeLoki{count: 7}
	svc := NewService(fl, discard())

	page, err := svc.Search(t.Context(), Query{From: base, To: base.Add(time.Hour), Cursor: encodeCursor(base)})

	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Empty(t, fl.queries, "nothing is left before the cursor")
	require.NotNil(t, page.Total, "the total is still the whole range")
	require.Equal(t, int64(7), *page.Total)
}

func TestSearchOmitsTotalWhenCountFails(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	fl := &fakeLoki{
		streams:  []Stream{{Lines: []Line{line(base, jsonLine(base, "INFO", "shown", "api"))}}},
		countErr: fmt.Errorf("%w: slow", ErrUnavailable),
	}
	svc := NewService(fl, discard())

	page, err := svc.Search(t.Context(), Query{From: base, To: base.Add(time.Hour)})

	require.NoError(t, err, "the entries are still shown")
	require.Len(t, page.Items, 1)
	require.Nil(t, page.Total)
}

func TestSearchPassesLokiErrors(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	for _, sentinel := range []error{ErrUnavailable, ErrRangeTooLong} {
		fl := &fakeLoki{queryErr: fmt.Errorf("loki: %w", sentinel)}
		svc := NewService(fl, discard())

		_, err := svc.Search(t.Context(), Query{From: base, To: base.Add(time.Hour)})

		require.ErrorIs(t, err, sentinel)
		require.Empty(t, fl.counts, "no total when the lines failed")
	}
}

func TestSearchValidatesBeforeCallingLoki(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	fl := &fakeLoki{}
	svc := NewService(fl, discard())

	_, err := svc.Search(t.Context(), Query{From: base, To: base.Add(-time.Hour)})

	var ve *validation.Error
	require.ErrorAs(t, err, &ve)
	require.Empty(t, fl.queries)
	require.Empty(t, fl.counts)
}

func TestSearchRefusesCursorsItDidNotIssue(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	svc := NewService(&fakeLoki{}, discard())

	_, err := svc.Search(t.Context(), Query{From: base, To: base.Add(time.Hour), Cursor: "nope"})

	var ve *validation.Error
	require.ErrorAs(t, err, &ve)
}

func TestParseEntryReadsStandardFieldsAndKeepsRest(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	e := parseEntry(line(ts, `{"time":"2026-10-27T10:00:00Z","level":"ERROR","msg":"failed","component":"icecast","request_id":"r1","err":"boom","status":503}`), "ERROR")
	require.Equal(t, ts, e.Time)
	require.Equal(t, "ERROR", e.Level)
	require.Equal(t, "failed", e.Message)
	require.Equal(t, "icecast", e.Component)
	require.Equal(t, "r1", e.RequestID)
	require.Equal(t, "boom", e.Error)
	require.Equal(t, map[string]any{"status": float64(503)}, e.Attrs)
}

func TestParseEntryKeepsTextOfAForeignLine(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	e := parseEntry(line(ts, "plain text"), "WARN")
	require.Equal(t, "WARN", e.Level)
	require.Equal(t, "plain text", e.Message)
	require.Nil(t, e.Attrs)
}
