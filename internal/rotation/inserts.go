package rotation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

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
	Placement Placement
	// OffsetMS is the target start of the fixed placement, whole seconds.
	OffsetMS *int
	// ToleranceMS is the start tolerance of the fixed placement; nil takes
	// the default of new inserts.
	ToleranceMS *int
}

// AddInsertRequest adds a track to one planned hour at a time with seconds
// (today's news).
type AddInsertRequest struct {
	HourStart time.Time
	OffsetMS  int
	// ToleranceMS nil takes the default of new inserts.
	ToleranceMS *int
	TrackID     uint64
}

// PinRequest pins a planned fill track of an open hour at a new time: a
// one-time insert of its track, fixed at OffsetMS (§7.2, pins).
type PinRequest struct {
	PlanItemID uint64
	OffsetMS   int
	// ToleranceMS nil takes the default of new inserts.
	ToleranceMS *int
}

// MoveInsertRequest moves a one-time insert inside its hour.
type MoveInsertRequest struct {
	ID       uint64
	OffsetMS int
	// ToleranceMS nil keeps the tolerance the insert has.
	ToleranceMS *int
}

// insertOp is a queued operation of a one-time insert: add (a reschedule,
// a track of the manager, or a pin), move, or remove.
type insertOp struct {
	add    *Insert
	move   *Insert
	remove *Insert
}

// target is the insert the operation is about.
func (op insertOp) target() *Insert {
	switch {
	case op.add != nil:
		return op.add
	case op.move != nil:
		return op.move
	default:
		return op.remove
	}
}

// Reschedule checks a request to place a missed item into a later hour and
// starts the reschedule operation, which places it (§7.1). The checks that
// need no build are refused here, per field; whether the track passes the
// rules at its place is known only when the hour is built, and a refusal
// there fails the operation without a change. ctx is the request context:
// it bounds the checks and the wait for a queue slot.
func (s *Service) Reschedule(ctx context.Context, r RescheduleRequest) (uint64, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return 0, fmt.Errorf("read rotation settings: %w", err)
	}
	m, err := s.store.Missed(rctx, r.MissedID)
	if err != nil {
		return 0, err
	}
	if m.Resolution != outage.ResolutionOpen {
		return 0, outage.ErrMissedResolved
	}
	const hourField = "target_hour_start_utc"
	if err = s.checkTarget(rctx, hourField, r.HourStart, set); err != nil {
		return 0, err
	}
	if _, _, err = s.gridHour(rctx, hourField, r.HourStart); err != nil {
		return 0, err
	}
	lib, err := libraryFor(rctx, s.store, LibraryScope{Tracks: []uint64{m.TrackID}})
	if err != nil {
		return 0, err
	}
	t, ok := lib.track(m.TrackID)
	if !ok {
		return 0, validation.Field("id", validation.Invalid, "the track of the missed item has no duration")
	}
	ins := Insert{
		HourStart: r.HourStart, TrackID: m.TrackID, MissedID: &m.ID, Placement: r.Placement, OffsetMS: r.OffsetMS,
		ToleranceMS: set.DefaultFixedToleranceMS,
	}
	if r.ToleranceMS != nil {
		ins.ToleranceMS = *r.ToleranceMS
	}
	if err = checkPlacement(ins, t.DurationMS); err != nil {
		return 0, err
	}
	id, err := s.ops.Start(ctx, operations.KindReschedule)
	if err != nil {
		return 0, err
	}
	s.enqueueOp(ctx, rebuild{opID: id, insert: &insertOp{add: &ins}})
	return id, nil
}

// AddInsert checks a request to add a track to a planned hour and starts
// the hour_insert operation, which stores the insert and rebuilds the hour
// in one transaction; it fails without a change when the track cannot be
// placed whole (§7.2).
func (s *Service) AddInsert(ctx context.Context, r AddInsertRequest) (uint64, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return 0, fmt.Errorf("read rotation settings: %w", err)
	}
	const hourField = "hour_start_utc"
	if err = s.checkTarget(rctx, hourField, r.HourStart, set); err != nil {
		return 0, err
	}
	if _, _, err = s.gridHour(rctx, hourField, r.HourStart); err != nil {
		return 0, err
	}
	lib, err := libraryFor(rctx, s.store, LibraryScope{Tracks: []uint64{r.TrackID}})
	if err != nil {
		return 0, err
	}
	t, ok := lib.track(r.TrackID)
	if !ok {
		return 0, validation.Field("track_id", validation.UnknownID, "no track with a duration has ID %d", r.TrackID)
	}
	ins := Insert{
		HourStart: r.HourStart, TrackID: r.TrackID, Placement: PlacementFixed, OffsetMS: new(r.OffsetMS),
		ToleranceMS: set.DefaultFixedToleranceMS,
	}
	if r.ToleranceMS != nil {
		ins.ToleranceMS = *r.ToleranceMS
	}
	if err = checkPlacement(ins, t.DurationMS); err != nil {
		return 0, err
	}
	id, err := s.ops.Start(ctx, operations.KindHourInsert)
	if err != nil {
		return 0, err
	}
	s.enqueueOp(ctx, rebuild{opID: id, insert: &insertOp{add: &ins}})
	return id, nil
}

// PinFill pins a planned fill track at a new time inside its hour. It stores
// a one-time insert of the same track, fixed at the offset, and rebuilds the
// hour in the same unit of work as the insert (runInsert). The hour is kept
// around the pin: the track stays until the end of the hour, and a rebuild
// keeps it. A frozen hour is refused (ErrHourStarted); an item that is not a
// planned fill track is refused (ErrNotMovable). A pin the builder cannot
// place fails the operation with the reason and changes nothing.
func (s *Service) PinFill(ctx context.Context, r PinRequest) (uint64, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return 0, fmt.Errorf("read rotation settings: %w", err)
	}
	it, err := s.store.PlanItem(rctx, r.PlanItemID)
	if err != nil {
		return 0, err
	}
	if set.frozen(it.HourStart, s.now().UTC()) {
		return 0, ErrHourStarted
	}
	if it.Source != SourceFill || it.Status != planItemPlanned {
		return 0, ErrNotMovable
	}
	lib, err := libraryFor(rctx, s.store, LibraryScope{Tracks: []uint64{it.TrackID}})
	if err != nil {
		return 0, err
	}
	t, ok := lib.track(it.TrackID)
	if !ok {
		return 0, validation.Field("plan_item_id", validation.Invalid, "the track of the item has no duration")
	}
	ins := Insert{
		HourStart: it.HourStart, TrackID: it.TrackID, Placement: PlacementFixed, OffsetMS: new(r.OffsetMS),
		ToleranceMS: set.DefaultFixedToleranceMS,
	}
	if r.ToleranceMS != nil {
		ins.ToleranceMS = *r.ToleranceMS
	}
	if err = checkPlacement(ins, t.DurationMS); err != nil {
		return 0, err
	}
	id, err := s.ops.Start(ctx, operations.KindHourInsert)
	if err != nil {
		return 0, err
	}
	s.enqueueOp(ctx, rebuild{opID: id, insert: &insertOp{add: &ins}})
	return id, nil
}

// MoveInsert moves a one-time insert inside its hour and starts the
// hour_insert operation that stores the new offset and rebuilds the hour in
// one transaction. An insert with no fixed time (start or end) is refused
// (ErrNotMovable); a frozen hour is refused (ErrHourStarted). When the
// builder cannot place the insert at its new place, the operation fails with
// the reason and the insert keeps its old offset.
func (s *Service) MoveInsert(ctx context.Context, r MoveInsertRequest) (uint64, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return 0, fmt.Errorf("read rotation settings: %w", err)
	}
	cur, err := s.store.Insert(rctx, r.ID)
	if err != nil {
		return 0, err
	}
	if set.frozen(cur.HourStart, s.now().UTC()) {
		return 0, ErrHourStarted
	}
	if cur.Placement != PlacementFixed {
		return 0, ErrNotMovable
	}
	lib, err := libraryFor(rctx, s.store, LibraryScope{Tracks: []uint64{cur.TrackID}})
	if err != nil {
		return 0, err
	}
	t, ok := lib.track(cur.TrackID)
	if !ok {
		return 0, validation.Field("id", validation.Invalid, "the track of the insert has no duration")
	}
	moved := cur
	moved.OffsetMS = new(r.OffsetMS)
	if r.ToleranceMS != nil {
		moved.ToleranceMS = *r.ToleranceMS
	}
	if err = checkPlacement(moved, t.DurationMS); err != nil {
		return 0, err
	}
	id, err := s.ops.Start(ctx, operations.KindHourInsert)
	if err != nil {
		return 0, err
	}
	s.enqueueOp(ctx, rebuild{opID: id, insert: &insertOp{move: &moved}})
	return id, nil
}

// RemoveInsert starts the hour_insert operation that removes a one-time
// insert and rebuilds its hour. The insert of a rescheduled missed item
// opens the missed item again. A frozen hour is refused (ErrHourStarted).
func (s *Service) RemoveInsert(ctx context.Context, id uint64) (uint64, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return 0, fmt.Errorf("read rotation settings: %w", err)
	}
	ins, err := s.store.Insert(rctx, id)
	if err != nil {
		return 0, err
	}
	if set.frozen(ins.HourStart, s.now().UTC()) {
		return 0, ErrHourStarted
	}
	opID, err := s.ops.Start(ctx, operations.KindHourInsert)
	if err != nil {
		return 0, err
	}
	s.enqueueOp(ctx, rebuild{opID: opID, insert: &insertOp{remove: &ins}})
	return opID, nil
}

// checkPlacement refuses a one-time insert whose placement is incomplete or
// which cannot fit the hour whole even at the earliest start of its window.
func checkPlacement(ins Insert, lengthMS int) error {
	errs := &validation.Error{}
	switch ins.Placement {
	case PlacementStart, PlacementEnd:
		if ins.OffsetMS != nil {
			errs.Add("offset_ms", validation.Invalid, "only a fixed placement has a time")
		}
		if lengthMS > clocks.HourMS {
			errs.Add("placement", validation.DoesNotFit, "the track is longer than the hour")
		}
	case PlacementFixed:
		switch {
		case ins.OffsetMS == nil:
			errs.Add("offset_ms", validation.Required, "a fixed placement needs its time")
		case *ins.OffsetMS < 0 || *ins.OffsetMS > clocks.MaxOffsetMS:
			errs.Add("offset_ms", validation.Range, "must be between 0 (00:00) and %d (59:59)", clocks.MaxOffsetMS)
		case *ins.OffsetMS%1000 != 0:
			errs.Add("offset_ms", validation.Format, "must be whole seconds")
		case max(*ins.OffsetMS-ins.ToleranceMS, 0)+lengthMS > clocks.HourMS:
			errs.Add("offset_ms", validation.DoesNotFit, "even from the earliest start of its window the track runs past the end of the hour")
		}
	default:
		errs.Add("placement", validation.Invalid, "must be start, end, or fixed")
	}
	if ins.ToleranceMS < 0 || ins.ToleranceMS > clocks.MaxToleranceMS {
		errs.Add("tolerance_ms", validation.Range, "must be between 0 and %d", clocks.MaxToleranceMS)
	}
	return errs.Err()
}

// checkTarget refuses a target hour that is not a station hour start, is
// frozen (on air, or its items may be pushed already), or lies beyond the
// plan. field names the request field.
func (s *Service) checkTarget(ctx context.Context, field string, start time.Time, set PlanSettings) error {
	now := s.now().UTC()
	l := start.In(set.Location)
	switch {
	case !time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, set.Location).Equal(start):
		return validation.Field(field, validation.Invalid, "must be the start of a station hour")
	case set.frozen(start, now):
		return validation.Field(field, validation.Range,
			"the hour is on air, over, or starts within %d minutes; choose a later hour", int(set.Freeze.Minutes()))
	case start.After(planHorizon(now, set)):
		return validation.Field(field, validation.Range, "the track plan covers the next %d days", set.PlanHorizonDays)
	}
	return ctx.Err()
}

// errNoGridHour: the grid assigns no clock to the hour, or its clock was
// deleted.
var errNoGridHour = errors.New("the grid assigns no clock to that hour")

// gridHour returns the grid hour that starts at start and its clock; a
// field error when the grid assigns no clock to it.
func (s *Service) gridHour(ctx context.Context, field string, start time.Time) (grid.ScheduledHour, clocks.Clock, error) {
	h, c, err := s.findGridHour(ctx, start)
	if errors.Is(err, errNoGridHour) {
		return grid.ScheduledHour{}, clocks.Clock{}, validation.Field(field, validation.Invalid, "the grid assigns no clock to that hour")
	}
	return h, c, err
}

func (s *Service) findGridHour(ctx context.Context, start time.Time) (grid.ScheduledHour, clocks.Clock, error) {
	hours, err := s.grid.Hours(ctx, start, start.Add(time.Hour))
	if err != nil {
		return grid.ScheduledHour{}, clocks.Clock{}, fmt.Errorf("read grid hour: %w", err)
	}
	i := slices.IndexFunc(hours, func(h grid.ScheduledHour) bool { return h.StartUTC.Equal(start) })
	if i < 0 {
		return grid.ScheduledHour{}, clocks.Clock{}, errNoGridHour
	}
	c, err := s.clock(ctx, map[uint64]*clocks.Clock{}, hours[i].Clock.ID)
	if err != nil {
		return grid.ScheduledHour{}, clocks.Clock{}, err
	}
	if c == nil {
		return grid.ScheduledHour{}, clocks.Clock{}, errNoGridHour
	}
	return hours[i], *c, nil
}

// runInsert runs a queued insert operation: in one transaction it stores
// or deletes the insert and rebuilds the hour. Nothing changes when an
// added insert cannot be placed whole.
func (s *Service) runInsert(ctx context.Context, op insertOp) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return "", fmt.Errorf("read rotation settings: %w", err)
	}
	ins := op.target()
	h, c, err := s.findGridHour(rctx, ins.HourStart)
	switch {
	case errors.Is(err, errNoGridHour) && op.remove != nil:
		// The hour is no longer planned: only the insert goes.
		return s.removeUnplanned(ctx, *op.remove, set)
	case errors.Is(err, errNoGridHour):
		return "", &InsertError{Reason: ReasonDoesNotFit, Message: "the grid no longer assigns a clock to that hour"}
	case err != nil:
		return "", err
	}
	lib, err := libraryFor(rctx, s.store, scopeOf([]clocks.Clock{c}, []Insert{*ins}))
	if err != nil {
		return "", err
	}
	j := hourJob{start: ins.HourStart, slotID: new(h.SlotID), clock: c, force: true, add: op.add, move: op.move, remove: op.remove}
	out, err := s.build(ctx, j, &lib, set)
	if err != nil {
		return "", err
	}
	what := fmt.Sprintf("track %d", ins.TrackID)
	if ins.MissedID != nil {
		what = fmt.Sprintf("missed item %d", *ins.MissedID)
	}
	if op.remove != nil {
		return fmt.Sprintf("one-time insert of %s removed from %s", what, ins.HourStart.In(set.Location).Format("2006-01-02 15:04")), nil
	}
	if out.placed != nil {
		at := ins.HourStart.Add(time.Duration(out.placed.OffsetMS) * time.Millisecond)
		return fmt.Sprintf("%s placed at %s", what, at.In(set.Location).Format(time.TimeOnly)), nil
	}
	return what + " placed", nil
}

// removeUnplanned deletes a one-time insert of an hour the grid no longer
// assigns.
func (s *Service) removeUnplanned(ctx context.Context, ins Insert, set PlanSettings) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hourTimeout)
	defer cancel()
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.LockPlan(ctx); err != nil {
			return err
		}
		if set.frozen(ins.HourStart, s.now().UTC()) {
			return ErrHourStarted
		}
		return dropInserts(ctx, st, []Insert{ins})
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("one-time insert %d removed", ins.ID), nil
}

// dropInserts removes one-time inserts and reopens their missed items, so
// the manager can reschedule them again.
func dropInserts(ctx context.Context, st Store, inserts []Insert) error {
	for _, ins := range inserts {
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

// storeInsert stores a one-time insert under the plan lock; the missed item
// of a reschedule must still be open.
func (s *Service) storeInsert(ctx context.Context, st Store, ins Insert) (uint64, error) {
	if ins.MissedID != nil {
		m, err := st.LockMissed(ctx, *ins.MissedID)
		if err != nil {
			return 0, err
		}
		if m.Resolution != outage.ResolutionOpen {
			return 0, outage.ErrMissedResolved
		}
	}
	return st.CreateInsert(ctx, ins)
}
