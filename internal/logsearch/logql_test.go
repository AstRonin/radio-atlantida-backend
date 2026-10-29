package logsearch

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSelectorHasOnlyJobAndEnumLabels(t *testing.T) {
	t.Parallel()
	require.Equal(t, `{job="backend"}`, selector(Query{}))
	require.Equal(t, `{job="backend",level=~"WARN|ERROR",component="api"}`,
		selector(Query{MinLevel: "WARN", Component: "api"}))
}

func TestUserTextNeverReachesASelector(t *testing.T) {
	t.Parallel()
	// Text that would close the selector and add a matcher if it were
	// concatenated: it must stay inside the message filter.
	q := Query{Search: `"} or {job="x`, RequestID: "a/b-000001"}
	require.Equal(t, `{job="backend"}`, selector(q))
	require.Equal(t, ` | json | request_id="a/b-000001" | msg=~"(?is).*\"\\} or \\{job=\"x.*"`, pipeline(q))
}

func TestSearchIsEscapedAsAMessageRegex(t *testing.T) {
	t.Parallel()
	// Each want is the LogQL literal: the regex text of messageRegex, with
	// its backslashes and quotes escaped once more for the LogQL string.
	tests := []struct {
		in, want string
	}{
		{`plain`, `"(?is).*plain.*"`},
		{`say "hi"`, `"(?is).*say \"hi\".*"`},
		{`a\b`, `"(?is).*a\\\\b.*"`},
		{`a.b`, `"(?is).*a\\.b.*"`},
		{`f(x)*`, `"(?is).*f\\(x\\)\\*.*"`},
		{`[a]+?|^$`, `"(?is).*\\[a\\]\\+\\?\\|\\^\\$.*"`},
		{"line\nbreak\ttab", `"(?is).*line\nbreak\ttab.*"`},
		{`\"`, `"(?is).*\\\\\".*"`},
	}
	for _, tt := range tests {
		require.Equal(t, " | json | msg=~"+tt.want, pipeline(Query{Search: tt.in}), tt.in)
	}
}

func TestSearchAloneParsesJsonBeforeTheMessageFilter(t *testing.T) {
	t.Parallel()
	require.Equal(t, ` | json | msg=~"(?is).*heartbeat.*"`, pipeline(Query{Search: "heartbeat"}))
}

func TestRequestIDIsAFieldFilterAfterJson(t *testing.T) {
	t.Parallel()
	require.Equal(t, ` | json | request_id="ab+/==/x-000042"`, pipeline(Query{RequestID: "ab+/==/x-000042"}))
}

func TestLineExprCombinesSelectorAndPipeline(t *testing.T) {
	t.Parallel()
	q := Query{MinLevel: "INFO", Component: "outage", Search: "heartbeat", RequestID: "r-1"}
	require.Equal(t,
		`{job="backend",level=~"INFO|WARN|ERROR",component="outage"} | json | request_id="r-1" | msg=~"(?is).*heartbeat.*"`,
		lineExpr(q))
}

func TestCountExprCountsTheRange(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	q := Query{From: from, To: from.Add(90 * time.Minute), Component: "api"}
	require.Equal(t,
		`sum(count_over_time({job="backend",component="api"} [5400000ms]))`,
		countExpr(q))
}

func TestDurationMillisRoundsUp(t *testing.T) {
	t.Parallel()
	require.Equal(t, "1ms", durationMillis(time.Nanosecond))
	require.Equal(t, "2ms", durationMillis(1500*time.Microsecond))
	require.Equal(t, "60000ms", durationMillis(time.Minute))
}

func TestCountWindowCoversFromAndStopsBeforeTo(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	q := Query{From: from, To: from.Add(time.Hour)}

	// Loki's window is (at-range, at]. The window must contain from and must
	// not contain to.
	at := countAt(q)
	rng := q.To.Sub(q.From)
	require.Equal(t, "3600000ms", durationMillis(rng))
	require.True(t, at.Add(-rng).Before(from), "from is inside the window")
	require.False(t, at.Add(-rng).Before(from.Add(-time.Nanosecond)), "nothing before from is inside")
	require.True(t, at.Before(q.To), "to is outside the window")
}
