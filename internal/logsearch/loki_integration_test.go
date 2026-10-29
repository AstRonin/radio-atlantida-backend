//go:build integration

package logsearch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/logsearch"
)

// lokiImage is the pinned Loki of the dev stack (docker-compose.dev.yml).
const lokiImage = "grafana/loki:3.7.8"

// startLoki starts Loki with the dev stack's configuration and returns its
// base URL. The container is removed when the test ends.
func startLoki(t *testing.T) (string, *testcontainers.DockerContainer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	cfg, err := filepath.Abs("../../dev/loki/loki.yaml")
	require.NoError(t, err)
	ctr, err := testcontainers.Run(ctx, lokiImage,
		testcontainers.WithExposedPorts("3100/tcp"),
		testcontainers.WithCmd("-config.file=/etc/loki/loki.yaml"),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath: cfg, ContainerFilePath: "/etc/loki/loki.yaml", FileMode: 0o644,
		}),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/ready").WithPort("3100/tcp").WithStartupTimeout(90*time.Second)),
	)
	if ctr != nil {
		testcontainers.CleanupContainer(t, ctr)
	}
	require.NoError(t, err)
	endpoint, err := ctr.PortEndpoint(ctx, "3100/tcp", "http")
	require.NoError(t, err)
	return endpoint, ctr // PortEndpoint with the "http" protocol includes the scheme
}

// pushLine is one log line to push, labelled as the backend's sink labels it.
type pushLine struct {
	at                      time.Time
	level, component, reqID string
	msg                     string
}

// push sends the lines through Loki's push API, one stream per label set.
func push(t *testing.T, base string, lines []pushLine) {
	t.Helper()
	type stream struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	}
	index := map[[2]string]int{}
	var streams []stream
	for _, l := range lines {
		entry := map[string]string{
			"time": l.at.Format(time.RFC3339Nano), "level": l.level, "msg": l.msg,
		}
		if l.component != "" {
			entry["component"] = l.component
		}
		if l.reqID != "" {
			entry["request_id"] = l.reqID
		}
		raw, err := json.Marshal(entry)
		require.NoError(t, err)

		key := [2]string{l.level, l.component}
		i, ok := index[key]
		if !ok {
			i = len(streams)
			index[key] = i
			streams = append(streams, stream{Stream: map[string]string{
				"job": logging.LokiJob, "level": l.level, "component": cmpOrNone(l.component),
			}})
		}
		streams[i].Values = append(streams[i].Values, [2]string{strconv.FormatInt(l.at.UnixNano(), 10), string(raw)})
	}
	body, err := json.Marshal(map[string]any{"streams": streams})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/loki/api/v1/push", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, string(msg))
}

func cmpOrNone(c string) string {
	if c == "" {
		return "none"
	}
	return c
}

// fixture is the log of one short run: six lines, one second apart.
type fixture struct {
	from, to time.Time
	lines    []pushLine
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	start := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	at := func(i int) time.Time { return start.Add(time.Duration(i) * time.Second) }
	return fixture{
		from: start,
		to:   start.Add(time.Hour),
		lines: []pushLine{
			{at: at(0), level: "INFO", component: "api", reqID: "r-1", msg: "started"},
			{at: at(1), level: "WARN", component: "api", reqID: "r-2", msg: "slow: boom"},
			{at: at(2), level: "ERROR", component: "db", reqID: "r-1", msg: "database failed"},
			{at: at(3), level: "DEBUG", component: "rotation", msg: "plan built"},
			{at: at(4), level: "INFO", component: "outage", msg: "heartbeat ok"},
			// The message has a quote and a backslash; the JSON line escapes
			// them, and search matches the decoded message.
			{at: at(5), level: "WARN", msg: `he said "hi" and left \ quickly`},
		},
	}
}

// messagesOf returns the messages of the entries, in order.
func messagesOf(items []logsearch.Entry) []string {
	out := make([]string, 0, len(items))
	for _, e := range items {
		out = append(out, e.Message)
	}
	return out
}

// awaitLines waits until Loki answers the whole fixture, then returns.
func awaitLines(t *testing.T, svc *logsearch.Service, f fixture) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for {
		page, err := svc.Search(t.Context(), logsearch.Query{From: f.from, To: f.to})
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("items=%d total=%v", len(page.Items), page.Total)
			if page.Total != nil && *page.Total == int64(len(f.lines)) {
				return
			}
		}
		if time.Now().After(deadline) {
			require.FailNow(t, "the pushed lines are not readable", last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestLokiSearchFiltersPagingAndTotal(t *testing.T) {
	t.Parallel()
	base, _ := startLoki(t)
	f := newFixture(t)
	push(t, base, f.lines)
	svc := logsearch.NewService(logsearch.NewClient(base, nil), slog.New(slog.DiscardHandler))
	awaitLines(t, svc, f)
	ctx := t.Context()

	newest := []string{`he said "hi" and left \ quickly`, "heartbeat ok", "plan built", "database failed", "slow: boom", "started"}

	t.Run("range returns every line newest first with total", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Limit: 100})
		require.NoError(t, err)
		require.Equal(t, newest, messagesOf(page.Items))
		require.Empty(t, page.NextCursor)
		require.NotNil(t, page.Total)
		require.Equal(t, int64(6), *page.Total)
	})

	t.Run("level is a minimum", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, MinLevel: "WARN"})
		require.NoError(t, err)
		require.Equal(t, []string{`he said "hi" and left \ quickly`, "database failed", "slow: boom"}, messagesOf(page.Items))
		require.Equal(t, int64(3), *page.Total, "total follows the filters")
	})

	t.Run("level ERROR alone", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, MinLevel: "ERROR"})
		require.NoError(t, err)
		require.Equal(t, []string{"database failed"}, messagesOf(page.Items))
	})

	t.Run("component", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Component: "db"})
		require.NoError(t, err)
		require.Equal(t, []string{"database failed"}, messagesOf(page.Items))
	})

	t.Run("lines without a component are component none", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Component: "api"})
		require.NoError(t, err)
		require.Equal(t, []string{"slow: boom", "started"}, messagesOf(page.Items))
	})

	t.Run("search matches the message text", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Search: "heartbeat"})
		require.NoError(t, err)
		require.Equal(t, []string{"heartbeat ok"}, messagesOf(page.Items))
	})

	t.Run("search is case-insensitive", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Search: "HEARTBEAT"})
		require.NoError(t, err)
		require.Equal(t, []string{"heartbeat ok"}, messagesOf(page.Items))
	})

	t.Run("search does not match the other fields", func(t *testing.T) {
		for _, text := range []string{"r-1", "api", "INFO"} {
			page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Search: text})
			require.NoError(t, err, text)
			require.Empty(t, page.Items, text)
		}
	})

	// The quote is in one message only: a search for it finds that entry and
	// not every entry (a raw-line match would find all of them).
	t.Run("a double quote matches only the entries whose message has it", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Search: `"`})
		require.NoError(t, err)
		require.Equal(t, []string{`he said "hi" and left \ quickly`}, messagesOf(page.Items))
		require.NotNil(t, page.Total)
		require.Equal(t, int64(1), *page.Total)
	})

	t.Run("a quoted phrase with a backslash matches", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Search: `"hi" and left \ quickly`})
		require.NoError(t, err)
		require.Equal(t, []string{`he said "hi" and left \ quickly`}, messagesOf(page.Items))
	})

	t.Run("a backslash matches only the entry that has it", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Search: `\`})
		require.NoError(t, err)
		require.Equal(t, []string{`he said "hi" and left \ quickly`}, messagesOf(page.Items))
	})

	// Regular expression characters are plain text: none of these is in a
	// message, and as a regular expression .* would match every entry.
	t.Run("search is not a regular expression", func(t *testing.T) {
		for _, text := range []string{"slow.*boom", ".*", "(", "[a-z]+"} {
			page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Search: text})
			require.NoError(t, err, text)
			require.Empty(t, page.Items, text)
			require.NotNil(t, page.Total, text)
			require.Zero(t, *page.Total, text)
		}
	})

	t.Run("request id", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, RequestID: "r-1"})
		require.NoError(t, err)
		require.Equal(t, []string{"database failed", "started"}, messagesOf(page.Items))
	})

	t.Run("paging crosses page boundaries without loss or repeats", func(t *testing.T) {
		var got []string
		var cursor string
		pages := 0
		for {
			page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, Limit: 2, Cursor: cursor})
			require.NoError(t, err)
			require.NotNil(t, page.Total)
			require.Equal(t, int64(6), *page.Total, "the total is the whole range on every page")
			require.LessOrEqual(t, len(page.Items), 2)
			got = append(got, messagesOf(page.Items)...)
			pages++
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
			require.Less(t, pages, 10, "paging does not end")
		}
		require.Equal(t, 3, pages)
		require.Equal(t, newest, got)
	})

	t.Run("filters apply to every page", func(t *testing.T) {
		page, err := svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, MinLevel: "WARN", Limit: 2})
		require.NoError(t, err)
		require.Equal(t, []string{`he said "hi" and left \ quickly`, "database failed"}, messagesOf(page.Items))
		require.NotEmpty(t, page.NextCursor)

		page, err = svc.Search(ctx, logsearch.Query{From: f.from, To: f.to, MinLevel: "WARN", Limit: 2, Cursor: page.NextCursor})
		require.NoError(t, err)
		require.Equal(t, []string{"slow: boom"}, messagesOf(page.Items))
		require.Empty(t, page.NextCursor)
	})
}

func TestLokiRefusesRangeLongerThanItsLimit(t *testing.T) {
	t.Parallel()
	base, _ := startLoki(t)
	svc := logsearch.NewService(logsearch.NewClient(base, nil), slog.New(slog.DiscardHandler))
	now := time.Now().UTC()

	_, err := svc.Search(t.Context(), logsearch.Query{From: now.Add(-3000 * time.Hour), To: now})

	require.ErrorIs(t, err, logsearch.ErrRangeTooLong)
}

// The HTTP answers of the Logs endpoint: 503 when Loki is down, 422 for a
// range Loki refuses. The router is the real one, with the real client.
func TestLogsEndpointAnswersWhenLokiIsDown(t *testing.T) {
	t.Parallel()
	base, ctr := startLoki(t)
	svc := logsearch.NewService(logsearch.NewClient(base, nil), slog.New(slog.DiscardHandler))
	router := httpapi.NewRouter(httpapi.Deps{
		Log:       slog.New(slog.DiscardHandler),
		DB:        pinger{},
		LogSearch: svc,
		Now:       time.Now,
	})
	f := newFixture(t)
	push(t, base, f.lines)
	awaitLines(t, svc, f)

	get := func(query url.Values) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/logs?"+query.Encode(), nil)
		router.ServeHTTP(rec, req)
		return rec
	}
	window := url.Values{
		"from": {f.from.Format(time.RFC3339)}, "to": {f.to.Format(time.RFC3339)},
	}

	rec := get(url.Values{"from": {f.from.Add(-3000 * time.Hour).Format(time.RFC3339)}, "to": {time.Now().Format(time.RFC3339)}})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"range_too_long"`)

	stop := 10 * time.Second
	require.NoError(t, ctr.Stop(t.Context(), &stop))

	rec = get(window)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"code":"logs_unavailable"`)
	_, err := svc.Search(t.Context(), logsearch.Query{From: f.from, To: f.to})
	require.ErrorIs(t, err, logsearch.ErrUnavailable)
}

// The sink of the logger pushes the lines the backend writes, and the search
// reads them back under the same job label.
func TestLoggerSinkLinesAreSearchable(t *testing.T) {
	t.Parallel()
	base, _ := startLoki(t)
	logs := logging.New(base)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- logs.RunPush(ctx) }()

	from := time.Now().Add(-time.Minute)
	logging.For(logs.Logger, logging.ComponentRetention).Warn("sink line one", logging.KeyRequestID, "sink-1")
	logs.Logger.Info("sink line two")
	cancel()
	require.NoError(t, <-done, "the sink flushes at shutdown and returns nil")
	// The lines may still be in the async writer: Close drains it and sends
	// what is left.
	logs.Close(t.Context())

	svc := logsearch.NewService(logsearch.NewClient(base, nil), slog.New(slog.DiscardHandler))
	var page logsearch.Page
	require.Eventually(t, func() bool {
		got, serr := svc.Search(t.Context(), logsearch.Query{From: from, To: time.Now().Add(time.Minute)})
		page = got
		return serr == nil && len(page.Items) == 2
	}, 30*time.Second, 200*time.Millisecond)

	// Newest first: the second line has no component, so it is "none".
	require.Equal(t, "sink line two", page.Items[0].Message)
	require.Empty(t, page.Items[0].Component)
	require.Equal(t, "INFO", page.Items[0].Level)
	require.Equal(t, "retention", page.Items[1].Component)
	require.Equal(t, "sink-1", page.Items[1].RequestID)
	require.Equal(t, "WARN", page.Items[1].Level)

	filtered, err := svc.Search(t.Context(), logsearch.Query{From: from, To: time.Now().Add(time.Minute), Component: "retention"})
	require.NoError(t, err)
	require.Len(t, filtered.Items, 1)
}

// pinger is a database that always answers.
type pinger struct{}

func (pinger) Ping(context.Context) error { return nil }
