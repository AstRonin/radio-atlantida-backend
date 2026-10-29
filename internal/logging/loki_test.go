package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pushServer records every push request body and answers with status.
type pushServer struct {
	mu       sync.Mutex
	status   func(call int) int
	requests []pushRequest
	calls    int
}

func newPushServer(t *testing.T, status func(call int) int) (*pushServer, *httptest.Server) {
	t.Helper()
	ps := &pushServer{status: status}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.URL.Path != "/loki/api/v1/push" || r.Method != http.MethodPost ||
			r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		ps.mu.Lock()
		ps.calls++
		code := ps.status(ps.calls)
		if code >= 200 && code < 300 {
			var req pushRequest
			if err := json.Unmarshal(body, &req); err != nil {
				ps.mu.Unlock()
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			ps.requests = append(ps.requests, req)
		}
		ps.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return ps, srv
}

// delivered returns the number of values received in all successful requests.
func (ps *pushServer) delivered() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	n := 0
	for _, r := range ps.requests {
		for _, s := range r.Streams {
			n += len(s.Values)
		}
	}
	return n
}

func (ps *pushServer) callCount() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.calls
}

func (ps *pushServer) successes() int {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return len(ps.requests)
}

// lineAt builds a JSON line as the handler writes it, with a time offset in
// nanoseconds from base.
func lineAt(base time.Time, offset int, level, component, msg string) []byte {
	m := map[string]string{
		"time":  base.Add(time.Duration(offset)).Format(time.RFC3339Nano),
		"level": level,
		"msg":   msg,
	}
	if component != "" {
		m["component"] = component
	}
	b, _ := json.Marshal(m) // a map of strings always marshals
	return append(b, '\n')
}

// startRun runs the sink in a goroutine and returns a function that stops it
// and waits for the final flush.
func startRun(t *testing.T, s *LokiSink) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err, "Run returns nil after the final flush")
		case <-time.After(10 * time.Second):
			t.Fatal("sink did not stop")
		}
	}
}

func testSink(t *testing.T, url string, buffer int) (*LokiSink, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	s := newLokiSink(url, For(slog.New(NewHandler(&out, slog.LevelInfo)), ComponentAPI), buffer)
	s.flushEvery = time.Hour // tests flush on their own terms unless they say otherwise
	s.reportEvery = time.Hour
	s.backoff = time.Millisecond
	return s, &out
}

func TestLokiSinkPushesLabelsAndLines(t *testing.T) {
	t.Parallel()
	ps, srv := newPushServer(t, func(int) int { return http.StatusNoContent })
	s, _ := testSink(t, srv.URL+"/", 16) // a trailing slash in the URL is not doubled
	s.flushEvery = 5 * time.Millisecond
	stop := startRun(t, s)

	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	_, _ = s.Write(lineAt(base, 1, "INFO", "api", "started"))
	_, _ = s.Write(lineAt(base, 2, "WARN", "", "no component"))
	require.Eventually(t, func() bool { return ps.delivered() == 2 }, 5*time.Second, 5*time.Millisecond)
	stop()

	ps.mu.Lock()
	defer ps.mu.Unlock()
	streams := map[string]pushStream{}
	for _, r := range ps.requests {
		for _, st := range r.Streams {
			streams[st.Stream["level"]+"/"+st.Stream["component"]] = st
		}
	}
	info, ok := streams["INFO/api"]
	require.True(t, ok, "stream labels: %v", streams)
	require.Equal(t, LokiJob, info.Stream["job"])
	require.Len(t, info.Values, 1)
	require.Equal(t, strconv.FormatInt(base.Add(1).UnixNano(), 10), info.Values[0][0])
	require.JSONEq(t, `{"time":"2026-10-28T10:00:00.000000001Z","level":"INFO","msg":"started","component":"api"}`,
		info.Values[0][1])

	none, ok := streams["WARN/none"]
	require.True(t, ok, "an entry without a component gets component=none: %v", streams)
	require.Equal(t, LokiJob, none.Stream["job"])
	require.Len(t, none.Values, 1)
}

func TestLokiSinkSendsAtBatchSizeLimit(t *testing.T) {
	t.Parallel()
	ps, srv := newPushServer(t, func(int) int { return http.StatusNoContent })
	s, _ := testSink(t, srv.URL, 4096)
	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	for i := range 1200 {
		_, _ = s.Write(lineAt(base, i, "INFO", "api", "line"))
	}
	stop := startRun(t, s)

	// Two full batches go out on the size limit; the rest on shutdown.
	require.Eventually(t, func() bool { return ps.successes() >= 2 }, 5*time.Second, 5*time.Millisecond)
	stop()

	ps.mu.Lock()
	defer ps.mu.Unlock()
	require.Len(t, ps.requests, 3)
	require.Equal(t, 500, len(ps.requests[0].Streams[0].Values))
	require.Equal(t, 500, len(ps.requests[1].Streams[0].Values))
	require.Equal(t, 200, len(ps.requests[2].Streams[0].Values))
}

func TestLokiSinkFlushesBufferedLinesOnShutdown(t *testing.T) {
	t.Parallel()
	ps, srv := newPushServer(t, func(int) int { return http.StatusNoContent })
	s, _ := testSink(t, srv.URL, 16)
	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	stop := startRun(t, s)
	for i := range 3 {
		_, _ = s.Write(lineAt(base, i, "ERROR", "db", "queued"))
	}

	// The periodic flush is an hour away: only the shutdown sends the lines.
	stop()

	require.Equal(t, 3, ps.delivered())
}

func TestLokiSinkDropsWhenFullAndReportsOnce(t *testing.T) {
	t.Parallel()
	// Not running: nothing drains the buffer, so it fills up.
	s, out := testSink(t, "http://127.0.0.1:1", 2)
	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)

	for i := range 5 {
		n, err := s.Write(lineAt(base, i, "INFO", "api", "x"))
		require.NoError(t, err)
		require.Positive(t, n)
	}
	require.Equal(t, uint64(3), s.dropped.Load())

	s.report()
	require.Equal(t, 1, bytes.Count(out.Bytes(), []byte("log lines not sent to loki")))
	require.Contains(t, out.String(), `"dropped":3`)
	require.Contains(t, out.String(), `"component":"api"`)
	require.Zero(t, s.dropped.Load(), "the count is reset by the report")

	out.Reset()
	s.report()
	require.Empty(t, out.String(), "nothing to report, nothing logged")
}

func TestLokiSinkRetriesThenDelivers(t *testing.T) {
	t.Parallel()
	ps, srv := newPushServer(t, func(call int) int {
		if call <= 2 {
			return http.StatusInternalServerError
		}
		return http.StatusNoContent
	})
	s, out := testSink(t, srv.URL, 16)
	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	_, _ = s.Write(lineAt(base, 1, "INFO", "api", "retried"))
	stop := startRun(t, s)
	stop()

	require.Equal(t, 1, ps.delivered(), "the third attempt delivers the batch")
	s.report()
	require.Empty(t, out.String(), "nothing lost")
}

func TestLokiSinkCountsLossAfterAttempts(t *testing.T) {
	t.Parallel()
	ps, srv := newPushServer(t, func(int) int { return http.StatusBadGateway })
	s, out := testSink(t, srv.URL, 16)
	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	stop := startRun(t, s)
	_, _ = s.Write(lineAt(base, 1, "INFO", "api", "lost"))
	_, _ = s.Write(lineAt(base, 2, "INFO", "api", "lost too"))
	stop()

	require.Zero(t, ps.delivered())
	require.Equal(t, lokiAttempts, ps.callCount(), "every attempt was made")
	s.report()
	require.Contains(t, out.String(), `"lost":2`)
	require.Contains(t, out.String(), "status 502")
}

func TestLokiSinkDoesNotRetryRejectedPush(t *testing.T) {
	t.Parallel()
	ps, srv := newPushServer(t, func(int) int { return http.StatusBadRequest })
	s, out := testSink(t, srv.URL, 16)
	base := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	stop := startRun(t, s)
	_, _ = s.Write(lineAt(base, 1, "INFO", "api", "refused"))
	stop()

	require.Equal(t, 1, ps.callCount(), "a 4xx is not retried")
	s.report()
	require.Contains(t, out.String(), `"lost":1`)
}

func TestLogTimeParsedAndFallbackForBrokenLine(t *testing.T) {
	t.Parallel()
	arrived := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

	p := parseLine([]byte("not json\n"), arrived)
	require.Equal(t, arrived, p.ts)
	require.Equal(t, lokiNone, p.level)
	require.Equal(t, lokiNone, p.component)
	require.Equal(t, "not json", p.line)
}

func TestNewWithoutLokiURLHasNoPush(t *testing.T) {
	t.Parallel()
	l := New("")
	require.Nil(t, l.loki)
	require.NoError(t, l.RunPush(t.Context()), "returns at once")
	l.Close(t.Context())
}

// A line the async writer still holds when the push stops reaches Loki: Close
// drains the writer first and then sends what is left.
func TestLoggingCloseSendsLinesStillInTheAsyncWriter(t *testing.T) {
	t.Parallel()
	ps, srv := newPushServer(t, func(int) int { return http.StatusNoContent })
	l := New(srv.URL)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.RunPush(ctx) }()

	l.Logger.Info("first")
	For(l.Logger, ComponentAPI).Warn("last")
	cancel()
	require.NoError(t, <-done, "RunPush flushes and returns nil")
	l.Close(t.Context())

	require.Equal(t, 2, ps.delivered())
}
