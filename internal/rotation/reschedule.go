package rotation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Insert is a one-time element of one hour (hour_inserts): a missed item
// rescheduled into it (§7.1, §7.2).
type Insert struct {
	ID        uint64
	HourStart time.Time
	TrackID   uint64
	// MissedID is the missed play log row; nil when retention deleted it.
	MissedID         *uint64
	Placement        clocks.Placement
	FixedMinute      *int
	FixedToleranceMS int
}

// MissedItem is a missed play log row, as rescheduling reads it.
type MissedItem struct {
	ID         uint64
	TrackID    uint64
	Resolution outage.Resolution
}

// RescheduleRequest places a missed item into a later hour.
type RescheduleRequest struct {
	MissedID  uint64
	HourStart time.Time
	Placement clocks.Placement
	// FixedMinute and FixedToleranceMS are for the fixed placement; a nil
	// tolerance takes the default of new fixed elements.
	FixedMinute      *int
	FixedToleranceMS *int
}

// Reschedule checks a request to place a missed item into a later hour and
// starts the reschedule operation, which places it (§7.1, decision 10 of
// docs/phase-10-plan.md). The checks that need no build are refused here,
// per field; whether the track passes the rules at its place is known only
// when the hour is built, and a refusal there fails the operation. ctx is
// the request context: it bounds the checks and the wait for a queue slot.
func (s *Service) Reschedule(ctx context.Context, r RescheduleRequest) (uint64, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return 0, fmt.Errorf("read rotation settings: %w", err)
	}
	if r.FixedToleranceMS == nil && r.Placement == clocks.PlacementFixed {
		r.FixedToleranceMS = new(set.DefaultFixedToleranceMS)
	}
	m, err := s.store.Missed(rctx, r.MissedID)
	if err != nil {
		return 0, err
	}
	if m.Resolution != outage.ResolutionOpen {
		return 0, outage.ErrMissedResolved
	}
	if targetErr := s.checkTarget(rctx, r.HourStart, set); targetErr != nil {
		return 0, targetErr
	}
	_, view, err := s.gridHour(rctx, r.HourStart)
	if err != nil {
		return 0, err
	}
	lib, err := s.store.Library(rctx)
	if err != nil {
		return 0, fmt.Errorf("read library: %w", err)
	}
	t, ok := lib.track(m.TrackID)
	if !ok {
		return 0, validation.Field("id", validation.Invalid, "the track of the missed item has no duration")
	}
	el := insertElement(r.Placement, r.FixedMinute, r.FixedToleranceMS, t.DurationMS)
	if _, layoutErr := clocks.WithInsert(view.Clock.Elements, el); layoutErr != nil {
		return 0, layoutErr
	}

	id, err := s.ops.Start(ctx, operations.KindReschedule)
	if err != nil {
		return 0, err
	}
	s.enqueueOp(ctx, rebuild{opID: id, reschedule: &r})
	return id, nil
}

// checkTarget refuses a target hour that is not a station hour start, is
// frozen (on air, or its items may be pushed already), or lies beyond the
// plan.
func (s *Service) checkTarget(ctx context.Context, start time.Time, set PlanSettings) error {
	const field = "target_hour_start_utc"
	now := s.now().UTC()
	l := start.In(set.Location)
	switch {
	case !time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, set.Location).Equal(start):
		return validation.Field(field, validation.Invalid, "must be the start of a station hour")
	case set.frozen(start, now):
		return validation.Field(field, validation.Range,
			"the hour is on air, over, or starts within %d minutes; choose a later hour", int(set.Freeze.Minutes()))
	case start.After(now.Add(time.Duration(set.PlanHorizonHours) * time.Hour)):
		return validation.Field(field, validation.Range, "the track plan covers the next %d hours", set.PlanHorizonHours)
	}
	return ctx.Err()
}

// gridHour returns the grid hour that starts at start and its clock; a
// field error when the grid assigns no clock to it.
func (s *Service) gridHour(ctx context.Context, start time.Time) (grid.ScheduledHour, clocks.View, error) {
	hours, err := s.grid.Hours(ctx, start, start.Add(time.Hour))
	if err != nil {
		return grid.ScheduledHour{}, clocks.View{}, fmt.Errorf("read grid hour: %w", err)
	}
	i := slices.IndexFunc(hours, func(h grid.ScheduledHour) bool { return h.StartUTC.Equal(start) })
	if i < 0 {
		return grid.ScheduledHour{}, clocks.View{}, validation.Field("target_hour_start_utc", validation.Invalid, "the grid assigns no clock to that hour")
	}
	view, err := s.view(ctx, map[uint64]*clocks.View{}, hours[i].Clock.ID)
	if err != nil {
		return grid.ScheduledHour{}, clocks.View{}, err
	}
	if view == nil {
		return grid.ScheduledHour{}, clocks.View{}, validation.Field("target_hour_start_utc", validation.Invalid, "the clock of that hour was deleted")
	}
	return hours[i], *view, nil
}

// insertElement is the element of a one-time insert: its track's length,
// no tolerance (the track is fixed), no filler of its own.
func insertElement(p clocks.Placement, minute, toleranceMS *int, durationMS int) clocks.Element {
	e := clocks.Element{
		Placement: p, FixedMinute: minute, NominalDurationMS: durationMS,
		Filler: clocks.Filler{Mode: clocks.FillerNone},
	}
	if toleranceMS != nil {
		e.FixedToleranceMS = *toleranceMS
	}
	return e
}

// reschedule runs a reschedule operation: in one transaction it stores the
// insert, rebuilds the hour with it, and resolves the missed item. Nothing
// changes when the item cannot be placed whole.
func (s *Service) reschedule(ctx context.Context, r RescheduleRequest) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return "", fmt.Errorf("read rotation settings: %w", err)
	}
	h, view, err := s.gridHour(rctx, r.HourStart)
	if err != nil {
		return "", err
	}
	lib, err := s.store.Library(rctx)
	if err != nil {
		return "", fmt.Errorf("read library: %w", err)
	}
	j := hourJob{start: r.HourStart, slotID: new(h.SlotID), view: view, force: true, reschedule: &r}
	out, err := s.build(ctx, j, &lib, set)
	if err != nil {
		return "", err
	}
	for _, it := range out.plan.Items {
		if it.Source == SourceReschedule && it.RescheduleOfID != nil && *it.RescheduleOfID == r.MissedID {
			at := r.HourStart.Add(time.Duration(it.OffsetMS) * time.Millisecond)
			return fmt.Sprintf("missed item %d placed at %s", r.MissedID, at.In(set.Location).Format(time.TimeOnly)), nil
		}
	}
	return fmt.Sprintf("missed item %d placed", r.MissedID), nil
}

// ErrRescheduleRefused is the failure of a reschedule operation whose item
// cannot be placed whole in its hour; the reason follows it.
var ErrRescheduleRefused = errors.New("the item cannot be placed in that hour")

// applyInserts adds the hour's inserts, in ID order, to the clock and its
// layout: the build input's clock, layout, and pins, the origin of each
// element (its index in the clock; -1 for an insert), the clock elements
// the inserts replace, and the inserts that no longer fit, with the reason.
func applyInserts(c clocks.Clock, l clocks.Layout, inserts []Insert, lib *Library) insertedHour {
	out := insertedHour{clock: c, layout: l, pins: map[int]Pin{}, dropped: map[uint64]string{}}
	out.origin = make([]int, len(c.Elements))
	for i := range out.origin {
		out.origin[i] = i
	}
	for _, ins := range inserts {
		t, ok := lib.track(ins.TrackID)
		if !ok {
			out.dropped[ins.ID] = "the track has no duration"
			continue
		}
		tol := &ins.FixedToleranceMS
		r, err := clocks.WithInsert(out.clock.Elements, insertElement(ins.Placement, ins.FixedMinute, tol, t.DurationMS))
		if err != nil {
			var v *validation.Error
			reason := err.Error()
			if errors.As(err, &v) && len(v.Fields) > 0 {
				reason = v.Fields[0].Message
			}
			out.dropped[ins.ID] = reason
			continue
		}
		pins := map[int]Pin{}
		kept := map[uint64]bool{}
		origin := make([]int, len(r.Elements))
		for i, prev := range r.Origin {
			origin[i] = -1
			if prev < 0 {
				continue
			}
			origin[i] = out.origin[prev]
			if p, ok := out.pins[prev]; ok {
				pins[i], kept[p.InsertID] = p, true
			}
		}
		for _, p := range out.pins {
			if !kept[p.InsertID] {
				out.dropped[p.InsertID] = "a later rescheduled item at a fixed minute replaced it"
			}
		}
		pins[r.Index] = Pin{InsertID: ins.ID, TrackID: ins.TrackID, MissedID: ins.MissedID}
		for _, e := range r.Replaced {
			if e.ID != 0 {
				out.replaced = append(out.replaced, e)
			}
		}
		out.clock.Elements, out.layout, out.pins, out.origin = r.Elements, r.Layout, pins, origin
	}
	return out
}

// insertedHour is a clock with the inserts of one hour.
type insertedHour struct {
	clock    clocks.Clock
	layout   clocks.Layout
	pins     map[int]Pin
	origin   []int
	replaced []clocks.Element
	dropped  map[uint64]string
}

// remapWarnings points the element indexes of warnings back to the clock's
// elements; a warning of an insert has no element.
func remapWarnings(ws []Warning, origin []int) {
	for i := range ws {
		if e := ws[i].Element; e != nil && *e < len(origin) {
			if origin[*e] < 0 {
				ws[i].Element = nil
			} else {
				ws[i].Element = new(origin[*e])
			}
		}
	}
}

// dropInserts removes inserts that cannot be placed and reopens their
// missed items, so the manager can reschedule them again.
func dropInserts(ctx context.Context, st Store, inserts []Insert, ids []uint64) error {
	for _, ins := range inserts {
		if !slices.Contains(ids, ins.ID) {
			continue
		}
		if err := st.DeleteInsert(ctx, ins.ID); err != nil {
			return err
		}
		if ins.MissedID != nil {
			if err := st.ReopenMissed(ctx, *ins.MissedID); err != nil {
				return err
			}
		}
	}
	return nil
}

// linkRescheduled resolves the missed items whose rescheduled item is in
// the built hour, pointing at the new plan row.
func linkRescheduled(ctx context.Context, st Store, items []Item, at time.Time) error {
	for _, it := range items {
		if it.Source != SourceReschedule || it.RescheduleOfID == nil {
			continue
		}
		if err := st.LinkRescheduled(ctx, *it.RescheduleOfID, at); err != nil {
			return err
		}
	}
	return nil
}

// storeInsert checks under the plan lock that the missed item is still
// open and stores its insert.
func (s *Service) storeInsert(ctx context.Context, st Store, r RescheduleRequest) (uint64, error) {
	m, err := st.LockMissed(ctx, r.MissedID)
	if err != nil {
		return 0, err
	}
	if m.Resolution != outage.ResolutionOpen {
		return 0, outage.ErrMissedResolved
	}
	ins := Insert{
		HourStart: r.HourStart, TrackID: m.TrackID, MissedID: &m.ID, Placement: r.Placement, FixedMinute: r.FixedMinute,
	}
	if r.FixedToleranceMS != nil {
		ins.FixedToleranceMS = *r.FixedToleranceMS
	}
	return st.CreateInsert(ctx, ins)
}

// settleInserts finishes the inserts of a build: the refusal of the
// reschedule being placed fails the build; other inserts that no longer fit
// are dropped with a warning and their missed items reopened; warnings
// point at the clock's elements again, and replaced elements are named.
func (s *Service) settleInserts(ctx context.Context, st Store, j hourJob, insertID uint64, inserts []Insert,
	ih *insertedHour, plan *HourPlan,
) error {
	if insertID != 0 {
		if reason, ok := ih.dropped[insertID]; ok {
			return fmt.Errorf("%w: %s", ErrRescheduleRefused, reason)
		}
		if slices.Contains(plan.PinsRefused, insertID) {
			reason := "it breaks a rule there or does not fit"
			for _, w := range plan.Warnings {
				if w.Code == WarnRescheduleDropped && w.Element != nil && ih.pins[*w.Element].InsertID == insertID {
					reason = w.Message
				}
			}
			return fmt.Errorf("%w: %s", ErrRescheduleRefused, reason)
		}
	}
	remapWarnings(plan.Warnings, ih.origin)
	drop := slices.Clone(plan.PinsRefused)
	for _, ins := range inserts {
		reason, ok := ih.dropped[ins.ID]
		if !ok {
			continue
		}
		drop = append(drop, ins.ID)
		plan.Warnings = append(plan.Warnings, Warning{
			Code: WarnRescheduleDropped, TrackID: new(ins.TrackID),
			Message: "the rescheduled item cannot be placed: " + reason,
		})
	}
	for _, e := range ih.replaced {
		i := slices.IndexFunc(j.view.Clock.Elements, func(c clocks.Element) bool { return c.ID == e.ID })
		w := Warning{Code: WarnElementReplaced, Message: fmt.Sprintf("a rescheduled item replaces the fixed element at :%02d in this hour", *e.FixedMinute)}
		if i >= 0 {
			w.Element = new(i)
		}
		plan.Warnings = append(plan.Warnings, w)
	}
	return dropInserts(ctx, st, inserts, drop)
}
