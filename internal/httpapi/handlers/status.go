package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

type liveStatusOutput struct {
	Body dto.LiveStatus
}

// registerStatus registers the live check of the update script. Every
// source is optional: a missing or failing one leaves its field null
// (unknown), which the script treats as on air.
func registerStatus(api huma.API, log *slog.Logger, now func() time.Time, lv Live, outages Outages, g Grid) {
	register(api, log, huma.Operation{
		OperationID: "get-live-status",
		Method:      http.MethodGet,
		Path:        "/api/status/live",
		Summary:     "Get the live status",
		Description: "Whether the station is on air live (a live voice session sends audio), whether an outage is open, and the clock of the current hour. The update script checks it before restarting services. " +
			"A null field means unknown and must be treated as on air.",
		Tags:   []string{"Status"},
		Errors: commonErrors,
	}, func(ctx context.Context, _ *struct{}) (*liveStatusOutput, error) {
		t := now().UTC()
		out := dto.LiveStatus{Time: t}
		warn := func(msg string, err error) {
			log.WarnContext(ctx, msg, logging.KeyRequestID, middleware.GetReqID(ctx), logging.KeyError, err)
		}
		if lv != nil {
			out.OnAir = new(lv.Status().OnAir())
		}
		if outages != nil {
			if o, open, err := outages.OpenOutage(ctx); err != nil {
				warn("live status: read open outage failed", err)
			} else if open {
				out.OpenOutageID = new(o.ID)
			}
		}
		if g != nil {
			hours, err := g.Hours(ctx, t.Add(-time.Hour), t.Add(time.Second))
			if err != nil {
				warn("live status: read grid failed", err)
			}
			for _, h := range hours {
				if !h.StartUTC.After(t) && t.Before(h.StartUTC.Add(time.Hour)) {
					out.CurrentClock = new(h.Clock.Name)
				}
			}
		}
		return &liveStatusOutput{Body: out}, nil
	})
}
