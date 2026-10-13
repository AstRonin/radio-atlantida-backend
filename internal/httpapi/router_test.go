package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) entries(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		out = append(out, m)
	}
	return out
}

func newTestRouter(db Pinger) (http.Handler, *syncBuffer) {
	logs := &syncBuffer{}
	log := slog.New(logging.NewHandler(logs, slog.LevelDebug))
	return NewRouter(Deps{Log: log, DB: db}), logs
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		path       string
		db         Pinger
		wantStatus int
		wantBody   string
	}{
		{"live", "/healthz", fakePinger{errors.New("down")}, http.StatusOK, `{"status":"ok"}`},
		{"ready", "/readyz", fakePinger{}, http.StatusOK, `{"status":"ok","checks":{"database":"ok"}}`},
		{
			"not ready", "/readyz",
			fakePinger{errors.New("down")},
			http.StatusServiceUnavailable,
			`{"status":"unavailable","checks":{"database":"unreachable"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestRouter(tt.db)
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))

			require.Equal(t, tt.wantStatus, rec.Code)
			require.JSONEq(t, tt.wantBody, rec.Body.String())
			require.Equal(t, contentTypeJSON, rec.Header().Get("Content-Type"))
			require.NotEmpty(t, rec.Header().Get(requestIDHeader))
		})
	}
}

func TestReadyLogsDatabaseComponentOnce(t *testing.T) {
	t.Parallel()
	h, logs := newTestRouter(fakePinger{errors.New("down")})

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))

	var warn map[string]any
	for _, e := range logs.entries(t) {
		if e["msg"] == "database not reachable" {
			warn = e
		}
	}
	require.NotNil(t, warn)
	require.Equal(t, "db", warn["component"])
	require.NotEmpty(t, warn["request_id"])
}

func TestRecovererReturnsProblemAndLogsOnce(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	log := slog.New(logging.NewHandler(logs, slog.LevelInfo))
	r := chi.NewRouter()
	r.Use(exposeRequestID, requestLogger(log), recoverer(log))
	r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boom", nil))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, contentTypeProblem, rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{"title":"Internal Server Error","status":500}`, rec.Body.String())

	var errorsLogged int
	for _, e := range logs.entries(t) {
		if e["level"] == "ERROR" {
			errorsLogged++
			require.Equal(t, "kaboom", e["err"])
			require.Contains(t, e["stack"], "router_test.go")
		}
	}
	require.Equal(t, 1, errorsLogged)
}

func TestLimitBody(t *testing.T) {
	t.Parallel()
	h := limitBody(4)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(r.Body); err != nil {
			var tooLarge *http.MaxBytesError
			require.ErrorAs(t, err, &tooLarge)
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader("12345")))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestNewServerHasTimeouts(t *testing.T) {
	t.Parallel()

	s := NewServer("127.0.0.1:0", http.NotFoundHandler(), slog.New(slog.DiscardHandler))

	require.Positive(t, s.ReadHeaderTimeout)
	require.Positive(t, s.ReadTimeout)
	require.Positive(t, s.WriteTimeout)
	require.Positive(t, s.IdleTimeout)
}
