package rotation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync/atomic"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

// Errors returned by the service.
var (
	// ErrHourStarted: the hour is on air, over, or frozen for playout
	// (PlanSettings.Freeze).
	ErrHourStarted = errors.New("the hour has started")
	// ErrInsertNotFound: no one-time insert has the ID.
	ErrInsertNotFound = errors.New("hour insert not found")
	// ErrPlanItemNotFound: no planned item of the track plan has the ID.
	ErrPlanItemNotFound = errors.New("plan item not found")
	// ErrNotMovable: the item cannot be pinned or moved. Only a planned fill
	// track, a planned rule track with a known playlist, and a one-time insert
	// with a fixed time, can be.
	ErrNotMovable = errors.New("the item cannot be pinned or moved")
)

const (
	// dbTimeout bounds each read of the service outside a build.
	dbTimeout = 10 * time.Second
	// hourTimeout bounds the transaction of one hour: reads, the build (CPU
	// only), and the writes.
	hourTimeout = 15 * time.Second
	// MaxPlanDays bounds one request of a range rebuild, and the range the
	// rebuild checks (checkRange): at most this many days.
	MaxPlanDays = 31
	// startedHourLead is how far ahead of now the first build of a started
	// hour begins. The planner does not wake the pusher, which ticks every
	// 5 s and skips an item more than 30 s late: the lead lets the next tick
	// push the first item before its planned start.
	startedHourLead = 10 * time.Second
	// startedHourMinRemainder is the least time, after the lead, worth
	// building for: under it a track or two would only cut the emergency
	// source for moments before the next hour.
	startedHourMinRemainder = 2 * time.Minute
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
	// full is set while a full pass runs: only one runs at a time, whether
	// the nightly job or the planner starts it.
	full atomic.Bool
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

// sync brings the plan in step with the grid for the hours that start after
// now and within the plan horizon: it builds missing and stale hours and
// those in force, and clears built hours the grid no longer assigns. A
// built hour that is frozen (PlanSettings.Freeze) is never rebuilt or
// cleared; a missing one is still built. The grid hour that contains now is
// built too if it has no plan_hours row, once, for what is left of it
// (startedHour). A failed hour does not stop the others; the errors are
// returned together. A pass that builds at least one hour is logged (its
// start and its end, see passlog.go).
func (s *Service) sync(ctx context.Context, trigger Trigger, force []Range) (SyncResult, error) {
	res := SyncResult{ByCode: map[WarningCode]int{}}
	began := s.now()
	count := &queryCount{}
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return res, fmt.Errorf("read rotation settings: %w", err)
	}
	now := s.now().UTC()
	after := now.Add(time.Nanosecond)
	horizon := planHorizon(now, set)
	st := countingStore{Store: s.store, n: count}
	count.add(1) // settings

	cache := map[uint64]*clocks.Clock{}
	var todo []hourJob
	var errs []error
	// Read on its own, so the window below stays (now, horizon]: stale hours
	// that have started must not join the work list.
	count.add(1)
	started, err := s.startedHour(rctx, st, cache, now)
	switch {
	case err != nil:
		errs = append(errs, err)
	case started != nil:
		started.count = count
		todo = append(todo, *started)
	}

	hours, err := s.grid.Hours(rctx, after, horizon)
	if err != nil {
		return res, fmt.Errorf("read grid hours: %w", err)
	}
	count.add(1)
	built, err := st.PlanHours(rctx, after, horizon)
	if err != nil {
		return res, fmt.Errorf("read plan hours: %w", err)
	}
	// Keyed by Unix seconds: hour starts are whole seconds, and time.Time
	// keys would also compare locations.
	planned := map[int64]PlanHour{}
	for _, h := range built {
		planned[h.Start.Unix()] = h
	}

	assigned := map[int64]bool{}
	for _, h := range hours {
		assigned[h.StartUTC.Unix()] = true
		c, cerr := s.clock(ctx, cache, h.Clock.ID)
		if cerr != nil {
			errs = append(errs, cerr)
			continue
		}
		if c == nil {
			res.Skipped++ // deleted since the grid was read
			continue
		}
		forced := slices.ContainsFunc(force, func(r Range) bool { return r.contains(h.StartUTC) })
		if ph, ok := planned[h.StartUTC.Unix()]; ok && !forced && ph.current(*c) {
			res.Skipped++
			continue
		}
		todo = append(todo, hourJob{start: h.StartUTC, slotID: new(h.SlotID), clock: *c, force: forced, count: count})
	}
	count.add(len(cache)) // one clock read per distinct clock
	var clears []time.Time
	for _, h := range built {
		if assigned[h.Start.Unix()] || set.frozen(h.Start, now) {
			continue
		}
		clears = append(clears, h.Start)
	}

	// The snapshot holds the playlists of the clocks this pass builds; it is
	// read only when there is something to build.
	var lib Library
	if len(todo) > 0 {
		cs := make([]clocks.Clock, len(todo))
		for i, j := range todo {
			cs[i] = j.clock
		}
		lib, err = libraryFor(rctx, st, scopeOf(cs, nil))
		if err != nil {
			return res, err
		}
	}
	active := len(todo) > 0
	var runStart memStats
	if active {
		runStart = readMem()
		considered := len(hours)
		if started != nil {
			considered++ // the started hour is not one of hours
		}
		s.logPassStart(passShort, trigger, after, horizon, len(todo), considered-len(todo), runStart)
	}
	for _, j := range todo {
		out, err := s.build(ctx, j, &lib, set)
		if err != nil {
			errs = append(errs, fmt.Errorf("build hour %s: %w", j.start.Format(time.RFC3339), err))
			continue
		}
		if out.skipped {
			res.Skipped++
			continue
		}
		res.Built++
		res.Items += len(out.plan.Items)
		res.Warnings += len(out.plan.Warnings)
		for _, w := range out.plan.Warnings {
			res.ByCode[w.Code]++
		}
	}
	for _, start := range clears {
		if err := s.clear(ctx, start, count); err != nil {
			errs = append(errs, fmt.Errorf("clear hour %s: %w", start.Format(time.RFC3339), err))
			continue
		}
		res.Cleared++
	}
	if active {
		s.logPassEnd(runTotals{
			pass: passShort, trigger: trigger, elapsed: s.now().Sub(began), built: res.Built, skipped: res.Skipped,
			items: res.Items, byCode: res.ByCode, queries: count.n, batches: res.Built + res.Cleared,
			mem: readMem(), base: runStart, slowMS: set.FullSlowMS, heapWarn: set.FullHeapWarnMB,
		})
	}
	return res, errors.Join(errs...)
}

// startedHour returns the job of the grid hour that contains now, if it has
// no plan_hours row and enough of it is left; nil otherwise. A row of any
// kind, even with no items, keeps the hour as it is.
func (s *Service) startedHour(ctx context.Context, st Store, cache map[uint64]*clocks.Clock, now time.Time) (*hourJob, error) {
	// The hours that start in (now - 1 h, now]: the one that contains now,
	// whatever the station's offset from UTC.
	hours, err := s.grid.Hours(ctx, now.Add(time.Nanosecond-time.Hour), now.Add(time.Nanosecond))
	if err != nil {
		return nil, fmt.Errorf("read the grid hour on air: %w", err)
	}
	if len(hours) == 0 {
		return nil, nil
	}
	h := hours[len(hours)-1]
	if _, ok := startedOffset(h.StartUTC, now); !ok {
		return nil, nil
	}
	_, found, err := st.PlanHour(ctx, h.StartUTC)
	if err != nil {
		return nil, fmt.Errorf("read plan hour %s: %w", h.StartUTC.Format(time.RFC3339), err)
	}
	if found {
		return nil, nil
	}
	c, err := s.clock(ctx, cache, h.Clock.ID)
	if err != nil || c == nil {
		return nil, err
	}
	return &hourJob{start: h.StartUTC, slotID: new(h.SlotID), clock: *c, started: true}, nil
}

// startedOffset returns where the first build of the hour that starts at
// start begins when it is built at now, from the start of the hour; false if
// too little of the hour is left.
func startedOffset(start, now time.Time) (int, bool) {
	from := now.Add(startedHourLead).Sub(start)
	if time.Hour-from < startedHourMinRemainder {
		return 0, false
	}
	// Rounded up: the first item never starts before now + lead.
	return int((from + time.Millisecond - 1) / time.Millisecond), true
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

// RunJob is the 5-minute plan job (§7): the plan is synced to its horizon.
func (s *Service) RunJob(ctx context.Context) (string, error) {
	res, err := s.sync(ctx, TriggerJob, nil)
	return res.String(), err
}
