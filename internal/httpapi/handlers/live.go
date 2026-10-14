package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/live"
)

// Live relays the microphone to air (*live.Bridge).
type Live interface {
	Start(ctx context.Context, offer string, takeover bool) (live.Answer, error)
	Stop()
	Status() live.Status
	Available() bool
}

type liveSessionInput struct {
	Body dto.LiveSessionBody
}

type liveSessionOutput struct {
	Body dto.LiveSession
}

type liveInputOutput struct {
	Body dto.LiveInput
}

func registerLive(api huma.API, log *slog.Logger, svc Live) {
	tags := []string{"Live"}

	register(api, log, huma.Operation{
		OperationID:   "start-live-session",
		Method:        http.MethodPost,
		Path:          "/api/live/session",
		Summary:       "Go live",
		Description:   "Starts a live voice session (§8.4): the browser sends its WebRTC offer and gets the answer; its microphone then goes to air over the music, which is lowered while the session lasts (§8.5). One session at a time. The state is sent as live.state events.",
		Tags:          tags,
		DefaultStatus: http.StatusCreated,
		Errors:        errorsWith(http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable),
	}, func(ctx context.Context, in *liveSessionInput) (*liveSessionOutput, error) {
		a, err := svc.Start(ctx, in.Body.Offer, in.Body.Takeover)
		if err != nil {
			return nil, err
		}
		return &liveSessionOutput{Body: dto.LiveSession{SessionID: a.SessionID, Answer: a.SDP}}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "stop-live-session",
		Method:        http.MethodDelete,
		Path:          "/api/live/session",
		Summary:       "End the live session",
		Description:   "Ends the live voice session, if any; the music returns to its level.",
		Tags:          tags,
		DefaultStatus: http.StatusNoContent,
		Errors:        errorsWith(),
	}, func(context.Context, *struct{}) (*struct{}, error) {
		svc.Stop()
		return &struct{}{}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "get-live-input",
		Method:      http.MethodGet,
		Path:        "/api/live/status",
		Summary:     "Get the live voice state",
		Description: "The state of the live voice input and its session.",
		Tags:        tags,
		Errors:      errorsWith(),
	}, func(context.Context, *struct{}) (*liveInputOutput, error) {
		st := svc.Status()
		return &liveInputOutput{Body: dto.LiveInput{
			Available: svc.Available(), State: string(st.State), SessionID: st.SessionID, OnAirSince: st.OnAirSince,
			HarborConnected: st.HarborConnected, Packets: st.Packets, LastPacketAt: st.LastPacketAt,
		}}, nil
	})
}
