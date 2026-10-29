// Package grid holds the clock grid (§4, §7, §10.2 of the design): which
// clock plays in which hour, up to the grid horizon (30 days) ahead.
//
// A slot assigns a clock to a weekday and an hour, in station local time,
// for a range of local dates (both ends inclusive, the end may be open). At
// most one clock plays in an hour: saving a slot that overlaps others
// replaces them for the overlapping dates, trimming, splitting, or deleting
// them in one transaction, and the result names what was replaced.
//
// The grid is read, not computed: Schedule and ClockHours resolve the slots
// to hours with their UTC start. Rebuilding the planned hours after a change
// belongs to rotation (internal/rotation): the API starts a rebuild after
// every change.
//
// Persistence goes through internal/repository/grid.
package grid

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Errors returned by the service.
var (
	ErrNotFound = errors.New("grid slot not found")
	// ErrConflict: another change of the same hours was saved at the same
	// time (the exclusion constraint refused the write).
	ErrConflict = errors.New("the grid hours were changed at the same time")
)

// ErrUnknownClock is returned by the store when the clock does not exist.
var ErrUnknownClock = errors.New("unknown clock")

// ClockRef is the clock of a slot.
type ClockRef struct {
	ID   uint64
	Name string
}

// Slot assigns a clock to a weekday and hour for a range of dates. Dates
// are station local calendar dates, held as midnight UTC.
type Slot struct {
	ID uint64
	// Weekday is ISO (1 = Monday ... 7 = Sunday); Weekday and Hour are
	// station local.
	Weekday   int
	Hour      int
	Clock     ClockRef
	ValidFrom time.Time
	// ValidTo is nil for an open end.
	ValidTo *time.Time
}

// covers reports whether the slot applies on date.
func (s Slot) covers(date time.Time) bool {
	return !date.Before(s.ValidFrom) && (s.ValidTo == nil || !date.After(*s.ValidTo))
}

// NewSlot is a slot to insert.
type NewSlot struct {
	Weekday   int
	Hour      int
	ClockID   uint64
	ValidFrom time.Time
	ValidTo   *time.Time
}

// Filter selects slots. Zero fields do not filter.
type Filter struct {
	ClockID uint64
	// From and To select slots valid on some date between them.
	From *time.Time
	To   *time.Time
}

// AssignInput assigns a clock to every combination of weekdays and hours
// for a range of dates. ClockID is 0 to clear the hours.
type AssignInput struct {
	ClockID   uint64
	Weekdays  []int
	Hours     []int
	ValidFrom time.Time
	ValidTo   *time.Time
}

// Change says what a save did to an overlapping slot.
type Change string

// Changes.
const (
	// Trimmed: the slot keeps the dates on one side of the new range.
	Trimmed Change = "trimmed"
	// Split: the slot keeps the dates on both sides; the later part is a
	// new slot.
	Split Change = "split"
	// Deleted: the new range covers the whole slot.
	Deleted Change = "deleted"
)

// Replaced is an existing slot changed by a save.
type Replaced struct {
	// Slot is the slot as it was.
	Slot   Slot
	Change Change
	// Remaining are the slots left of it (none when deleted).
	Remaining []Slot
}

// AssignResult is the outcome of Assign.
type AssignResult struct {
	Created  []Slot
	Replaced []Replaced
}

// ScheduledHour is one hour of the grid with its clock.
type ScheduledHour struct {
	// Date is the station local date.
	Date     time.Time
	Weekday  int
	Hour     int
	StartUTC time.Time
	SlotID   uint64
	Clock    ClockRef
}

// Store persists grid slots (internal/repository/grid).
type Store interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) Store
	// List returns slots in weekday, hour, valid_from order.
	List(ctx context.Context, f Filter) ([]Slot, error)
	// Get returns one slot, or ErrNotFound.
	Get(ctx context.Context, id uint64) (Slot, error)
	// LockOverlapping returns, locked until the transaction ends, the
	// slots of weekday and hour valid on some date from from to to (nil:
	// open end).
	LockOverlapping(ctx context.Context, weekday, hour int, from time.Time, to *time.Time) ([]Slot, error)
	// Insert adds a slot. ErrUnknownClock, ErrConflict.
	Insert(ctx context.Context, s NewSlot) (uint64, error)
	// SetRange changes the dates of a slot. ErrNotFound, ErrConflict.
	SetRange(ctx context.Context, id uint64, from time.Time, to *time.Time) error
	// Delete removes a slot; history keeps its clock and loses only the
	// slot (ON DELETE SET NULL). ErrNotFound.
	Delete(ctx context.Context, id uint64) error
}

// Settings are the settings the service reads.
type Settings struct {
	// Location is the station timezone.
	Location    *time.Location
	HorizonDays int
}

// SettingsFunc reads the settings for each request.
type SettingsFunc func(ctx context.Context) (Settings, error)

// TxRunner runs fn in one transaction (*db.DB).
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error
}

// dbTimeout bounds each statement or transaction of the service.
const dbTimeout = 5 * time.Second

// MaxScheduleDays bounds the dates of one Schedule request.
const MaxScheduleDays = 62

// Service manages the grid.
type Service struct {
	tx       TxRunner
	store    Store
	settings SettingsFunc
	now      func() time.Time
}

// NewService returns the service.
func NewService(tx TxRunner, store Store, settings SettingsFunc, now func() time.Time) *Service {
	return &Service{tx: tx, store: store, settings: settings, now: now}
}

// Date returns the calendar date of t in its location, as midnight UTC.
func Date(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// ISOWeekday returns 1 (Monday) to 7 (Sunday).
func ISOWeekday(t time.Time) int {
	return (int(t.Weekday())+6)%7 + 1
}

// ListSlots returns the slots that match f.
func (s *Service) ListSlots(ctx context.Context, f Filter) ([]Slot, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	slots, err := s.store.List(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("list grid slots: %w", err)
	}
	return slots, nil
}

// DeleteSlot deletes one slot.
func (s *Service) DeleteSlot(ctx context.Context, id uint64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return s.store.Delete(ctx, id)
}

// Assign assigns a clock to the hours, replacing the slots that overlap
// them for the overlapping dates (§4). With ClockID 0 it clears the hours.
func (s *Service) Assign(ctx context.Context, in AssignInput) (AssignResult, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return AssignResult{}, fmt.Errorf("read grid settings: %w", err)
	}
	in, err = s.validate(in, set)
	if err != nil {
		return AssignResult{}, err
	}

	out, err := s.assign(rctx, in)
	if errors.Is(err, ErrUnknownClock) {
		return AssignResult{}, validation.Field("clock_id", validation.UnknownID, "no clock has ID %d", in.ClockID)
	}
	if err != nil {
		return AssignResult{}, err
	}
	return out, nil
}

// assign runs an assignment in one transaction.
func (s *Service) assign(ctx context.Context, in AssignInput) (AssignResult, error) {
	var out AssignResult
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		out = AssignResult{}
		st := s.store.WithTx(tx)
		for _, wd := range in.Weekdays {
			for _, h := range in.Hours {
				if err := s.assignHour(ctx, st, in, wd, h, &out); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return out, err
}

// assignHour replaces the slots of one weekday and hour that overlap the
// new range, and inserts the new slot.
func (s *Service) assignHour(ctx context.Context, st Store, in AssignInput, wd, h int, out *AssignResult) error {
	olds, err := st.LockOverlapping(ctx, wd, h, in.ValidFrom, in.ValidTo)
	if err != nil {
		return err
	}
	for _, old := range olds {
		var r Replaced
		if r, err = replace(ctx, st, old, in.ValidFrom, in.ValidTo); err != nil {
			return err
		}
		out.Replaced = append(out.Replaced, r)
	}
	if in.ClockID == 0 {
		return nil
	}
	ns := NewSlot{Weekday: wd, Hour: h, ClockID: in.ClockID, ValidFrom: in.ValidFrom, ValidTo: in.ValidTo}
	id, err := st.Insert(ctx, ns)
	if err != nil {
		return err
	}
	created, err := st.Get(ctx, id)
	if err != nil {
		return err
	}
	out.Created = append(out.Created, created)
	return nil
}

// Remainder is what is left of an existing slot outside a new range.
type Remainder struct {
	// Before is set when the slot starts before the new range.
	Before *DateRange
	// After is set when the slot ends after the new range.
	After *DateRange
}

// DateRange is a range of dates, both ends inclusive; To is nil for an
// open end.
type DateRange struct {
	From time.Time
	To   *time.Time
}

// Remainders returns what is left of the slot range [from, to] outside the
// new range [nFrom, nTo]. The ranges overlap.
func Remainders(from time.Time, to *time.Time, nFrom time.Time, nTo *time.Time) Remainder {
	var r Remainder
	if from.Before(nFrom) {
		r.Before = &DateRange{From: from, To: new(nFrom.AddDate(0, 0, -1))}
	}
	if nTo != nil && (to == nil || to.After(*nTo)) {
		r.After = &DateRange{From: nTo.AddDate(0, 0, 1), To: to}
	}
	return r
}

// replace trims, splits, or deletes an existing slot so that it no longer
// overlaps the new range.
func replace(ctx context.Context, st Store, old Slot, nFrom time.Time, nTo *time.Time) (Replaced, error) {
	rem := Remainders(old.ValidFrom, old.ValidTo, nFrom, nTo)
	out := Replaced{Slot: old}
	switch {
	case rem.Before == nil && rem.After == nil:
		out.Change = Deleted
		return out, st.Delete(ctx, old.ID)
	case rem.Before != nil && rem.After != nil:
		out.Change = Split
		if err := st.SetRange(ctx, old.ID, rem.Before.From, rem.Before.To); err != nil {
			return Replaced{}, err
		}
		id, err := st.Insert(ctx, NewSlot{
			Weekday: old.Weekday, Hour: old.Hour, ClockID: old.Clock.ID,
			ValidFrom: rem.After.From, ValidTo: rem.After.To,
		})
		if err != nil {
			return Replaced{}, err
		}
		before, after := old, old
		before.ValidFrom, before.ValidTo = rem.Before.From, rem.Before.To
		after.ID, after.ValidFrom, after.ValidTo = id, rem.After.From, rem.After.To
		out.Remaining = []Slot{before, after}
	default:
		out.Change = Trimmed
		keep := rem.Before
		if keep == nil {
			keep = rem.After
		}
		if err := st.SetRange(ctx, old.ID, keep.From, keep.To); err != nil {
			return Replaced{}, err
		}
		left := old
		left.ValidFrom, left.ValidTo = keep.From, keep.To
		out.Remaining = []Slot{left}
	}
	return out, nil
}

// validate checks an assignment and returns it with sorted weekdays and
// hours. The past is never rewritten: valid_from lies between today and
// today + the grid horizon, station local.
func (s *Service) validate(in AssignInput, set Settings) (AssignInput, error) {
	errs := &validation.Error{}
	in.Weekdays = sortedSet(errs, "weekdays", in.Weekdays, 1, 7, "weekdays are 1 (Monday) to 7 (Sunday)")
	in.Hours = sortedSet(errs, "hours", in.Hours, 0, 23, "hours are 0 to 23")

	today := Date(s.now().In(set.Location))
	last := today.AddDate(0, 0, set.HorizonDays)
	switch {
	case in.ValidFrom.Before(today):
		errs.Add("valid_from", validation.Range, "must not be before today (%s, station time)", today.Format(time.DateOnly))
	case in.ValidFrom.After(last):
		errs.Add("valid_from", validation.Range, "the grid is planned at most %d days ahead (until %s)", set.HorizonDays, last.Format(time.DateOnly))
	}
	if in.ValidTo != nil && in.ValidTo.Before(in.ValidFrom) {
		errs.Add("valid_to", validation.Range, "must not be before valid_from")
	}
	// A slot matches a date by its ISO weekday, so a range with no date on a
	// chosen weekday would be stored and never play. Refused here, not
	// silently stored. Skipped when the range or the weekdays are already
	// invalid, to avoid a second error for the same cause.
	if in.ValidTo != nil && !in.ValidTo.Before(in.ValidFrom) && len(in.Weekdays) > 0 &&
		!weekdaysFall(in.Weekdays, in.ValidFrom, *in.ValidTo) {
		errs.Add("weekdays", validation.Invalid, "none of the chosen weekdays falls between valid_from and valid_to")
	}
	return in, errs.Err()
}

// weekdaysFall reports whether a date from first to last (both inclusive)
// falls on one of the ISO weekdays. Seven consecutive dates cover every
// weekday, so the scan stops after a week however long the range is.
func weekdaysFall(weekdays []int, first, last time.Time) bool {
	for i := range 7 {
		d := first.AddDate(0, 0, i)
		if d.After(last) {
			return false
		}
		if slices.Contains(weekdays, ISOWeekday(d)) {
			return true
		}
	}
	return false
}

func sortedSet(errs *validation.Error, field string, vs []int, lo, hi int, msg string) []int {
	out := slices.Clone(vs)
	slices.Sort(out)
	switch {
	case len(out) == 0:
		errs.Add(field, validation.Required, "list at least one")
	case out[0] < lo || out[len(out)-1] > hi:
		errs.Add(field, validation.Range, "%s", msg)
	case len(slices.Compact(slices.Clone(out))) != len(out):
		errs.Add(field, validation.Invalid, "a value is listed twice")
	}
	return out
}

// Schedule resolves the grid to hours for the local dates from ... to
// (inclusive), with the clock of each assigned hour.
func (s *Service) Schedule(ctx context.Context, from, to time.Time) ([]ScheduledHour, error) {
	switch {
	case to.Before(from):
		return nil, validation.Field("query.to", validation.Range, "must not be before from")
	case to.Sub(from) >= MaxScheduleDays*24*time.Hour:
		return nil, validation.Field("query.to", validation.Range, "at most %d days per request", MaxScheduleDays)
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read grid settings: %w", err)
	}
	slots, err := s.store.List(ctx, Filter{From: &from, To: &to})
	if err != nil {
		return nil, fmt.Errorf("list grid slots: %w", err)
	}
	return resolve(slots, from, to, set.Location), nil
}

// ClockHours returns the hours the grid assigns to a clock that start in
// [from, to). It implements clocks.GridHours for the dry check.
func (s *Service) ClockHours(ctx context.Context, clockID uint64, from, to time.Time) ([]clocks.Hour, error) {
	set, err := s.settings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read grid settings: %w", err)
	}
	fromDate, toDate := Date(from.In(set.Location)), Date(to.In(set.Location))
	slots, err := s.store.List(ctx, Filter{ClockID: clockID, From: &fromDate, To: &toDate})
	if err != nil {
		return nil, fmt.Errorf("list grid slots of clock %d: %w", clockID, err)
	}
	var out []clocks.Hour
	for _, h := range resolve(slots, fromDate, toDate, set.Location) {
		if h.StartUTC.Before(from) || !h.StartUTC.Before(to) {
			continue
		}
		out = append(out, clocks.Hour{StartUTC: h.StartUTC, Weekday: h.Weekday, Hour: h.Hour})
	}
	return out, nil
}

// Hours returns the assigned hours that start in [from, to), in time order.
// Rotation plans them (§7).
func (s *Service) Hours(ctx context.Context, from, to time.Time) ([]ScheduledHour, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read grid settings: %w", err)
	}
	fromDate, toDate := Date(from.In(set.Location)), Date(to.In(set.Location))
	slots, err := s.store.List(ctx, Filter{From: &fromDate, To: &toDate})
	if err != nil {
		return nil, fmt.Errorf("list grid slots: %w", err)
	}
	var out []ScheduledHour
	for _, h := range resolve(slots, fromDate, toDate, set.Location) {
		if !h.StartUTC.Before(from) && h.StartUTC.Before(to) {
			out = append(out, h)
		}
	}
	return out, nil
}

// resolve lists the assigned hours of the local dates from ... to in time
// order. A local hour that does not exist (a DST gap) is skipped; in a
// repeated hour the clock plays once, at its first occurrence (§10.2:
// Armenia has no DST).
func resolve(slots []Slot, from, to time.Time, loc *time.Location) []ScheduledHour {
	byHour := map[[2]int][]Slot{}
	for _, sl := range slots {
		k := [2]int{sl.Weekday, sl.Hour}
		byHour[k] = append(byHour[k], sl)
	}
	var out []ScheduledHour
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		wd := ISOWeekday(d)
		for h := range 24 {
			for _, sl := range byHour[[2]int{wd, h}] {
				if !sl.covers(d) {
					continue
				}
				start := time.Date(d.Year(), d.Month(), d.Day(), h, 0, 0, 0, loc)
				if start.Hour() != h {
					break // the hour does not exist on this date
				}
				out = append(out, ScheduledHour{
					Date: d, Weekday: wd, Hour: h, StartUTC: start.UTC(), SlotID: sl.ID, Clock: sl.Clock,
				})
				break // at most one slot covers a date (ex_grid_slots_weekday_hour_valid)
			}
		}
	}
	return out
}
