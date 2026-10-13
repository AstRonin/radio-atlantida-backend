// Package httpapi holds the HTTP server: router, middleware, and handlers.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

const (
	// requestTimeout bounds normal endpoints. Streaming routes (track
	// preview) and the WebSocket hub are registered without it.
	requestTimeout = 30 * time.Second
	// maxRequestBody limits JSON request bodies.
	maxRequestBody = 1 << 20

	contentTypeJSON    = "application/json"
	contentTypeProblem = "application/problem+json"
)

// Pinger reports whether a dependency is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps are the dependencies of the HTTP API.
type Deps struct {
	// Log is the root logger; each handler tags entries with its component.
	Log *slog.Logger
	// DB is checked by /readyz.
	DB Pinger
}

// NewRouter builds the HTTP handler with the common middleware chain.
func NewRouter(d Deps) http.Handler {
	apiLog := logging.For(d.Log, logging.ComponentAPI)

	r := chi.NewRouter()
	r.Use(
		middleware.RequestID,
		exposeRequestID,
		requestLogger(apiLog),
		recoverer(apiLog),
		limitBody(maxRequestBody),
	)

	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(requestTimeout))

		h := health{db: d.DB, log: logging.For(d.Log, logging.ComponentDB)}
		r.Get("/healthz", h.live)
		r.Get("/readyz", h.ready)
	})

	return r
}

// problem is the error body. It follows RFC 9457; the full error format is
// added with the API contract (Phase 3).
type problem struct {
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	// The status line is already sent; an encoding error (client gone) can
	// only be dropped.
	_ = json.NewEncoder(w).Encode(body)
}
