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
	ErrRequestNotFound = errors.New("filler request not found")
	// ErrRequestClosed: the reminder was answered, defaulted, or expired.
	ErrRequestClosed = errors.New("filler request is closed")
	// ErrNotAnswerable: a pool_empty reminder is closed by refilling the
	// pool, not by a track.
	ErrNotAnswerable = errors.New("filler request is closed by refilling the pool")
	// ErrHourStarted: the hour of the reminder is on air or over.
	ErrHourStarted = errors.New("the hour has started")
)

// Reasons of a filler request (ck_filler_requests_reason).
const (
	ReasonShortClock = "short_clock"
	ReasonPoolEmpty  = "pool_empty"
)

// RequestStatus is the state of a filler request.
type RequestStatus string

// Request states (ck_filler_requests_status).
const (
	RequestPending   RequestStatus = "pending"
	RequestAnswered  RequestStatus = "answered"
	RequestDefaulted RequestStatus = "defaulted"
	RequestExpired   RequestStatus = "expired"
)

// FillerRequest is a filler reminder of one clock hour (§7).
type FillerRequest struct {
	ID        uint64
	ClockID   uint64
	ClockName string
	HourStart time.Time
	Reason    string
	// Mode is the filler mode that asked: none, ask, or pool.
	Mode          clocks.FillerMode
	Status        RequestStatus
	CreatedAt     time.Time
	DeadlineAt    time.Time
	AnswerTrackID *uint64
}

// NewRequest is a filler request to store.
type NewRequest struct {
	ClockID    uint64
	HourStart  time.Time
	Reason     string
	Mode       clocks.FillerMode
	Status     RequestStatus
	CreatedAt  time.Time
	DeadlineAt time.Time
}

// PlanHour records a built hour (plan_hours).
type PlanHour struct {
	Start          time.Time
	ClockID        uint64
	ClockUpdatedAt time.Time
	GridSlotID     *uint64
	Seed           int64
	BuiltAt        time.Time
	// FillerRequestID is the reminder of this build.
	FillerRequestID *uint64
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
	ElementID  *uint64
	Start      time.Time
	OutMS      int
	Truncated  bool
	Source     string
	Status     string
}

// PlannedHour is a built hour with its items.
type PlannedHour struct {
	PlanHour
	ClockName string
	Items     []PlannedItem
}

// RequestFilter selects filler requests; an empty status selects all.
type RequestFilter struct {
	Status RequestStatus
	Limit  int
}

// Store persists the plan and the reminders (internal/repository/rotation).
type Store interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) Store
	// LockPlan takes the transaction-level advisory lock that serializes
	// writes of the plan.
	LockPlan(ctx context.Context) error
	// Library returns the tracks with a duration and every play rule.
	Library(ctx context.Context) (Library, error)
	// Separation returns the longest interval of each scope.
	Separation(ctx context.Context) (Separation, error)
	// History returns the plays in [from, to): aired and truncated in the
	// play log, planned and queued in the plan, except the planned items
	// of the hour that starts at hourStart.
	History(ctx context.Context, from, to, hourStart time.Time) ([]Play, error)
	// PlanHours returns the built hours that start in [from, to).
	PlanHours(ctx context.Context, from, to time.Time) ([]PlanHour, error)
	// PlanHour returns one built hour; false if there is none.
	PlanHour(ctx context.Context, start time.Time) (PlanHour, bool, error)
	// Pool returns the unused rows of a clock's filler pool in order.
	Pool(ctx context.Context, clockID uint64) ([]PoolRow, error)
	// ReplaceHour replaces the planned items of the hour (releasing the pool
	// rows they took) with items, marks their pool rows used, and stores h.
	ReplaceHour(ctx context.Context, h PlanHour, items []Item) error
	// ClearHour deletes the planned items of the hour and its record.
	ClearHour(ctx context.Context, start time.Time) error
	// ExpirePending expires the pending requests of the hour.
	ExpirePending(ctx context.Context, hourStart time.Time) error
	// CreateRequest stores a request; false when a pending request of the
	// clock hour exists already.
	CreateRequest(ctx context.Context, r NewRequest) (uint64, bool, error)
	// Request returns one request, or ErrRequestNotFound.
	Request(ctx context.Context, id uint64) (FillerRequest, error)
	// LockRequest returns one request locked until the transaction ends, or
	// ErrRequestNotFound.
	LockRequest(ctx context.Context, id uint64) (FillerRequest, error)
	// SetRequestStatus closes a request.
	SetRequestStatus(ctx context.Context, id uint64, status RequestStatus, answerTrackID *uint64) error
	// DueRequests returns pending requests whose deadline is at or before
	// at, at most limit, oldest deadline first.
	DueRequests(ctx context.Context, at time.Time, limit int) ([]FillerRequest, error)
	// Requests returns requests, newest hour first.
	Requests(ctx context.Context, f RequestFilter) ([]FillerRequest, error)
	// AnswerPoolEmpty answers the pending pool_empty requests of a clock.
	AnswerPoolEmpty(ctx context.Context, clockID uint64) (int64, error)
	// Plan returns the built hours that start in [from, to) with their
	// items.
	Plan(ctx context.Context, from, to time.Time) ([]PlannedHour, error)
}

// ClockSource reads a clock with its layout (*clocks.Service).
type ClockSource interface {
	Get(ctx context.Context, id uint64) (clocks.View, error)
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
	// ReminderDeadline is how long before its hour a reminder closes.
	ReminderDeadline time.Duration
	// Fallback is the global fallback filler mode (filler.fallback).
	Fallback clocks.Filler
	Build    Settings
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
	// dueBatch bounds the reminders resolved by one job run; the next run
	// takes the rest.
	dueBatch = 200
	// MaxPlanDays bounds one plan request.
	MaxPlanDays = 7
	// requestListLimit bounds the reminders listed.
	requestListLimit = 500
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
	fillLog  *slog.Logger // component filler
	now      func() time.Time
	// queue holds the clock_build operations waiting for the planner
	// goroutine (Run); see planner.go.
	queue chan rebuild
}

// NewService returns the service. log is the root logger.
func NewService(tx TxRunner, store Store, clockSrc ClockSource, gridSrc GridSource, settings SettingsFunc,
	ops Operations, pub Publisher, log *slog.Logger, now func() time.Time,
) *Service {
	return &Service{
		tx: tx, store: store, clocks: clockSrc, grid: gridSrc, settings: settings, ops: ops, pub: pub,
		log: logging.For(log, logging.ComponentRotation), fillLog: logging.For(log, logging.ComponentFiller),
		now: now, queue: make(chan rebuild, rebuildQueue),
	}
}

// Range is a span of time, [From, To).
type Range struct {
	From, To time.Time
}

func (r Range) contains(t time.Time) bool { return !t.Before(r.From) && t.Before(r.To) }

// SyncResult counts what a sync did.
type SyncResult struct {
	Built, Cleared, Warnings, Reminders int
}

func (r SyncResult) String() string {
	return fmt.Sprintf("built %d hours, cleared %d, %d warnings, %d new reminders", r.Built, r.Cleared, r.Warnings, r.Reminders)
}

// Sync brings the plan in step with the grid for the hours that start after
// now and within the plan horizon: it builds missing and stale hours and
// those in force, and clears built hours the grid no longer assigns. An hour
// that has started is never rebuilt. A failed hour does not stop the
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

	views := map[uint64]*clocks.View{}
	assigned := map[int64]bool{}
	var errs []error
	for _, h := range hours {
		assigned[h.StartUTC.Unix()] = true
		view, err := s.view(ctx, views, h.Clock.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if view == nil {
			continue // deleted since the grid was read
		}
		forced := slices.ContainsFunc(force, func(r Range) bool { return r.contains(h.StartUTC) })
		if ph, ok := planned[h.StartUTC.Unix()]; ok && !forced && ph.current(view.Clock) {
			continue
		}
		out, err := s.build(ctx, hourJob{start: h.StartUTC, slotID: new(h.SlotID), view: *view, force: forced}, &lib, set)
		if err != nil {
			errs = append(errs, fmt.Errorf("build hour %s: %w", h.StartUTC.Format(time.RFC3339), err))
			continue
		}
		if !out.skipped {
			res.Built++
			res.Warnings += len(out.plan.Warnings)
			res.Reminders += len(out.created)
		}
	}
	for _, h := range built {
		if assigned[h.Start.Unix()] {
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

// view returns the clock with its layout, cached per sync; nil if the clock
// no longer exists.
func (s *Service) view(ctx context.Context, cache map[uint64]*clocks.View, id uint64) (*clocks.View, error) {
	if v, ok := cache[id]; ok {
		return v, nil
	}
	v, err := s.clocks.Get(ctx, id)
	switch {
	case errors.Is(err, clocks.ErrNotFound):
		cache[id] = nil
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read clock %d: %w", id, err)
	}
	cache[id] = &v
	return &v, nil
}

// buildKind says why an hour is built.
type buildKind int

const (
	// buildFresh: the hour is missing, stale, or forced. Its pending
	// reminders expire and new ones are created.
	buildFresh buildKind = iota
	// buildDefault: the reminder of the build reached its deadline; the
	// waiting segments get the default mode. Same seed.
	buildDefault
	// buildAnswer: the operator answered the reminder. Same seed.
	buildAnswer
)

// hourJob is one hour to build.
type hourJob struct {
	start  time.Time
	slotID *uint64
	view   clocks.View
	kind   buildKind
	// force rebuilds a fresh hour even when it is current.
	force bool
	// request is the reminder of a default or answer build.
	request       FillerRequest
	answerTrackID uint64
}

// hourOutcome is what a build wrote.
type hourOutcome struct {
	skipped bool
	plan    HourPlan
	created []FillerRequest
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
	s.report(j, out)
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
	c := j.view.Clock
	switch {
	case j.kind == buildFresh && !j.force && found && ph.current(c):
		return hourOutcome{skipped: true}, nil
	case j.kind != buildFresh && (!found || !ph.current(c) || ph.FillerRequestID == nil || *ph.FillerRequestID != j.request.ID):
		// The hour was rebuilt since the reminder was made; the next sync
		// handles it.
		return hourOutcome{skipped: true}, nil
	}

	sep, err := st.Separation(ctx)
	if err != nil {
		return hourOutcome{}, err
	}
	span := max(sep.Track, sep.Artist, lib.maxRuleInterval())
	history, err := st.History(ctx, j.start.Add(-span), j.start.Add(time.Hour+span), j.start)
	if err != nil {
		return hourOutcome{}, err
	}
	pool, err := st.Pool(ctx, c.ID)
	if err != nil {
		return hourOutcome{}, err
	}

	now := s.now().UTC()
	deadline := j.start.Add(-set.ReminderDeadline)
	in := HourInput{
		Start: j.start, Location: set.Location, Clock: c, Layout: j.view.Layout,
		DefaultFiller: set.Fallback, Library: lib, History: history, Separation: sep, Pool: pool,
		Settings: set.Build, Seed: uint64(j.start.Unix()), //nolint:gosec // G115: hour starts are after 1970
	}
	if c.DefaultFiller != nil {
		in.DefaultFiller = *c.DefaultFiller
	}
	next := PlanHour{
		Start: j.start, ClockID: c.ID, ClockUpdatedAt: c.UpdatedAt, GridSlotID: j.slotID, BuiltAt: now,
	}
	switch j.kind {
	case buildFresh:
		if err := st.ExpirePending(ctx, j.start); err != nil {
			return hourOutcome{}, err
		}
		if now.Before(deadline) {
			in.Waiting = WaitAsk
		} else {
			in.Waiting = WaitDefault
		}
	case buildDefault, buildAnswer:
		in.Seed = uint64(ph.Seed) //nolint:gosec // G115: seeds are stored from uint64 hour starts
		in.Waiting = WaitDefault
		if j.kind == buildAnswer {
			in.Waiting, in.AnswerTrackID = WaitAnswer, j.answerTrackID
		}
		next.GridSlotID, next.FillerRequestID = ph.GridSlotID, ph.FillerRequestID
	}
	next.Seed = int64(in.Seed) //nolint:gosec // G115: the seed is an hour start in seconds

	plan := Build(in)
	if j.kind == buildAnswer && plan.AnswerRefused {
		return hourOutcome{}, validation.Field("track_id", validation.Blocked,
			"the track breaks separation or a play rule at that time, or has no duration")
	}

	out := hourOutcome{plan: plan}
	if j.kind == buildFresh {
		if len(plan.Asked) > 0 {
			r := NewRequest{
				ClockID: c.ID, HourStart: j.start, Reason: ReasonShortClock, Mode: plan.AskMode,
				Status: RequestPending, CreatedAt: now, DeadlineAt: deadline,
			}
			if in.Waiting == WaitDefault {
				r.Status = RequestDefaulted // recorded; the default filled the hour at once
			}
			id, ok, err := st.CreateRequest(ctx, r)
			if err != nil {
				return hourOutcome{}, err
			}
			if ok {
				next.FillerRequestID = &id
				if r.Status == RequestPending {
					out.created = append(out.created, requestOf(id, r, c.Name))
				}
			}
		}
		if plan.PoolEmpty && now.Before(deadline) {
			r := NewRequest{
				ClockID: c.ID, HourStart: j.start, Reason: ReasonPoolEmpty, Mode: clocks.FillerPool,
				Status: RequestPending, CreatedAt: now, DeadlineAt: deadline,
			}
			id, ok, err := st.CreateRequest(ctx, r)
			if err != nil {
				return hourOutcome{}, err
			}
			if ok {
				out.created = append(out.created, requestOf(id, r, c.Name))
			}
		}
	}
	if err := st.ReplaceHour(ctx, next, plan.Items); err != nil {
		return hourOutcome{}, err
	}
	return out, nil
}

func requestOf(id uint64, r NewRequest, clockName string) FillerRequest {
	return FillerRequest{
		ID: id, ClockID: r.ClockID, ClockName: clockName, HourStart: r.HourStart, Reason: r.Reason, Mode: r.Mode,
		Status: r.Status, CreatedAt: r.CreatedAt, DeadlineAt: r.DeadlineAt,
	}
}

// report logs the warnings of a build and sends its events.
func (s *Service) report(j hourJob, out hourOutcome) {
	if out.skipped {
		return
	}
	for _, r := range out.created {
		s.pub.Publish(ws.FillerRequestCreatedEvent{
			Type: ws.EventFillerRequestCreated, Time: r.CreatedAt,
			Data: ws.FillerRequestCreatedData{
				ID: r.ID, ClockID: r.ClockID, ClockName: r.ClockName, HourStartUTC: r.HourStart,
				Reason: r.Reason, DeadlineAt: r.DeadlineAt,
			},
		})
	}
	if len(out.plan.Warnings) == 0 {
		return
	}
	data := ws.TrackPlanWarningData{
		HourStartUTC: j.start, ClockID: j.view.Clock.ID, ClockName: j.view.Clock.Name,
		Warnings: make([]ws.TrackPlanWarning, len(out.plan.Warnings)),
	}
	for i, w := range out.plan.Warnings {
		log := s.log
		if w.filler() {
			log = s.fillLog
		}
		log.Warn("track plan warning", "code", string(w.Code), "hour_start_utc", j.start,
			"clock_id", j.view.Clock.ID, "message", w.Message)
		data.Warnings[i] = ws.TrackPlanWarning{
			Code: string(w.Code), Message: w.Message, ElementIndex: w.Element, SegmentIndex: w.Segment, TrackID: w.TrackID,
		}
	}
	s.pub.Publish(ws.TrackPlanWarningEvent{Type: ws.EventTrackPlanWarning, Time: s.now().UTC(), Data: data})
}

// clear removes a built hour that the grid no longer assigns.
func (s *Service) clear(ctx context.Context, start time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, hourTimeout)
	defer cancel()
	return s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.LockPlan(ctx); err != nil {
			return err
		}
		if err := st.ExpirePending(ctx, start); err != nil {
			return err
		}
		return st.ClearHour(ctx, start)
	})
}

// RunJob is the plan job (§7): reminders whose deadline passed get the
// default mode, then the plan is synced.
func (s *Service) RunJob(ctx context.Context) (string, error) {
	n, dueErr := s.resolveDue(ctx)
	res, err := s.Sync(ctx, nil)
	summary := fmt.Sprintf("%s; %d reminders defaulted", res, n)
	return summary, errors.Join(dueErr, err)
}

// resolveDue defaults the pending reminders whose deadline passed and fills
// their hours with the default mode.
func (s *Service) resolveDue(ctx context.Context) (int, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	now := s.now().UTC()
	due, err := s.store.DueRequests(rctx, now, dueBatch)
	if err != nil {
		return 0, fmt.Errorf("read due reminders: %w", err)
	}
	if len(due) == 0 {
		return 0, nil
	}
	set, err := s.settings(rctx)
	if err != nil {
		return 0, fmt.Errorf("read rotation settings: %w", err)
	}
	lib, err := s.store.Library(rctx)
	if err != nil {
		return 0, fmt.Errorf("read library: %w", err)
	}
	views := map[uint64]*clocks.View{}
	var errs []error
	n := 0
	for _, r := range due {
		view, err := s.view(ctx, views, r.ClockID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.closeDue(ctx, r, view, &lib, set); err != nil {
			errs = append(errs, fmt.Errorf("default reminder %d: %w", r.ID, err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// closeDue defaults one reminder and, for a future hour still built from it,
// rebuilds the hour with the default mode.
func (s *Service) closeDue(ctx context.Context, r FillerRequest, view *clocks.View, lib *Library, set PlanSettings) error {
	ctx, cancel := context.WithTimeout(ctx, hourTimeout)
	defer cancel()
	var out hourOutcome
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.LockPlan(ctx); err != nil {
			return err
		}
		req, err := st.LockRequest(ctx, r.ID)
		if err != nil || req.Status != RequestPending {
			return err
		}
		if err = st.SetRequestStatus(ctx, req.ID, RequestDefaulted, nil); err != nil {
			return err
		}
		if req.Reason != ReasonShortClock || view == nil || !req.HourStart.After(s.now()) {
			return nil
		}
		out, err = s.buildTx(ctx, st, hourJob{start: req.HourStart, view: *view, kind: buildDefault, request: req}, lib, set)
		return err
	})
	if err != nil {
		return err
	}
	s.fillLog.Info("filler reminder defaulted", "request_id", r.ID, "clock_id", r.ClockID, "hour_start_utc", r.HourStart)
	s.report(hourJob{start: r.HourStart, view: derefView(view)}, out)
	return nil
}

func derefView(v *clocks.View) clocks.View {
	if v == nil {
		return clocks.View{}
	}
	return *v
}

// Answer answers a pending reminder with a track: the track fills the
// first waiting segment of the hour, the default mode the others. A track
// that breaks separation or a play rule at that time is refused.
func (s *Service) Answer(ctx context.Context, id, trackID uint64) (FillerRequest, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	r, err := s.store.Request(rctx, id)
	if err != nil {
		return FillerRequest{}, err
	}
	set, err := s.settings(rctx)
	if err != nil {
		return FillerRequest{}, fmt.Errorf("read rotation settings: %w", err)
	}
	lib, err := s.store.Library(rctx)
	if err != nil {
		return FillerRequest{}, fmt.Errorf("read library: %w", err)
	}
	if _, ok := lib.track(trackID); !ok {
		return FillerRequest{}, validation.Field("track_id", validation.UnknownID, "no track with a duration has ID %d", trackID)
	}
	view, err := s.view(ctx, map[uint64]*clocks.View{}, r.ClockID)
	if err != nil {
		return FillerRequest{}, err
	}

	wctx, cancel := context.WithTimeout(ctx, hourTimeout)
	defer cancel()
	var out hourOutcome
	err = s.tx.WithTx(wctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if lockErr := st.LockPlan(ctx); lockErr != nil {
			return lockErr
		}
		req, txErr := st.LockRequest(ctx, id)
		switch {
		case txErr != nil:
			return txErr
		case req.Status != RequestPending:
			return ErrRequestClosed
		case req.Reason != ReasonShortClock:
			return ErrNotAnswerable
		case !req.HourStart.After(s.now()):
			return ErrHourStarted
		}
		if txErr = st.SetRequestStatus(ctx, id, RequestAnswered, &trackID); txErr != nil {
			return txErr
		}
		if view == nil {
			return nil
		}
		out, txErr = s.buildTx(ctx, st, hourJob{
			start: req.HourStart, view: *view, kind: buildAnswer, request: req, answerTrackID: trackID,
		}, &lib, set)
		return txErr
	})
	if err != nil {
		return FillerRequest{}, err
	}
	s.report(hourJob{start: r.HourStart, view: derefView(view)}, out)
	return s.store.Request(rctx, id)
}

// PoolRefilled answers the pending pool_empty reminders of a clock after
// its pool was refilled.
func (s *Service) PoolRefilled(ctx context.Context, clockID uint64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if _, err := s.store.AnswerPoolEmpty(ctx, clockID); err != nil {
		return fmt.Errorf("answer pool reminders of clock %d: %w", clockID, err)
	}
	return nil
}

// Requests lists the reminders with the status; empty: all.
func (s *Service) Requests(ctx context.Context, status RequestStatus) ([]FillerRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rs, err := s.store.Requests(ctx, RequestFilter{Status: status, Limit: requestListLimit})
	if err != nil {
		return nil, fmt.Errorf("list filler requests: %w", err)
	}
	return rs, nil
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

// Plan returns the built hours that start in the range, with their items.
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
