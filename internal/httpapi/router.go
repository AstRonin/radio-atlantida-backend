// Package httpapi holds the HTTP server: router, middleware, the huma API
// that produces the OpenAPI document, and the error format.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/handlers"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

const (
	// requestTimeout bounds normal endpoints. Streaming routes (track
	// preview) and the WebSocket hub are registered without it.
	requestTimeout = 30 * time.Second
	// maxRequestBody limits JSON request bodies.
	maxRequestBody = 1 << 20

	contentTypeJSON = "application/json"

	wsPath = "/api/ws"
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
	// Version is the release version shown in the OpenAPI document.
	Version string
	// Events serves the WebSocket at /api/ws.
	Events http.Handler
	// Operations and Logs back the REST handlers.
	Operations handlers.OperationStore
	Logs       handlers.LogFiles
	Settings   handlers.Settings
	Library    handlers.Library
	PlayRules  handlers.PlayRules
	Separation handlers.Separation
	Clocks     handlers.Clocks
	Grid       handlers.Grid
	Rotation   handlers.Rotation
	// Now is the clock of the live status.
	Now func() time.Time
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
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusNotFound, errcodes.NotFound)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusMethodNotAllowed, errcodes.MethodNotAllowed)
	})

	// Long-lived: no request timeout. The hub clears the server deadlines.
	if d.Events != nil {
		r.Get(wsPath, d.Events.ServeHTTP)
	}

	timed := r.With(middleware.Timeout(requestTimeout))
	h := health{db: d.DB, log: logging.For(d.Log, logging.ComponentDB)}
	timed.Get("/healthz", h.live)
	timed.Get("/readyz", h.ready)

	// Streaming operations (track preview) are registered on r, without
	// the timeout; every other operation on timed.
	api := newAPI(timed, r, d.Version)
	handlers.Register(api, handlers.Deps{
		Log: d.Log, Operations: d.Operations, Logs: d.Logs, Settings: d.Settings,
		Library: d.Library, PlayRules: d.PlayRules, Separation: d.Separation,
		Clocks: d.Clocks, Grid: d.Grid, Rotation: d.Rotation, Now: d.Now,
	})

	return r
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code errcodes.Problem) {
	p := dto.NewProblem(status, code, "")
	p.RequestID = middleware.GetReqID(r.Context())
	writeJSON(w, status, p, dto.ContentTypeProblem)
}

func writeJSON(w http.ResponseWriter, status int, body any, contentType string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	// The status line is already sent; an encoding error (client gone) can
	// only be dropped.
	_ = json.NewEncoder(w).Encode(body)
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
