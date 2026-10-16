package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

// Clocks manages clock templates (*clocks.Service).
type Clocks interface {
	List(ctx context.Context) ([]clocks.Summary, error)
	Get(ctx context.Context, id uint64) (clocks.Clock, error)
	Create(ctx context.Context, in clocks.Input) (clocks.Result, error)
	Update(ctx context.Context, id uint64, in clocks.Input) (clocks.Result, error)
	Delete(ctx context.Context, id uint64) error
}

type clockListOutput struct {
	Body dto.ClockList
}

type clockOutput struct {
	Body dto.Clock
}

type clockSaveOutput struct {
	Body dto.ClockSaveResult
}

type createClockInput struct {
	Body dto.ClockBody
}

type updateClockInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.ClockBody
}

func registerClocks(api huma.API, log *slog.Logger, svc Clocks, rot Rotation) {
	tags := []string{"Clocks"}

	register(api, log, huma.Operation{
		OperationID: "list-clocks",
		Method:      http.MethodGet,
		Path:        "/api/clocks",
		Summary:     "List clocks",
		Description: "Clocks with their playlist and insert counts, in name order.",
		Tags:        tags,
		Errors:      errorsWith(),
	}, func(ctx context.Context, _ *struct{}) (*clockListOutput, error) {
		cs, err := svc.List(ctx)
		if err != nil {
			return nil, err
		}
		out := &clockListOutput{Body: dto.ClockList{Items: make([]dto.ClockSummary, 0, len(cs))}}
		for _, c := range cs {
			out.Body.Items = append(out.Body.Items, dto.ClockSummary{
				ID: c.ID, Name: c.Name, PlaylistCount: c.PlaylistCount, InsertCount: c.InsertCount, UpdatedAt: c.UpdatedAt.UTC(),
			})
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "get-clock",
		Method:      http.MethodGet,
		Path:        "/api/clocks/{id}",
		Summary:     "Get a clock",
		Description: "The clock with its playlists (and the effective placement rule of each) and its timed inserts.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*clockOutput, error) {
		c, err := svc.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &clockOutput{Body: clockDTO(c)}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "create-clock",
		Method:        http.MethodPost,
		Path:          "/api/clocks",
		Summary:       "Create a clock",
		Description:   "Validates every field, saves the clock, and returns it with a sample hour and the warnings of the dry check (§6.0). Warnings never block the save. A refusal names the field (playlists[1].fill_weight, inserts[0].offset_ms).",
		Tags:          tags,
		DefaultStatus: http.StatusCreated,
		Errors:        errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *createClockInput) (*clockSaveOutput, error) {
		r, err := svc.Create(ctx, clockInput(in.Body))
		if err != nil {
			return nil, err
		}
		return &clockSaveOutput{Body: saveResultDTO(ctx, log, rot, r)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "update-clock",
		Method:      http.MethodPut,
		Path:        "/api/clocks/{id}",
		Summary:     "Update a clock",
		Description: "Replaces the clock, its playlists, and its inserts, like create, and rebuilds its planned hours (build_operation_id). An insert at the time of a stored one keeps its ID.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *updateClockInput) (*clockSaveOutput, error) {
		r, err := svc.Update(ctx, in.ID, clockInput(in.Body))
		if err != nil {
			return nil, err
		}
		return &clockSaveOutput{Body: saveResultDTO(ctx, log, rot, r)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "delete-clock",
		Method:      http.MethodDelete,
		Path:        "/api/clocks/{id}",
		Summary:     "Delete a clock",
		Description: "Refused while grid slots assign the clock (clock_in_use, with used_by naming the slots) and for good once it has play history (clock_has_history).",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*struct{}, error) {
		return nil, svc.Delete(ctx, in.ID)
	})
}

func clockInput(b dto.ClockBody) clocks.Input {
	in := clocks.Input{Name: b.Name, Playlists: make([]clocks.PlaylistInput, len(b.Playlists)), Inserts: make([]clocks.InsertInput, len(b.Inserts))}
	for i, p := range b.Playlists {
		pi := clocks.PlaylistInput{PlaylistID: p.PlaylistID, Count: p.ClockCount, FillWeight: p.FillWeight}
		if p.ClockRule != nil {
			pi.Rule = new(clocks.Rule(*p.ClockRule))
		}
		in.Playlists[i] = pi
	}
	for i, ins := range b.Inserts {
		in.Inserts[i] = clocks.InsertInput{OffsetMS: ins.OffsetMS, ToleranceMS: ins.ToleranceMS, TrackID: ins.TrackID, PlaylistID: ins.PlaylistID}
	}
	return in
}

func clockDTO(c clocks.Clock) dto.Clock {
	out := dto.Clock{
		ID: c.ID, Name: c.Name, UpdatedAt: c.UpdatedAt.UTC(),
		Playlists: make([]dto.ClockPlaylist, len(c.Playlists)), Inserts: make([]dto.ClockInsert, len(c.Inserts)),
	}
	for i, p := range c.Playlists {
		cp := dto.ClockPlaylist{
			PlaylistID: p.PlaylistID, Position: i, ClockCount: p.Count, FillWeight: p.FillWeight,
			EffectiveRule: string(p.EffectiveRule()), EffectiveCount: p.EffectiveCount(),
		}
		if p.Rule != nil {
			cp.ClockRule = new(string(*p.Rule))
		}
		out.Playlists[i] = cp
	}
	for i, ins := range c.Inserts {
		out.Inserts[i] = dto.ClockInsert{
			ID: ins.ID, OffsetMS: ins.OffsetMS, ToleranceMS: ins.ToleranceMS, TrackID: ins.TrackID, PlaylistID: ins.PlaylistID,
		}
	}
	return out
}

// saveResultDTO builds the answer of a save: the clock, its sample hour,
// the warnings of the dry check and of the sample, and the rebuild of its
// planned hours. The clock is saved already, so a failed sample or rebuild
// start is logged, not returned.
func saveResultDTO(ctx context.Context, log *slog.Logger, rot Rotation, r clocks.Result) dto.ClockSaveResult {
	out := dto.ClockSaveResult{Clock: clockDTO(r.Clock), Warnings: make([]dto.ClockWarning, 0, len(r.Warnings))}
	for _, w := range r.Warnings {
		dw := dto.ClockWarning{Code: string(w.Code), Message: w.Message, PlaylistIndex: w.PlaylistIndex, InsertIndex: w.InsertIndex}
		for _, h := range w.Hours {
			dw.Hours = append(dw.Hours, dto.WeekdayHour{Weekday: h.Weekday, Hour: h.Hour})
		}
		out.Warnings = append(out.Warnings, dw)
	}
	s, err := rot.Sample(ctx, r.Clock)
	if err != nil {
		log.ErrorContext(ctx, "build sample hour failed", "clock_id", r.Clock.ID,
			logging.KeyRequestID, middleware.GetReqID(ctx), logging.KeyError, err)
	} else {
		out.Sample = sampleDTO(s)
		for _, w := range s.Warnings {
			out.Warnings = append(out.Warnings, dto.ClockWarning{
				Code: string(w.Code), Message: w.Message, PlaylistIndex: w.PlaylistIndex, InsertIndex: w.InsertIndex,
			})
		}
	}
	out.BuildOperationID = startRebuild(ctx, log, rot)
	return out
}

func sampleDTO(s rotation.Sample) *dto.ClockSample {
	out := &dto.ClockSample{HourStartUTC: s.HourStart.UTC(), Items: make([]dto.ClockSampleItem, len(s.Items))}
	for i, it := range s.Items {
		out.Items[i] = dto.ClockSampleItem{
			OffsetMS: it.OffsetMS, LengthMS: it.LengthMS, TrackID: it.TrackID, Title: it.Title, Artist: it.ArtistName,
			Source: string(it.Source), PlaylistID: it.PlaylistID, InsertIndex: it.InsertIndex, Truncated: it.Truncated,
		}
	}
	return out
}
