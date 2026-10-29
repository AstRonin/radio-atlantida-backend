package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

// Rotation keeps the track plan and its one-time inserts
// (*rotation.Service).
type Rotation interface {
	Rebuild(ctx context.Context) (uint64, error)
	RebuildRange(ctx context.Context, r rotation.Range) (uint64, error)
	RebuildFull(ctx context.Context) (uint64, error)
	Plan(ctx context.Context, r rotation.Range) ([]rotation.PlannedHour, error)
	Sample(ctx context.Context, c clocks.Clock) (rotation.Sample, error)
	Simulate(ctx context.Context, from, to time.Time, withItems bool) (rotation.Simulation, error)
	AddInsert(ctx context.Context, r rotation.AddInsertRequest) (uint64, error)
	RemoveInsert(ctx context.Context, id uint64) (uint64, error)
	Rescheduler
}

type trackPlanInput struct {
	From time.Time `query:"from" required:"true" doc:"Hours that start at or after this instant (UTC)."`
	To   time.Time `query:"to" required:"true" doc:"... and before this one; at most 31 days after from."`
}

type trackSimulateInput struct {
	From  string `query:"from" required:"true" format:"date" doc:"First station local date."`
	To    string `query:"to" required:"true" format:"date" doc:"Last station local date, inclusive; at most 31 days in all."`
	Items bool   `query:"items" doc:"Include the items of every built hour."`
}

type trackSimulateOutput struct {
	Body dto.TrackSimulation
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

type addHourInsertInput struct {
	Body dto.HourInsertBody
}

func registerRotation(api huma.API, log *slog.Logger, svc Rotation) {
	tags := []string{"Track plan"}

	register(api, log, huma.Operation{
		OperationID: "get-track-plan",
		Method:      http.MethodGet,
		Path:        "/api/track-plan",
		Summary:     "Get the track plan",
		Description: "The built hours that start in the range, with their tracks in time order (§7). The 5-minute pass keeps the plan for the next rotation.plan_horizon_days.",
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
		OperationID: "simulate-track-plan",
		Method:      http.MethodGet,
		Path:        "/api/rotation/simulate",
		Summary:     "Simulate the track plan for a date range",
		Description: "Builds every grid hour of the station local dates, inclusive, in time order, as the planner would, and returns the result. Nothing is stored: the plan, the scheduled items and the one-time inserts are not changed. Each built hour adds its items to the history of the later hours, so the play rules hold across the simulated hours. Hours whose clock no longer exists are skipped and counted.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *trackSimulateInput) (*trackSimulateOutput, error) {
		from, err := parseDate("query.from", in.From)
		if err != nil {
			return nil, err
		}
		to, err := parseDate("query.to", in.To)
		if err != nil {
			return nil, err
		}
		sim, err := svc.Simulate(ctx, from, to, in.Items)
		if err != nil {
			return nil, err
		}
		return &trackSimulateOutput{Body: simulationDTO(sim)}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "rebuild-track-plan",
		Method:        http.MethodPost,
		Path:          "/api/track-plan/rebuild",
		Summary:       "Rebuild the track plan",
		Description:   "Rebuilds every future hour of the range, even when it is current, as a clock_build operation. Hours that have started are not touched; one-time inserts stay.",
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
		OperationID:   "rebuild-track-plan-full",
		Method:        http.MethodPost,
		Path:          "/api/track-plan/rebuild-full",
		Summary:       "Rebuild the whole track plan",
		Description:   "Starts the full pass of the track plan: the hours from now to the grid horizon are rebuilt in batches, as a track_plan_full operation. Hours that have started, hours with an item in Liquidsoap's queue, and hours whose inputs did not change are skipped. Progress is stored on the operation; rotation.plan_full is sent at the start and at the end. Refused (track_plan_full_running) while another full pass runs.",
		Tags:          tags,
		DefaultStatus: http.StatusAccepted,
		Errors:        errorsWith(http.StatusConflict),
	}, func(ctx context.Context, _ *struct{}) (*operationAcceptedOutput, error) {
		id, err := svc.RebuildFull(ctx)
		if err != nil {
			return nil, err
		}
		return &operationAcceptedOutput{Body: dto.OperationAccepted{OperationID: id}}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "add-hour-insert",
		Method:        http.MethodPost,
		Path:          "/api/track-plan/inserts",
		Summary:       "Add a one-time insert to a planned hour",
		Description:   "Plays the track at a time with seconds in one hour that has not started (today's news), as an hour_insert operation that stores the insert and rebuilds the hour around it (§7.2). The clock template is not changed; the insert stays when the hour is rebuilt, and a template insert at the same time is replaced for that hour. Checks that need no build are refused per field; the operation fails without a change (reason insert_does_not_fit or insert_blocked) when the track cannot be placed whole.",
		Tags:          tags,
		DefaultStatus: http.StatusAccepted,
		Errors:        errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *addHourInsertInput) (*operationAcceptedOutput, error) {
		id, err := svc.AddInsert(ctx, rotation.AddInsertRequest{
			HourStart: in.Body.HourStartUTC.UTC(), OffsetMS: in.Body.OffsetMS, ToleranceMS: in.Body.ToleranceMS, TrackID: in.Body.TrackID,
		})
		if err != nil {
			return nil, err
		}
		return &operationAcceptedOutput{Body: dto.OperationAccepted{OperationID: id}}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "remove-hour-insert",
		Method:        http.MethodDelete,
		Path:          "/api/track-plan/inserts/{id}",
		Summary:       "Remove a one-time insert",
		Description:   "Removes the insert and rebuilds its hour, as an hour_insert operation. Removing a rescheduled missed item opens the missed item again. Refused once the hour is frozen (hour_started).",
		Tags:          tags,
		DefaultStatus: http.StatusAccepted,
		Errors:        errorsWith(http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*operationAcceptedOutput, error) {
		id, err := svc.RemoveInsert(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &operationAcceptedOutput{Body: dto.OperationAccepted{OperationID: id}}, nil
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

// startRangeRebuild starts a clock_build operation that rebuilds the future
// hours of r and returns its ID; nil when it could not be started (the
// change itself is saved; the hours keep what they were planned with).
func startRangeRebuild(ctx context.Context, log *slog.Logger, svc Rotation, r rotation.Range) *uint64 {
	id, err := svc.RebuildRange(ctx, r)
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
		GridSlotID: h.GridSlotID, Seed: h.Seed, BuiltAt: h.BuiltAt.UTC(),
		Items: make([]dto.TrackPlanItem, len(h.Items)), Inserts: make([]dto.TrackPlanInsert, len(h.Inserts)),
	}
	for i, it := range h.Items {
		out.Items[i] = dto.TrackPlanItem{
			ID: it.ID, TrackID: it.TrackID, Title: it.Title, ArtistName: it.ArtistName, DurationMS: it.DurationMS,
			InsertID: it.InsertID, HourInsertID: it.HourInsertID, PlannedStartUTC: it.Start.UTC(), PlannedInMS: it.InMS,
			PlannedOutMS: it.OutMS, Truncated: it.Truncated, Source: it.Source, Status: it.Status,
		}
	}
	for i, ins := range h.Inserts {
		out.Inserts[i] = dto.TrackPlanInsert{
			ID: ins.ID, Placement: string(ins.Placement), OffsetMS: ins.OffsetMS, ToleranceMS: ins.ToleranceMS,
			TrackID: ins.TrackID, MissedID: ins.MissedID,
		}
	}
	return out
}

// simulationDTO maps a simulation to its response.
func simulationDTO(s rotation.Simulation) dto.TrackSimulation {
	out := dto.TrackSimulation{
		From: s.From.Format(time.DateOnly), To: s.To.Format(time.DateOnly),
		Hours: make([]dto.TrackSimulationHour, len(s.Hours)), HoursBuilt: len(s.Hours), HoursSkipped: s.Skipped,
		Items: s.Items, WarningsByCode: make(map[string]int, len(s.Warnings)),
		ElapsedMS: s.Elapsed.Milliseconds(), Queries: s.Queries, LibraryTracks: s.LibraryTracks, HistoryPlays: s.HistoryPlays,
	}
	for code, n := range s.Warnings {
		out.WarningsByCode[string(code)] = n
	}
	for i, h := range s.Hours {
		dh := dto.TrackSimulationHour{
			HourStartUTC: h.Start.UTC(), Clock: dto.TrackPlanClock{ID: h.ClockID, Name: h.ClockName}, ItemCount: h.ItemCount,
			Warnings: make([]dto.TrackSimulationWarning, len(h.Warnings)),
		}
		for j, w := range h.Warnings {
			dh.Warnings[j] = dto.TrackSimulationWarning{Code: string(w.Code), Message: w.Message}
		}
		if h.Items != nil {
			dh.Items = make([]dto.TrackSimulationItem, len(h.Items))
			for j, it := range h.Items {
				dh.Items[j] = dto.TrackSimulationItem{
					StartUTC: it.Start.UTC(), TrackID: it.TrackID, Title: it.Title, ArtistName: it.ArtistName,
					PlannedInMS: it.InMS, PlannedOutMS: it.OutMS, Truncated: it.Truncated, Source: string(it.Source),
				}
			}
		}
		out.Hours[i] = dh
	}
	return out
}
