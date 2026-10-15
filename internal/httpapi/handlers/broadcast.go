package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/broadcast"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
)

// Broadcast serves the derived broadcast state (*broadcast.Tracker).
type Broadcast interface {
	Status() broadcast.Status
}

type broadcastStatusOutput struct {
	Body dto.BroadcastStatus
}

func registerBroadcast(api huma.API, log *slog.Logger, now func() time.Time, b Broadcast) {
	register(api, log, huma.Operation{
		OperationID: "get-broadcast-status",
		Method:      http.MethodGet,
		Path:        "/api/broadcast/status",
		Summary:     "Get the broadcast status",
		Description: "Whether the stream reaches Icecast (connected, reconnecting, unreachable, unknown) and since when (§7.1). " +
			"The broadcast.state event reports each change; the panel reads this when its event connection opens.",
		Tags:   []string{"Status"},
		Errors: commonErrors,
	}, func(context.Context, *struct{}) (*broadcastStatusOutput, error) {
		return &broadcastStatusOutput{Body: broadcastStatusDTO(b.Status(), now().UTC())}, nil
	})
}

func broadcastStatusDTO(s broadcast.Status, t time.Time) dto.BroadcastStatus {
	return dto.BroadcastStatus{
		State: string(s.State), Since: s.Since, Reason: s.Reason, OutageID: s.OutageID,
		MountOK: s.MountOK, LastCheckAt: s.LastCheckAt, Time: t,
	}
}
