package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
)

type liveStatusOutput struct {
	Body dto.LiveStatus
}

func registerStatus(api huma.API, log *slog.Logger, now func() time.Time) {
	register(api, log, huma.Operation{
		OperationID: "get-live-status",
		Method:      http.MethodGet,
		Path:        "/api/status/live",
		Summary:     "Get the live status",
		Description: "Whether the station is on air and whether an outage is open. The update script checks it before restarting services. " +
			"A null field means unknown and must be treated as on air. Until playout and outage detection exist, on_air, open_outage_id and current_clock are always null.",
		Tags:   []string{"Status"},
		Errors: commonErrors,
	}, func(context.Context, *struct{}) (*liveStatusOutput, error) {
		// Playout and outage state arrive with the Liquidsoap and outage
		// phases; until then everything but the time is unknown.
		return &liveStatusOutput{Body: dto.LiveStatus{Time: now().UTC()}}, nil
	})
}
