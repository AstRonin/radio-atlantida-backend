package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

// requestIDHeader is echoed on every response so the panel can quote it.
const requestIDHeader = "X-Request-Id"

// exposeRequestID returns the request ID (set by middleware.RequestID) in the
// response header.
func exposeRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := middleware.GetReqID(r.Context()); id != "" {
			w.Header().Set(requestIDHeader, id)
		}
		next.ServeHTTP(w, r)
	})
}

// requestLogger logs one entry per request. Successful and client-error
// requests are logged at DEBUG (off by default, §10.1); 5xx at WARN. The
// cause of a 5xx is logged at ERROR where it is handled (for a panic, by
// recoverer), so the error is not reported twice at ERROR.
func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			level := slog.LevelDebug
			if status >= http.StatusInternalServerError {
				level = slog.LevelWarn
			}
			log.Log(r.Context(), level, "http request",
				logging.KeyRequestID, middleware.GetReqID(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// recoverer turns a panic in a handler into a 500 response and one ERROR
// entry with the stack. http.ErrAbortHandler is re-panicked, as net/http
// expects.
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { //nolint:contextcheck // false positive on the deferred closure; it uses r.Context()
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel compared as net/http does
					panic(rec)
				}
				reqID := middleware.GetReqID(r.Context())
				log.ErrorContext(r.Context(), "panic in http handler",
					logging.KeyRequestID, reqID,
					"method", r.Method,
					"path", r.URL.Path,
					logging.KeyError, fmt.Sprint(rec),
					"stack", string(debug.Stack()),
				)
				// Upgraded connections (WebSocket) cannot take a response.
				if r.Header.Get("Connection") != "Upgrade" {
					writeJSON(w, http.StatusInternalServerError, problem{
						Title:     http.StatusText(http.StatusInternalServerError),
						Status:    http.StatusInternalServerError,
						RequestID: reqID,
					}, contentTypeProblem)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// limitBody caps the size of request bodies.
func limitBody(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
