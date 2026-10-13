package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

const readyTimeout = 2 * time.Second

type health struct {
	db  Pinger
	log *slog.Logger // component db
}

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// live answers while the process is running (Docker health check).
func (health) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"}, contentTypeJSON)
}

// ready answers 200 when the database is reachable, 503 otherwise.
func (h health) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		h.log.WarnContext(r.Context(), "database not reachable",
			logging.KeyRequestID, middleware.GetReqID(r.Context()),
			logging.KeyError, err,
		)
		writeJSON(w, http.StatusServiceUnavailable, healthResponse{
			Status: "unavailable",
			Checks: map[string]string{"database": "unreachable"},
		}, contentTypeJSON)
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Status: "ok",
		Checks: map[string]string{"database": "ok"},
	}, contentTypeJSON)
}
