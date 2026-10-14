package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Grid manages the clock grid (*grid.Service).
type Grid interface {
	ListSlots(ctx context.Context, f grid.Filter) ([]grid.Slot, error)
	Assign(ctx context.Context, in grid.AssignInput) (grid.AssignResult, error)
	DeleteSlot(ctx context.Context, id int64) error
	Schedule(ctx context.Context, from, to time.Time) ([]grid.ScheduledHour, error)
}

type listGridSlotsInput struct {
	ClockID int64  `query:"clock_id" minimum:"0" doc:"Only the slots of this clock."`
	From    string `query:"from" format:"date" doc:"Only slots valid on some date from this one (station local)."`
	To      string `query:"to" format:"date" doc:"Only slots valid on some date up to this one."`
}

type gridSlotListOutput struct {
	Body dto.GridSlotList
}

type gridAssignInput struct {
	Body dto.GridAssignBody
}

type gridUnassignInput struct {
	Body dto.GridUnassignBody
}

type gridAssignOutput struct {
	Body dto.GridAssignResult
}

type gridScheduleInput struct {
	From string `query:"from" required:"true" format:"date" doc:"First station local date."`
	To   string `query:"to" required:"true" format:"date" doc:"Last station local date, inclusive; at most 62 days after from."`
}

type gridScheduleOutput struct {
	Body dto.GridSchedule
}

func registerGrid(api huma.API, log *slog.Logger, svc Grid) {
	tags := []string{"Grid"}

	register(api, log, huma.Operation{
		OperationID: "list-grid-slots",
		Method:      http.MethodGet,
		Path:        "/api/grid/slots",
		Summary:     "List grid slots",
		Description: "Slots in weekday, hour, valid_from order. Weekday, hour, and dates are station local.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *listGridSlotsInput) (*gridSlotListOutput, error) {
		f := grid.Filter{ClockID: in.ClockID}
		var err error
		if f.From, err = optionalDate("query.from", in.From); err != nil {
			return nil, err
		}
		if f.To, err = optionalDate("query.to", in.To); err != nil {
			return nil, err
		}
		slots, err := svc.ListSlots(ctx, f)
		if err != nil {
			return nil, err
		}
		return &gridSlotListOutput{Body: dto.GridSlotList{Items: slotsDTO(slots)}}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "assign-grid",
		Method:      http.MethodPost,
		Path:        "/api/grid/assign",
		Summary:     "Assign a clock to grid hours",
		Description: "Creates one slot per weekday and hour, in one transaction. Slots that overlap the new dates are trimmed, split, or deleted (replaced lists them). valid_from lies between today and the grid horizon, station local.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *gridAssignInput) (*gridAssignOutput, error) {
		a, err := assignInput(in.Body.ClockID, in.Body.Weekdays, in.Body.Hours, in.Body.ValidFrom, in.Body.ValidTo)
		if err != nil {
			return nil, err
		}
		r, err := svc.Assign(ctx, a)
		if err != nil {
			return nil, err
		}
		return &gridAssignOutput{Body: assignResultDTO(r)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "unassign-grid",
		Method:      http.MethodPost,
		Path:        "/api/grid/unassign",
		Summary:     "Clear grid hours",
		Description: "Removes every clock from the weekdays and hours for the dates: overlapping slots are trimmed, split, or deleted.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *gridUnassignInput) (*gridAssignOutput, error) {
		a, err := assignInput(0, in.Body.Weekdays, in.Body.Hours, in.Body.ValidFrom, in.Body.ValidTo)
		if err != nil {
			return nil, err
		}
		r, err := svc.Assign(ctx, a)
		if err != nil {
			return nil, err
		}
		return &gridAssignOutput{Body: assignResultDTO(r)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "delete-grid-slot",
		Method:      http.MethodDelete,
		Path:        "/api/grid/slots/{id}",
		Summary:     "Delete a grid slot",
		Description: "The play history of the slot keeps its clock and loses only the slot.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*struct{}, error) {
		return nil, svc.DeleteSlot(ctx, in.ID)
	})

	register(api, log, huma.Operation{
		OperationID: "get-grid-schedule",
		Method:      http.MethodGet,
		Path:        "/api/grid",
		Summary:     "Grid by hour",
		Description: "The assigned hours of the station local dates from ... to, with the UTC start of each hour and its clock. Hours without a clock are left out.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *gridScheduleInput) (*gridScheduleOutput, error) {
		from, err := parseDate("query.from", in.From)
		if err != nil {
			return nil, err
		}
		to, err := parseDate("query.to", in.To)
		if err != nil {
			return nil, err
		}
		hours, err := svc.Schedule(ctx, from, to)
		if err != nil {
			return nil, err
		}
		out := &gridScheduleOutput{Body: dto.GridSchedule{Items: make([]dto.GridHour, len(hours))}}
		for i, h := range hours {
			out.Body.Items[i] = dto.GridHour{
				Date: h.Date.Format(time.DateOnly), Weekday: h.Weekday, Hour: h.Hour,
				StartUTC: h.StartUTC.UTC(), SlotID: h.SlotID, Clock: gridClockDTO(h.Clock),
			}
		}
		return out, nil
	})
}

func parseDate(field, s string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, validation.Field(field, validation.Format, "must be a date (YYYY-MM-DD)")
	}
	return t, nil
}

func optionalDate(field, s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := parseDate(field, s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func assignInput(clockID int64, weekdays, hours []int, from string, to *string) (grid.AssignInput, error) {
	a := grid.AssignInput{ClockID: clockID, Weekdays: weekdays, Hours: hours}
	var err error
	if a.ValidFrom, err = parseDate("valid_from", from); err != nil {
		return grid.AssignInput{}, err
	}
	if to != nil {
		t, err := parseDate("valid_to", *to)
		if err != nil {
			return grid.AssignInput{}, err
		}
		a.ValidTo = &t
	}
	return a, nil
}

func gridClockDTO(c grid.ClockRef) dto.GridClockRef {
	return dto.GridClockRef{ID: c.ID, Name: c.Name, Status: string(c.Status)}
}

func slotDTO(s grid.Slot) dto.GridSlot {
	out := dto.GridSlot{
		ID: s.ID, Weekday: s.Weekday, Hour: s.Hour, Clock: gridClockDTO(s.Clock),
		ValidFrom: s.ValidFrom.Format(time.DateOnly),
	}
	if s.ValidTo != nil {
		out.ValidTo = new(s.ValidTo.Format(time.DateOnly))
	}
	return out
}

func slotsDTO(slots []grid.Slot) []dto.GridSlot {
	out := make([]dto.GridSlot, len(slots))
	for i, s := range slots {
		out[i] = slotDTO(s)
	}
	return out
}

func assignResultDTO(r grid.AssignResult) dto.GridAssignResult {
	out := dto.GridAssignResult{Created: slotsDTO(r.Created), Replaced: make([]dto.GridReplacedSlot, len(r.Replaced))}
	for i, rp := range r.Replaced {
		out.Replaced[i] = dto.GridReplacedSlot{Slot: slotDTO(rp.Slot), Change: string(rp.Change), Remaining: slotsDTO(rp.Remaining)}
	}
	return out
}
