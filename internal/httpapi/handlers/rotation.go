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
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

// Rotation keeps the track plan and the filler reminders
// (*rotation.Service).
type Rotation interface {
	Rebuild(ctx context.Context) (uint64, error)
	RebuildRange(ctx context.Context, r rotation.Range) (uint64, error)
	Plan(ctx context.Context, r rotation.Range) ([]rotation.PlannedHour, error)
	Requests(ctx context.Context, status rotation.RequestStatus) ([]rotation.FillerRequest, error)
	Answer(ctx context.Context, id, trackID uint64) (rotation.FillerRequest, error)
	PoolRefilled(ctx context.Context, clockID uint64) error
	Rescheduler
}

type trackPlanInput struct {
	From time.Time `query:"from" required:"true" doc:"Hours that start at or after this instant (UTC)."`
	To   time.Time `query:"to" required:"true" doc:"... and before this one; at most 7 days after from."`
}

type trackPlanOutput struct {
	Body dto.TrackPlan
}

type trackPlanRebuildInput struct {
	Body dto.TrackPlanRebuildBody
}

type operationAcceptedOutput struct {
	Body dto.OperationAccepted
}

type fillerRequestListInput struct {
	Status string `query:"status" enum:"pending,answered,defaulted,expired,all" default:"pending"`
}

type fillerRequestListOutput struct {
	Body dto.FillerRequestList
}

type fillerAnswerInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.FillerAnswerBody
}

type fillerRequestOutput struct {
	Body dto.FillerRequest
}

func registerRotation(api huma.API, log *slog.Logger, svc Rotation) {
	tags := []string{"Track plan"}

	register(api, log, huma.Operation{
		OperationID: "get-track-plan",
		Method:      http.MethodGet,
		Path:        "/api/track-plan",
		Summary:     "Get the track plan",
		Description: "The built hours that start in the range, with their tracks in time order (§7). The plan covers the next rotation.plan_horizon_hours.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *trackPlanInput) (*trackPlanOutput, error) {
		hs, err := svc.Plan(ctx, rotation.Range{From: in.From.UTC(), To: in.To.UTC()})
		if err != nil {
			return nil, err
		}
		out := &trackPlanOutput{Body: dto.TrackPlan{Items: make([]dto.TrackPlanHour, len(hs))}}
		for i, h := range hs {
			out.Body.Items[i] = planHourDTO(h)
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "rebuild-track-plan",
		Method:        http.MethodPost,
		Path:          "/api/track-plan/rebuild",
		Summary:       "Rebuild the track plan",
		Description:   "Rebuilds every future hour of the range, even when it is current, as a clock_build operation. Hours that have started are not touched. Pending reminders of the rebuilt hours expire, and new ones are created.",
		Tags:          tags,
		DefaultStatus: http.StatusAccepted,
		Errors:        errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *trackPlanRebuildInput) (*operationAcceptedOutput, error) {
		id, err := svc.RebuildRange(ctx, rotation.Range{From: in.Body.From.UTC(), To: in.Body.To.UTC()})
		if err != nil {
			return nil, err
		}
		return &operationAcceptedOutput{Body: dto.OperationAccepted{OperationID: id}}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "list-filler-requests",
		Method:      http.MethodGet,
		Path:        "/api/filler-requests",
		Summary:     "List filler reminders",
		Description: "Reminders of short hours that wait for the operator, and of empty filler pools (§7), newest hour first.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *fillerRequestListInput) (*fillerRequestListOutput, error) {
		status := rotation.RequestStatus(in.Status)
		if in.Status == "all" {
			status = ""
		}
		rs, err := svc.Requests(ctx, status)
		if err != nil {
			return nil, err
		}
		out := &fillerRequestListOutput{Body: dto.FillerRequestList{Items: make([]dto.FillerRequest, len(rs))}}
		for i, r := range rs {
			out.Body.Items[i] = fillerRequestDTO(r)
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "answer-filler-request",
		Method:      http.MethodPost,
		Path:        "/api/filler-requests/{id}/answer",
		Summary:     "Answer a filler reminder",
		Description: "The track fills the hour's first waiting segment, the clock's default filler mode the others, and the hour is rebuilt. Refused (track_id: blocked) when the track breaks separation or a play rule at that time; a pool_empty reminder is closed by refilling the pool instead.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *fillerAnswerInput) (*fillerRequestOutput, error) {
		r, err := svc.Answer(ctx, in.ID, in.Body.TrackID)
		if err != nil {
			return nil, err
		}
		return &fillerRequestOutput{Body: fillerRequestDTO(r)}, nil
	})
}

// startRebuild starts the clock_build operation after a clock or grid
// change. The change is already saved, so a failure to start is logged, not
// returned: the plan job rebuilds the hours on its next run.
func startRebuild(ctx context.Context, log *slog.Logger, svc Rotation) *uint64 {
	id, err := svc.Rebuild(ctx)
	if err != nil {
		log.ErrorContext(ctx, "start plan rebuild failed",
			logging.KeyRequestID, middleware.GetReqID(ctx), logging.KeyError, err)
		return nil
	}
	return &id
}

func planHourDTO(h rotation.PlannedHour) dto.TrackPlanHour {
	out := dto.TrackPlanHour{
		HourStartUTC: h.Start.UTC(), Clock: dto.TrackPlanClock{ID: h.ClockID, Name: h.ClockName},
		GridSlotID: h.GridSlotID, Seed: h.Seed, BuiltAt: h.BuiltAt.UTC(), FillerRequestID: h.FillerRequestID,
		Items: make([]dto.TrackPlanItem, len(h.Items)),
	}
	for i, it := range h.Items {
		out.Items[i] = dto.TrackPlanItem{
			ID: it.ID, TrackID: it.TrackID, Title: it.Title, ArtistName: it.ArtistName, DurationMS: it.DurationMS,
			ElementID: it.ElementID, PlannedStartUTC: it.Start.UTC(), PlannedOutMS: it.OutMS, Truncated: it.Truncated,
			Source: it.Source, Status: it.Status,
		}
	}
	return out
}

func fillerRequestDTO(r rotation.FillerRequest) dto.FillerRequest {
	return dto.FillerRequest{
		ID: r.ID, ClockID: r.ClockID, ClockName: r.ClockName, HourStartUTC: r.HourStart.UTC(), Reason: r.Reason,
		Mode: string(r.Mode), Status: string(r.Status), CreatedAt: r.CreatedAt.UTC(), DeadlineAt: r.DeadlineAt.UTC(),
		AnswerTrackID: r.AnswerTrackID,
	}
}
