package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
)

// Clocks manages clock templates (*clocks.Service).
type Clocks interface {
	List(ctx context.Context) ([]clocks.Summary, error)
	Get(ctx context.Context, id int64) (clocks.View, error)
	Create(ctx context.Context, in clocks.Input) (clocks.Result, error)
	Update(ctx context.Context, id int64, in clocks.Input) (clocks.Result, error)
	SaveWithFiller(ctx context.Context, id int64, choice clocks.FillerChoice) (clocks.Result, error)
	SaveIncomplete(ctx context.Context, id int64) (clocks.Result, error)
	Delete(ctx context.Context, id int64) error
	Pool(ctx context.Context, id int64) ([]clocks.PoolEntry, error)
	SetPool(ctx context.Context, id int64, trackIDs []int64) ([]clocks.PoolEntry, error)
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
	ID   int64 `path:"id" minimum:"1"`
	Body dto.ClockBody
}

type saveWithFillerInput struct {
	ID   int64 `path:"id" minimum:"1"`
	Body dto.SaveWithFillerBody
}

type fillerPoolOutput struct {
	Body dto.FillerPool
}

type setFillerPoolInput struct {
	ID   int64 `path:"id" minimum:"1"`
	Body dto.SetFillerPool
}

func registerClocks(api huma.API, log *slog.Logger, svc Clocks) {
	tags := []string{"Clocks"}
	saveErrors := errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity)

	register(api, log, huma.Operation{
		OperationID: "list-clocks",
		Method:      http.MethodGet,
		Path:        "/api/clocks",
		Summary:     "List clocks",
		Description: "Clocks with their status and element count, in name order.",
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
				ID: c.ID, Name: c.Name, Status: string(c.Status), ElementCount: c.ElementCount, UpdatedAt: c.UpdatedAt.UTC(),
			})
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "get-clock",
		Method:      http.MethodGet,
		Path:        "/api/clocks/{id}",
		Summary:     "Get a clock",
		Description: "The clock with its elements and its layout on nominal durations: segments, shortfall, and the element that holds each segment's filler.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*clockOutput, error) {
		v, err := svc.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &clockOutput{Body: clockDTO(v.Clock, v.Layout)}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "create-clock",
		Method:        http.MethodPost,
		Path:          "/api/clocks",
		Summary:       "Create a clock",
		Description:   "Runs the dry check and saves the clock (§6.0). A refusal names the element field. A short clock is saved incomplete with requires_filler_choice: the panel then opens the filler dialog and calls save-with-filler or save-incomplete.",
		Tags:          tags,
		DefaultStatus: http.StatusCreated,
		Errors:        errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *createClockInput) (*clockSaveOutput, error) {
		r, err := svc.Create(ctx, clockInput(in.Body))
		if err != nil {
			return nil, err
		}
		return &clockSaveOutput{Body: saveResultDTO(r)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "update-clock",
		Method:      http.MethodPut,
		Path:        "/api/clocks/{id}",
		Summary:     "Update a clock",
		Description: "Replaces the clock and runs the dry check, like create. Elements are matched by id; elements left out are removed. A new fixed element whose start window overlaps an existing one replaces it (check.replaced_elements).",
		Tags:        tags,
		Errors:      saveErrors,
	}, func(ctx context.Context, in *updateClockInput) (*clockSaveOutput, error) {
		r, err := svc.Update(ctx, in.ID, clockInput(in.Body))
		if err != nil {
			return nil, err
		}
		return &clockSaveOutput{Body: saveResultDTO(r)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "save-clock-with-filler",
		Method:      http.MethodPost,
		Path:        "/api/clocks/{id}/save-with-filler",
		Summary:     "Save the filler choice of a short clock",
		Description: "Stores the filler of each short segment on the element that holds it, and the pool if given. Every short segment must have a filler afterwards, so the clock becomes complete.",
		Tags:        tags,
		Errors:      saveErrors,
	}, func(ctx context.Context, in *saveWithFillerInput) (*clockSaveOutput, error) {
		choice := clocks.FillerChoice{PoolTrackIDs: in.Body.PoolTrackIDs}
		for _, s := range in.Body.Segments {
			choice.Segments = append(choice.Segments, clocks.SegmentChoice{
				Segment: s.Segment,
				Filler:  clocks.Filler{Mode: clocks.FillerMode(s.FillerMode), SourceID: s.FillerSourceID},
			})
		}
		r, err := svc.SaveWithFiller(ctx, in.ID, choice)
		if err != nil {
			return nil, err
		}
		return &clockSaveOutput{Body: saveResultDTO(r)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "save-clock-incomplete",
		Method:      http.MethodPost,
		Path:        "/api/clocks/{id}/save-incomplete",
		Summary:     "Keep a short clock incomplete",
		Description: "Closes the filler dialog without a choice: the clock stays incomplete and shows its warning. Reminders for its hours are created by the planner.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*clockSaveOutput, error) {
		r, err := svc.SaveIncomplete(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &clockSaveOutput{Body: saveResultDTO(r)}, nil
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

	register(api, log, huma.Operation{
		OperationID: "get-clock-filler-pool",
		Method:      http.MethodGet,
		Path:        "/api/clocks/{id}/filler-pool",
		Summary:     "Get the filler pool of a clock",
		Description: "Specific tracks used in order when the clock needs filler in pool mode. Used tracks stay as history.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*fillerPoolOutput, error) {
		p, err := svc.Pool(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &fillerPoolOutput{Body: poolDTO(p)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "set-clock-filler-pool",
		Method:      http.MethodPut,
		Path:        "/api/clocks/{id}/filler-pool",
		Summary:     "Set the filler pool of a clock",
		Description: "Replaces the unused tracks of the pool, in the given order; used ones stay.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *setFillerPoolInput) (*fillerPoolOutput, error) {
		p, err := svc.SetPool(ctx, in.ID, in.Body.TrackIDs)
		if err != nil {
			return nil, err
		}
		return &fillerPoolOutput{Body: poolDTO(p)}, nil
	})
}

func clockInput(b dto.ClockBody) clocks.Input {
	in := clocks.Input{Name: b.Name, Elements: make([]clocks.ElementInput, len(b.Elements))}
	if b.DefaultFillerMode != nil {
		in.DefaultFiller = &clocks.Filler{Mode: clocks.FillerMode(*b.DefaultFillerMode), SourceID: b.DefaultFillerSourceID}
	} else if b.DefaultFillerSourceID != nil {
		// A source without a mode is refused by the service.
		in.DefaultFiller = &clocks.Filler{Mode: clocks.FillerRandom, SourceID: b.DefaultFillerSourceID}
	}
	for i, e := range b.Elements {
		el := clocks.ElementInput{
			Placement: clocks.Placement(e.Placement), FixedMinute: e.FixedMinute,
			FixedToleranceMS: e.FixedToleranceMS, NominalDurationMS: e.NominalDurationMS,
			DurationToleranceMS: e.DurationToleranceMS,
			Filler:              clocks.Filler{Mode: clocks.FillerMode(e.FillerMode), SourceID: e.FillerSourceID},
			PlaylistIDs:         e.PlaylistIDs,
		}
		if el.Placement == "" {
			el.Placement = clocks.PlacementFlow
		}
		if e.ID != nil {
			el.ID = *e.ID
		}
		in.Elements[i] = el
	}
	return in
}

func elementDTO(e clocks.Element) dto.ClockElement {
	return dto.ClockElement{
		ID: e.ID, Placement: string(e.Placement), FixedMinute: e.FixedMinute,
		FixedToleranceMS: e.FixedToleranceMS, NominalDurationMS: e.NominalDurationMS,
		DurationToleranceMS: e.DurationToleranceMS, FillerMode: string(e.Filler.Mode),
		FillerSourceID: e.Filler.SourceID, PlaylistIDs: emptyIDs(e.PlaylistIDs),
	}
}

func clockDTO(c clocks.Clock, l clocks.Layout) dto.Clock {
	out := dto.Clock{
		ID: c.ID, Name: c.Name, Status: string(c.Status), UpdatedAt: c.UpdatedAt.UTC(),
		Elements: make([]dto.ClockElement, len(c.Elements)),
		Layout:   dto.ClockLayout{ShortfallMS: l.ShortfallMS, Segments: make([]dto.ClockSegment, len(l.Segments))},
	}
	if c.DefaultFiller != nil {
		out.DefaultFillerMode = new(string(c.DefaultFiller.Mode))
		out.DefaultFillerSourceID = c.DefaultFiller.SourceID
	}
	for i, e := range c.Elements {
		out.Elements[i] = elementDTO(e)
	}
	for i, s := range l.Segments {
		out.Layout.Segments[i] = dto.ClockSegment{
			Index: s.Index, StartMS: s.StartMS, EndMS: s.EndMS, ContentMS: s.ContentMS,
			ShortfallMS: s.ShortfallMS, OverflowMS: s.OverflowMS,
			ElementIndexes: append([]int{}, s.Elements...), HolderIndex: s.Holder,
			FillerMode:    string(c.Elements[s.Holder].Filler.Mode),
			ClosedByIndex: s.ClosedBy, FixedStartMS: s.FixedStartMS,
		}
	}
	return out
}

func saveResultDTO(r clocks.Result) dto.ClockSaveResult {
	check := dto.ClockCheck{
		ShortfallMS:      r.Layout.ShortfallMS,
		ShortSegments:    []int{},
		Warnings:         make([]dto.ClockWarning, len(r.Warnings)),
		ReplacedElements: make([]dto.ClockElement, len(r.Replaced)),
	}
	for _, s := range r.Layout.Uncovered(r.Clock.Elements) {
		check.ShortSegments = append(check.ShortSegments, s.Index)
	}
	check.RequiresFillerChoice = len(check.ShortSegments) > 0
	for i, w := range r.Warnings {
		dw := dto.ClockWarning{Code: string(w.Code), Message: w.Message, ElementIndex: w.Element, SegmentIndex: w.Segment}
		for _, h := range w.Hours {
			dw.Hours = append(dw.Hours, dto.WeekdayHour{Weekday: h.Weekday, Hour: h.Hour})
		}
		check.Warnings[i] = dw
	}
	for i, e := range r.Replaced {
		check.ReplacedElements[i] = elementDTO(e)
	}
	return dto.ClockSaveResult{Clock: clockDTO(r.Clock, r.Layout), Check: check}
}

func poolDTO(p []clocks.PoolEntry) dto.FillerPool {
	out := dto.FillerPool{Items: make([]dto.FillerPoolEntry, len(p))}
	for i, e := range p {
		out.Items[i] = dto.FillerPoolEntry{TrackID: e.TrackID, Title: e.Title, Position: e.Position, UsedAt: e.UsedAt}
	}
	return out
}
