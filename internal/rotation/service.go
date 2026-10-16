package rotation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// Errors returned by the service.
var (
	// ErrHourStarted: the hour is on air, over, or frozen for playout
	// (PlanSettings.Freeze).
	ErrHourStarted = errors.New("the hour has started")
	// ErrInsertNotFound: no one-time insert has the ID.
	ErrInsertNotFound = errors.New("hour insert not found")
)

// InsertError is the failure of an hour_insert or reschedule operation
// whose insert cannot be placed whole in its hour. Nothing was changed.
type InsertError struct {
	Reason  RefusalReason
	Message string
}

func (e *InsertError) Error() string { return "the insert cannot be placed in that hour: " + e.Message }

// FailureReason is the stable reason sent with the operation's failure
// event.
func (e *InsertError) FailureReason() string { return string(e.Reason) }

// PlanHour records a built hour (plan_hours).
type PlanHour struct {
	Start          time.Time
	ClockID        uint64
	ClockUpdatedAt time.Time
	GridSlotID     *uint64
	Seed           int64
	BuiltAt        time.Time
}

// current reports whether the hour was built from this version of c.
func (h PlanHour) current(c clocks.Clock) bool {
	return h.ClockID == c.ID && h.ClockUpdatedAt.Equal(c.UpdatedAt)
}

// PlannedItem is an item of the track plan, for the panel.
type PlannedItem struct {
	ID         uint64
	TrackID    uint64
	Title      string
	ArtistName *string
	DurationMS int
	Start      time.Time
	// InMS and OutMS are the in and out points from the start of the file.
	InMS      int
	OutMS     int
	Truncated bool
	Source    string
	Status    string
	// InsertID is the template insert of an insert item; HourInsertID the
	// one-time insert of an hour_insert or reschedule item.
	InsertID     *uint64
	HourInsertID *uint64
}

// PlannedHour is a built hour with its items and its one-time inserts.
type PlannedHour struct {
	PlanHour
	ClockName string
	Items     []PlannedItem
	Inserts   []Insert
}

// Store persists the plan and the one-time inserts
// (internal/repository/rotation).
type Store interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) Store
	// LockPlan takes the transaction-level advisory lock that serializes
	// writes of the plan.
	LockPlan(ctx context.Context) error
	// Library returns the tracks with a duration and every play rule.
	Library(ctx context.Context) (Library, error)
	// History returns the plays in [from, to): aired and truncated in the
	// play log, planned and queued in the plan, except the planned items
	// of the hour that starts at hourStart.
	History(ctx context.Context, from, to, hourStart time.Time) ([]Play, error)
	// PlanHours returns the built hours that start in [from, to).
	PlanHours(ctx context.Context, from, to time.Time) ([]PlanHour, error)
	// PlanHour returns one built hour; false if there is none.
	PlanHour(ctx context.Context, start time.Time) (PlanHour, bool, error)
	// ReplaceHour replaces the planned items of the hour with items and
	// stores h.
	ReplaceHour(ctx context.Context, h PlanHour, items []Item) error
	// ClearHour deletes the planned items of the hour and its record.
	ClearHour(ctx context.Context, start time.Time) error
	// Plan returns the built hours that start in [from, to) with their
	// items and one-time inserts.
	Plan(ctx context.Context, from, to time.Time) ([]PlannedHour, error)

	// Missed returns a missed item; outage.ErrMissedNotFound.
	Missed(ctx context.Context, id uint64) (MissedItem, error)
	// LockMissed returns a missed item locked for update;
	// outage.ErrMissedNotFound.
	LockMissed(ctx context.Context, id uint64) (MissedItem, error)
	// Inserts returns the one-time inserts of the hour, in ID order.
	Inserts(ctx context.Context, hourStart time.Time) ([]Insert, error)
	// Insert returns one one-time insert; ErrInsertNotFound.
	Insert(ctx context.Context, id uint64) (Insert, error)
	// CreateInsert stores a one-time insert and returns its ID.
	CreateInsert(ctx context.Context, ins Insert) (uint64, error)
	// DeleteInsert deletes a one-time insert.
	DeleteInsert(ctx context.Context, id uint64) error
	// ReopenMissed sets a rescheduled missed item open again.
	ReopenMissed(ctx context.Context, missedID uint64) error
	// LinkRescheduled resolves a missed item as rescheduled to the plan
	// row that airs it again (keeping the first resolved_at).
	LinkRescheduled(ctx context.Context, missedID uint64, at time.Time) error
}

// ClockSource reads a clock (*clocks.Service).
type ClockSource interface {
	Get(ctx context.Context, id uint64) (clocks.Clock, error)
}

// GridSource resolves the grid to hours (*grid.Service).
type GridSource interface {
	Hours(ctx context.Context, from, to time.Time) ([]grid.ScheduledHour, error)
}

// Operations starts and finishes operations (*operations.Tracker).
type Operations interface {
	Start(ctx context.Context, kind operations.Kind) (uint64, error)
	Execute(ctx context.Context, id uint64, component logging.Component, fn operations.Func) error
}

// Publisher sends an event to the panels without blocking.
type Publisher interface {
	Publish(event any)
}

// PlanSettings are the settings the service reads before each sync.
type PlanSettings struct {
	Location         *time.Location
	PlanHorizonHours int
	// Freeze is how long before its start an hour is frozen: playout may
	// have pushed its first items to Liquidsoap (playout.prepush_minutes),
	// so a built hour is no longer rebuilt or cleared, and its one-time
	// inserts are not changed.
	Freeze time.Duration
	// DefaultFixedToleranceMS is the start tolerance of a one-time insert
	// when the request gives none (rotation.default_fixed_tolerance_ms).
	DefaultFixedToleranceMS int
	Build                   Settings
}

// frozen reports whether the hour that starts at start is on air, over, or
// so close to its start that playout may have pushed its items.
func (p PlanSettings) frozen(start, now time.Time) bool {
	return !start.After(now.Add(p.Freeze))
}

// SettingsFunc reads the settings.
type SettingsFunc func(ctx context.Context) (PlanSettings, error)

// TxRunner runs fn in one transaction (*db.DB).
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error
}

const (
	// dbTimeout bounds each read of the service outside a build.
	dbTimeout = 10 * time.Second
	// hourTimeout bounds the transaction of one hour: reads, the build (CPU
	// only), and the writes.
	hourTimeout = 15 * time.Second
	// MaxPlanDays bounds one plan request.
	MaxPlanDays = 7
)

// Service keeps the track plan.
type Service struct {
	tx       TxRunner
	store    Store
	clocks   ClockSource
	grid     GridSource
	settings SettingsFunc
	ops      Operations
	pub      Publisher
	log      *slog.Logger // component rotation
	now      func() time.Time
	// queue holds the operations waiting for the planner goroutine (Run);
	// see planner.go.
	queue chan rebuild
}

// NewService returns the service. log is the root logger.
func NewService(tx TxRunner, store Store, clockSrc ClockSource, gridSrc GridSource, settings SettingsFunc,
	ops Operations, pub Publisher, log *slog.Logger, now func() time.Time,
) *Service {
	return &Service{
		tx: tx, store: store, clocks: clockSrc, grid: gridSrc, settings: settings, ops: ops, pub: pub,
		log: logging.For(log, logging.ComponentRotation), now: now, queue: make(chan rebuild, rebuildQueue),
	}
}

// Range is a span of time, [From, To).
type Range struct {
	From, To time.Time
}

func (r Range) contains(t time.Time) bool { return !t.Before(r.From) && t.Before(r.To) }

// SyncResult counts what a sync did.
type SyncResult struct {
	Built, Cleared, Warnings int
}

func (r SyncResult) String() string {
	return fmt.Sprintf("built %d hours, cleared %d, %d warnings", r.Built, r.Cleared, r.Warnings)
}

// Sync brings the plan in step with the grid for the hours that start after
// now and within the plan horizon: it builds missing and stale hours and
// those in force, and clears built hours the grid no longer assigns. A
// built hour that is frozen (PlanSettings.Freeze) is never rebuilt or
// cleared; a missing one is still built. A failed hour does not stop the
// others; the errors are returned together.
func (s *Service) Sync(ctx context.Context, force []Range) (SyncResult, error) {
	var res SyncResult
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return res, fmt.Errorf("read rotation settings: %w", err)
	}
	now := s.now().UTC()
	after := now.Add(time.Nanosecond)
	horizon := now.Add(time.Duration(set.PlanHorizonHours) * time.Hour)
	hours, err := s.grid.Hours(rctx, after, horizon)
	if err != nil {
		return res, fmt.Errorf("read grid hours: %w", err)
	}
	built, err := s.store.PlanHours(rctx, after, horizon)
	if err != nil {
		return res, fmt.Errorf("read plan hours: %w", err)
	}
	lib, err := s.store.Library(rctx)
	if err != nil {
		return res, fmt.Errorf("read library: %w", err)
	}
	// Keyed by Unix seconds: hour starts are whole seconds, and time.Time
	// keys would also compare locations.
	planned := map[int64]PlanHour{}
	for _, h := range built {
		planned[h.Start.Unix()] = h
	}

	cache := map[uint64]*clocks.Clock{}
	assigned := map[int64]bool{}
	var errs []error
	for _, h := range hours {
		assigned[h.StartUTC.Unix()] = true
		c, err := s.clock(ctx, cache, h.Clock.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if c == nil {
			continue // deleted since the grid was read
		}
		forced := slices.ContainsFunc(force, func(r Range) bool { return r.contains(h.StartUTC) })
		if ph, ok := planned[h.StartUTC.Unix()]; ok && !forced && ph.current(*c) {
			continue
		}
		out, err := s.build(ctx, hourJob{start: h.StartUTC, slotID: new(h.SlotID), clock: *c, force: forced}, &lib, set)
		if err != nil {
			errs = append(errs, fmt.Errorf("build hour %s: %w", h.StartUTC.Format(time.RFC3339), err))
			continue
		}
		if !out.skipped {
			res.Built++
			res.Warnings += len(out.plan.Warnings)
		}
	}
	for _, h := range built {
		if assigned[h.Start.Unix()] || set.frozen(h.Start, now) {
			continue
		}
		if err := s.clear(ctx, h.Start); err != nil {
			errs = append(errs, fmt.Errorf("clear hour %s: %w", h.Start.Format(time.RFC3339), err))
			continue
		}
		res.Cleared++
	}
	return res, errors.Join(errs...)
}

// clock returns the clock, cached per sync; nil if it no longer exists.
func (s *Service) clock(ctx context.Context, cache map[uint64]*clocks.Clock, id uint64) (*clocks.Clock, error) {
	if c, ok := cache[id]; ok {
		return c, nil
	}
	c, err := s.clocks.Get(ctx, id)
	switch {
	case errors.Is(err, clocks.ErrNotFound):
		cache[id] = nil
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read clock %d: %w", id, err)
	}
	cache[id] = &c
	return &c, nil
}

// hourJob is one hour to build.
type hourJob struct {
	start  time.Time
	slotID *uint64
	clock  clocks.Clock
	// force rebuilds the hour even when it is current.
	force bool
	// add stores this one-time insert before the build and fails the build
	// when it cannot be placed whole.
	add *Insert
	// remove deletes this one-time insert before the build.
	remove *Insert
}

// hourOutcome is what a build wrote.
type hourOutcome struct {
	skipped bool
	plan    HourPlan
	// placed is the item of the insert of add.
	placed *Item
}

// build builds one hour in its own transaction and reports it.
func (s *Service) build(ctx context.Context, j hourJob, lib *Library, set PlanSettings) (hourOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, hourTimeout)
	defer cancel()
	var out hourOutcome
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) (err error) {
		out, err = s.buildTx(ctx, s.store.WithTx(tx), j, lib, set)
		return err
	})
	if err != nil {
		return hourOutcome{}, err
	}
	s.report(j.start, j.clock, out)
	return out, nil
}

// buildTx builds one hour inside a transaction, under the plan lock. It
// re-reads the hour after taking the lock, so concurrent syncs build an hour
// once.
func (s *Service) buildTx(ctx context.Context, st Store, j hourJob, lib *Library, set PlanSettings) (hourOutcome, error) {
	if err := st.LockPlan(ctx); err != nil {
		return hourOutcome{}, err
	}
	ph, found, err := st.PlanHour(ctx, j.start)
	if err != nil {
		return hourOutcome{}, err
	}
	now := s.now().UTC()
	c := j.clock
	frozen := set.frozen(j.start, now)
	switch {
	case (j.add != nil || j.remove != nil) && frozen:
		return hourOutcome{}, ErrHourStarted
	case found && frozen:
		// Its items may be in Liquidsoap's queue already.
		return hourOutcome{skipped: true}, nil
	case !j.force && found && ph.current(c):
		return hourOutcome{skipped: true}, nil
	}

	var addedID uint64
	if j.add != nil {
		if addedID, err = s.storeInsert(ctx, st, *j.add); err != nil {
			return hourOutcome{}, err
		}
	}
	if j.remove != nil {
		if err = dropInserts(ctx, st, []Insert{*j.remove}); err != nil {
			return hourOutcome{}, err
		}
	}
	inserts, err := st.Inserts(ctx, j.start)
	if err != nil {
		return hourOutcome{}, err
	}
	span := lib.maxRuleInterval()
	history, err := st.History(ctx, j.start.Add(-span), j.start.Add(time.Hour+span), j.start)
	if err != nil {
		return hourOutcome{}, err
	}
	in := HourInput{
		Start: j.start, Location: set.Location, Clock: c, HourInserts: inserts, Library: lib, History: history,
		Settings: set.Build, Seed: uint64(j.start.Unix()), //nolint:gosec // G115: hour starts are after 1970
	}
	plan := Build(in)
	out := hourOutcome{plan: plan}
	if addedID != 0 {
		if r, ok := plan.Refused[addedID]; ok {
			return hourOutcome{}, &InsertError{Reason: r.Reason, Message: r.Message}
		}
		i := slices.IndexFunc(plan.Items, func(it Item) bool { return it.HourInsertID != nil && *it.HourInsertID == addedID })
		if i >= 0 {
			out.placed = &plan.Items[i]
		}
	}
	if err := settleInserts(ctx, st, inserts, &out.plan); err != nil {
		return hourOutcome{}, err
	}
	next := PlanHour{
		Start: j.start, ClockID: c.ID, ClockUpdatedAt: c.UpdatedAt, GridSlotID: j.slotID, BuiltAt: now,
		Seed: int64(in.Seed), //nolint:gosec // G115: the seed is an hour start in seconds
	}
	if err := st.ReplaceHour(ctx, next, plan.Items); err != nil {
		return hourOutcome{}, err
	}
	if err := linkRescheduled(ctx, st, plan.Items, now); err != nil {
		return hourOutcome{}, err
	}
	return out, nil
}

// settleInserts drops the one-time inserts a build could not place, with a
// warning, and reopens their missed items, so the manager can reschedule
// them again.
func settleInserts(ctx context.Context, st Store, inserts []Insert, plan *HourPlan) error {
	var drop []Insert
	for _, ins := range inserts {
		r, ok := plan.Refused[ins.ID]
		if !ok {
			continue
		}
		drop = append(drop, ins)
		w := Warning{
			Code: WarnHourInsertDropped, TrackID: new(ins.TrackID), HourInsertID: new(ins.ID),
			Message: "the one-time insert cannot be placed and is removed: " + r.Message,
		}
		if ins.MissedID != nil {
			w.Code, w.Message = WarnRescheduleDropped, "the rescheduled item cannot be placed and is removed; its missed item is open again: "+r.Message
		}
		plan.Warnings = append(plan.Warnings, w)
	}
	return dropInserts(ctx, st, drop)
}

// report logs the warnings of a build and sends them to the panels.
func (s *Service) report(start time.Time, c clocks.Clock, out hourOutcome) {
	if out.skipped || len(out.plan.Warnings) == 0 {
		return
	}
	data := ws.TrackPlanWarningData{
		HourStartUTC: start, ClockID: c.ID, ClockName: c.Name,
		Warnings: make([]ws.TrackPlanWarning, len(out.plan.Warnings)),
	}
	for i, w := range out.plan.Warnings {
		s.log.Warn("track plan warning", "code", string(w.Code), "hour_start_utc", start, "clock_id", c.ID, "message", w.Message)
		data.Warnings[i] = ws.TrackPlanWarning{
			Code: string(w.Code), Message: w.Message, PlaylistIndex: w.PlaylistIndex, InsertIndex: w.InsertIndex,
			TrackID: w.TrackID, HourInsertID: w.HourInsertID,
		}
	}
	s.pub.Publish(ws.TrackPlanWarningEvent{Type: ws.EventTrackPlanWarning, Time: s.now().UTC(), Data: data})
}

// clear removes a built hour that the grid no longer assigns. Its one-time
// inserts no longer play: they are removed and their missed items open
// again.
func (s *Service) clear(ctx context.Context, start time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, hourTimeout)
	defer cancel()
	return s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.LockPlan(ctx); err != nil {
			return err
		}
		inserts, err := st.Inserts(ctx, start)
		if err != nil {
			return err
		}
		if err := dropInserts(ctx, st, inserts); err != nil {
			return err
		}
		if len(inserts) > 0 {
			s.log.Warn("the grid no longer plays an hour with one-time inserts; they are removed and their missed items are open again",
				"hour_start_utc", start, "count", len(inserts))
		}
		return st.ClearHour(ctx, start)
	})
}

// RunJob is the plan job (§7): the plan is synced to its horizon.
func (s *Service) RunJob(ctx context.Context) (string, error) {
	res, err := s.Sync(ctx, nil)
	return res.String(), err
}

// checkRange refuses a range that is empty or longer than MaxPlanDays.
func checkRange(r Range) error {
	switch {
	case !r.To.After(r.From):
		return validation.Field("to", validation.Range, "must be after from")
	case r.To.Sub(r.From) > MaxPlanDays*24*time.Hour:
		return validation.Field("to", validation.Range, "at most %d days per request", MaxPlanDays)
	}
	return nil
}

// Plan returns the built hours that start in the range, with their items
// and one-time inserts.
func (s *Service) Plan(ctx context.Context, r Range) ([]PlannedHour, error) {
	if err := checkRange(r); err != nil {
		return nil, validationQuery(err)
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	hs, err := s.store.Plan(ctx, r.From, r.To)
	if err != nil {
		return nil, fmt.Errorf("read track plan: %w", err)
	}
	return hs, nil
}

// validationQuery moves the field errors of a range to the query
// parameters.
func validationQuery(err error) error {
	var v *validation.Error
	if !errors.As(err, &v) {
		return err
	}
	out := &validation.Error{}
	for _, f := range v.Fields {
		if !strings.HasPrefix(f.Field, "query.") {
			f.Field = "query." + f.Field
		}
		out.Fields = append(out.Fields, f)
	}
	return out
}
