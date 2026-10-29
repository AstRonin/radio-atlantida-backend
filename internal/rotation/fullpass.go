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
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// The full pass rebuilds the plan of the whole range, from now to the grid
// horizon, in batches of rotation.full_pass_batch_hours hours: one
// transaction per batch, the plan, inserts, queue, library and history read
// once per batch (docs/rotation-long-horizon-plan.md, "Full pass").
const (
	// FullPassTimeout bounds one full pass, from its start to its last batch.
	// Measured on the 800 000-track library (266 679 tracks in the snapshot of
	// the clocks, six months of grid, 3648 hours built, PostgreSQL in Docker on a
	// Mac): 3 min 5 s. Fifteen minutes is about five times that, which leaves
	// room for a PC running Docker Desktop, where the same pass is expected to
	// take two to three times as long. The earlier 2000-track measurement (34 s)
	// set five minutes; the larger library shows that Build's cost per hour grows
	// with the playlists of the clock.
	FullPassTimeout = 15 * time.Minute
	// fullBatchTimeout bounds the transaction of one batch: its reads, the
	// builds (CPU only), and the writes.
	fullBatchTimeout = 2 * time.Minute
)

// ErrFullPassRunning refuses a full pass while another one runs.
var ErrFullPassRunning = errors.New("a full pass of the track plan is running")

// SkipReason says why the full pass does not build an hour.
type SkipReason string

// Reasons of a skip.
const (
	// SkipStarted: the hour has started, or is so close to its start that
	// playout may have pushed its items (PlanSettings.Freeze).
	SkipStarted SkipReason = "started"
	// SkipQueued: an item of the hour is in Liquidsoap's queue.
	SkipQueued SkipReason = "queued"
	// SkipUnchanged: the stored fingerprint equals the one of the current
	// inputs (see decideHour).
	SkipUnchanged SkipReason = "unchanged"
	// SkipNoClock: the grid's clock no longer exists.
	SkipNoClock SkipReason = "no_clock"
)

// hourFacts is what decideHour reads of one hour.
type hourFacts struct {
	Start  time.Time
	Now    time.Time
	Freeze time.Duration
	// Clock is nil when the grid's clock no longer exists.
	Clock *clocks.Clock
	// Stored is the built hour; nil when the hour is not built.
	Stored *PlanHour
	// Queued is true when an item of the hour is in Liquidsoap's queue.
	Queued bool
	// InputsHash is the fingerprint of the current inputs (inputsHash).
	InputsHash string
}

// decideHour says whether the full pass builds the hour, and why not when it
// does not. The rules, in order:
//
//  1. Started: the hour starts at or before now, or a built hour starts
//     within the freeze. Skipped.
//  2. No clock: the grid's clock is gone. Skipped.
//  3. Queued: an item is in Liquidsoap's queue. Skipped.
//  4. Unchanged: the hour is built, its stored fingerprint is not empty and
//     equals the current one. The fingerprint covers the clock (ID and
//     version), the one-time inserts that stay, the build settings (minimum
//     gap, station timezone) and the library version (the tracks and play
//     rules a build reads). The history is not an input: the plays of the
//     hours before are what the 5-minute pass already leaves as they are.
//     Skipped.
//  5. Otherwise the hour is built (a missing hour, or one whose inputs
//     changed, or one built before migration 0016).
func decideHour(f hourFacts) (build bool, reason SkipReason) {
	frozen := !f.Start.After(f.Now.Add(f.Freeze))
	switch {
	case !f.Start.After(f.Now) || (f.Stored != nil && frozen):
		return false, SkipStarted
	case f.Clock == nil:
		return false, SkipNoClock
	case f.Queued:
		return false, SkipQueued
	case f.Stored != nil && f.Stored.InputsHash != "" && f.Stored.InputsHash == f.InputsHash:
		return false, SkipUnchanged
	}
	return true, ""
}

// hourDecision is what the full pass does with one hour of its range.
type hourDecision struct {
	start  time.Time
	slotID uint64
	// build: the hour is built from clock with inserts.
	build  bool
	clear  bool
	reason SkipReason
	clock  clocks.Clock
	// stored is the built hour, nil if none; inserts are the hour's one-time
	// inserts in ID order.
	stored  *PlanHour
	inserts []Insert
}

// fullInputs are the reads a decision needs: the grid hours and the built
// hours, the one-time inserts, the queued hours and the clocks of a range.
type fullInputs struct {
	hours   []grid.ScheduledHour
	stored  []PlanHour
	inserts []Insert
	queued  []time.Time
	clocks  map[uint64]*clocks.Clock
	lib     *Library
}

// decideRange decides every hour of the range in time order: the grid
// hours, and the built hours the grid no longer assigns, which are cleared
// unless they are frozen or queued.
func decideRange(in fullInputs, set PlanSettings, now time.Time) []hourDecision {
	stored := map[int64]PlanHour{}
	for _, h := range in.stored {
		stored[h.Start.Unix()] = h
	}
	inserts := map[int64][]Insert{}
	for _, ins := range in.inserts {
		inserts[ins.HourStart.Unix()] = append(inserts[ins.HourStart.Unix()], ins)
	}
	queued := map[int64]bool{}
	for _, t := range in.queued {
		queued[t.Unix()] = true
	}
	loc := locationName(set.Location)

	var out []hourDecision
	assigned := map[int64]bool{}
	for _, h := range in.hours {
		key := h.StartUTC.Unix()
		assigned[key] = true
		c := in.clocks[h.Clock.ID]
		var st *PlanHour
		if ph, ok := stored[key]; ok {
			st = &ph
		}
		ins := inserts[key]
		hash := ""
		if c != nil {
			hash = inputsHash(*c, ins, set.Build, loc, in.lib)
		}
		d := hourDecision{start: h.StartUTC, slotID: h.SlotID, stored: st, inserts: ins}
		if c != nil {
			d.clock = *c
		}
		d.build, d.reason = decideHour(hourFacts{
			Start: h.StartUTC, Now: now, Freeze: set.Freeze, Clock: c, Stored: st, Queued: queued[key], InputsHash: hash,
		})
		out = append(out, d)
	}
	for _, h := range in.stored {
		key := h.Start.Unix()
		if assigned[key] {
			continue
		}
		ph := h
		d := hourDecision{start: h.Start, stored: &ph, inserts: inserts[key]}
		switch {
		case queued[key]:
			d.reason = SkipQueued
		case set.frozen(h.Start, now):
			d.reason = SkipStarted
		default:
			d.clear = true
		}
		out = append(out, d)
	}
	slices.SortStableFunc(out, func(a, b hourDecision) int { return a.start.Compare(b.start) })
	return out
}

// batchRange is one batch of a full pass: the time range it reads and
// decides, and the grid hours [lo, hi) it covers.
type batchRange struct {
	from, to time.Time
	lo, hi   int
}

// batchRanges cuts the grid hours (time order) into batches of perBatch
// hours. The ranges are contiguous and cover [from, to), so a built hour
// that the grid no longer assigns falls into the batch of the hour before it.
func batchRanges(hours []grid.ScheduledHour, from, to time.Time, perBatch int) []batchRange {
	if len(hours) == 0 {
		return []batchRange{{from: from, to: to}}
	}
	n := (len(hours) + perBatch - 1) / perBatch
	out := make([]batchRange, 0, n)
	for k := range n {
		lo, hi := k*perBatch, min((k+1)*perBatch, len(hours))
		b := batchRange{from: from, to: to, lo: lo, hi: hi}
		if k > 0 {
			b.from = hours[lo].StartUTC
		}
		if k < n-1 {
			b.to = hours[hi].StartUTC
		}
		out = append(out, b)
	}
	return out
}

// fullTally counts what a full pass did.
type fullTally struct {
	built, cleared, items, batches int
	skipped                        map[SkipReason]int
	byCode                         map[WarningCode]int
}

func newFullTally() *fullTally {
	return &fullTally{skipped: map[SkipReason]int{}, byCode: map[WarningCode]int{}}
}

// merge adds the counts of a committed batch.
func (t *fullTally) merge(b *fullTally) {
	t.built += b.built
	t.cleared += b.cleared
	t.items += b.items
	for r, n := range b.skipped {
		t.skipped[r] += n
	}
	for c, n := range b.byCode {
		t.byCode[c] += n
	}
}

// skippedTotal counts the hours that were not built.
func (t *fullTally) skippedTotal() int {
	n := 0
	for _, c := range t.skipped {
		n += c
	}
	return n
}

// summary is the result summary of the operation.
func (t *fullTally) summary() string {
	return fmt.Sprintf("built %d hours, skipped %d, cleared %d, %d items", t.built, t.skippedTotal(), t.cleared, t.items)
}

// RunNightly is the nightly job's run: a full pass started by the nightly
// trigger.
func (s *Service) RunNightly(ctx context.Context) (string, error) {
	return s.FullPass(ctx, TriggerNightly)
}

// FullPass rebuilds the track plan from now to the grid horizon: it builds
// the hours that changed, skips the rest (decideHour), and reports the
// progress per batch. It runs in its own transaction per batch, under
// FullPassTimeout, and it publishes rotation.plan_full at its start and its
// end. Only one runs at a time: ErrFullPassRunning otherwise. ctx must be the
// service context, never a request context.
func (s *Service) FullPass(ctx context.Context, trigger Trigger) (string, error) {
	if !s.full.CompareAndSwap(false, true) {
		return "", ErrFullPassRunning
	}
	defer s.full.Store(false)
	ctx, cancel := context.WithTimeout(ctx, FullPassTimeout)
	defer cancel()
	began := s.now()

	rctx, rcancel := context.WithTimeout(ctx, dbTimeout)
	set, err := s.settings(rctx)
	rcancel()
	if err != nil {
		return "", fmt.Errorf("read rotation settings: %w", err)
	}
	now := s.now().UTC()
	from := now.Add(time.Nanosecond)
	to := now.Add(time.Duration(set.GridHorizonDays) * 24 * time.Hour)
	tally := newFullTally()
	s.publishFull(ws.TrackPlanFullStarted, tally, 0)

	err = s.fullRun(ctx, trigger, set, from, to, began, tally)
	elapsed := s.now().Sub(began)
	if err != nil {
		s.publishFull(ws.TrackPlanFullFailed, tally, elapsed)
		return "", err
	}
	s.publishFull(ws.TrackPlanFullCompleted, tally, elapsed)
	return tally.summary(), nil
}

// fullRun is the body of a full pass: it decides the whole range once for the
// start line, then builds it batch by batch.
func (s *Service) fullRun(ctx context.Context, trigger Trigger, set PlanSettings, from, to, began time.Time, tally *fullTally) error {
	count := &queryCount{}
	st := countingStore{Store: s.store, n: count}
	count.add(1) // settings
	hours, err := s.grid.Hours(ctx, from, to)
	if err != nil {
		return fmt.Errorf("read grid hours: %w", err)
	}
	count.add(1)
	slices.SortFunc(hours, func(a, b grid.ScheduledHour) int { return a.StartUTC.Compare(b.StartUTC) })
	batches := batchRanges(hours, from, to, set.FullBatchHours)

	lib, decisions, err := s.readFull(ctx, st, count, hours, from, to, set)
	if err != nil {
		return err
	}
	var toBuild, toSkip int
	for _, d := range decisions {
		switch {
		case d.build:
			toBuild++
		case !d.clear:
			toSkip++
		}
	}
	runStart := readMem()
	s.logPassStart(passFull, trigger, from, to, toBuild, toSkip, runStart)

	for k, b := range batches {
		batchStart := readMem()
		s.logBatch("track plan batch started", k+1, len(batches), b.from, b.to, 0, 0, batchStart, batchStart)
		bt, err := s.fullBatch(ctx, count, b, hours[b.lo:b.hi], set, s.now().UTC(), &lib)
		if err != nil {
			return fmt.Errorf("batch %d of %d (%s to %s): %w", k+1, len(batches),
				b.from.Format(time.RFC3339), b.to.Format(time.RFC3339), err)
		}
		tally.merge(bt)
		tally.batches++
		end := readMem()
		s.logBatch("track plan batch finished", k+1, len(batches), b.from, b.to, bt.built, bt.skippedTotal(), end, batchStart)
		s.checkHeap(passFull, end, set.FullHeapWarnMB)
		if err := s.ops.Checkpoint(ctx, operations.KindTrackPlanFull, b.hi, len(hours)); err != nil {
			return fmt.Errorf("store progress: %w", err)
		}
	}
	s.logPassEnd(runTotals{
		pass: passFull, trigger: trigger, elapsed: s.now().Sub(began), built: tally.built, skipped: tally.skippedTotal(),
		items: tally.items, byCode: tally.byCode, queries: count.n, batches: tally.batches, mem: readMem(),
		base: runStart, slowMS: set.FullSlowMS, heapWarn: set.FullHeapWarnMB,
	})
	return nil
}

// readFull reads the inputs of a range and decides it, without writing: the
// start line of a full pass counts with it.
func (s *Service) readFull(ctx context.Context, st countingStore, count *queryCount, hours []grid.ScheduledHour,
	from, to time.Time, set PlanSettings,
) (Library, []hourDecision, error) {
	stored, err := st.PlanHours(ctx, from, to)
	if err != nil {
		return Library{}, nil, fmt.Errorf("read plan hours: %w", err)
	}
	inserts, err := st.InsertsRange(ctx, from, to)
	if err != nil {
		return Library{}, nil, fmt.Errorf("read hour inserts: %w", err)
	}
	queued, err := st.QueuedHours(ctx, from, to)
	if err != nil {
		return Library{}, nil, fmt.Errorf("read queued hours: %w", err)
	}
	cache, err := s.clocksOf(ctx, hours, count)
	if err != nil {
		return Library{}, nil, err
	}
	// The snapshot of the run: the playlists of the clocks of the range and
	// the tracks of the inserts. It is read once for the run.
	lib, err := libraryFor(ctx, st, scopeOf(clocksIn(cache), inserts))
	if err != nil {
		return Library{}, nil, err
	}
	in := fullInputs{hours: hours, stored: stored, inserts: inserts, queued: queued, clocks: cache, lib: &lib}
	return lib, decideRange(in, set, s.now().UTC()), nil
}

// clocksIn returns the clocks of a clock cache, the deleted ones left out.
func clocksIn(cache map[uint64]*clocks.Clock) []clocks.Clock {
	var out []clocks.Clock
	for _, c := range cache {
		if c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// clocksOf reads the clocks of the grid hours, once each.
func (s *Service) clocksOf(ctx context.Context, hours []grid.ScheduledHour, count *queryCount) (map[uint64]*clocks.Clock, error) {
	cache := map[uint64]*clocks.Clock{}
	for _, h := range hours {
		if _, err := s.clock(ctx, cache, h.Clock.ID); err != nil {
			return nil, err
		}
	}
	count.add(len(cache))
	return cache, nil
}

// fullBatch builds the changed hours of one batch in one transaction, under
// the plan lock. It reads the plan, the inserts, the queue, the library and
// the history of its range once; each built hour replaces its planned items
// and keeps its one-time inserts. It returns what the batch did, which the
// caller merges only when the transaction commits.
func (s *Service) fullBatch(ctx context.Context, count *queryCount, b batchRange, hours []grid.ScheduledHour,
	set PlanSettings, now time.Time, lib *Library,
) (*fullTally, error) {
	bctx, cancel := context.WithTimeout(ctx, fullBatchTimeout)
	defer cancel()
	bt := newFullTally()
	err := s.tx.WithTx(bctx, func(ctx context.Context, tx db.Tx) error {
		st := countingStore{Store: s.store.WithTx(tx), n: count}
		if err := st.LockPlan(ctx); err != nil {
			return err
		}
		stored, err := st.PlanHours(ctx, b.from, b.to)
		if err != nil {
			return fmt.Errorf("read plan hours: %w", err)
		}
		inserts, err := st.InsertsRange(ctx, b.from, b.to)
		if err != nil {
			return fmt.Errorf("read hour inserts: %w", err)
		}
		queued, err := st.QueuedHours(ctx, b.from, b.to)
		if err != nil {
			return fmt.Errorf("read queued hours: %w", err)
		}
		span := lib.maxRuleInterval()
		plays, err := st.Window(ctx, b.from.Add(-span), b.to.Add(span))
		if err != nil {
			return fmt.Errorf("read history: %w", err)
		}
		cache, err := s.clocksOf(ctx, hours, count)
		if err != nil {
			return err
		}
		// A track or playlist the run's snapshot lacks is read now.
		if err := ensureLibrary(ctx, st, lib, scopeOf(clocksIn(cache), inserts)); err != nil {
			return err
		}
		decisions := decideRange(fullInputs{
			hours: hours, stored: stored, inserts: inserts, queued: queued, clocks: cache, lib: lib,
		}, set, now)
		// Hours of the range the grid no longer assigns sit between the grid
		// hours; decideRange returned them in time order, so they are
		// decided here too.
		decisions = slices.DeleteFunc(decisions, func(d hourDecision) bool {
			return !b.contains(d.start)
		})
		win := newWindow(plays)
		for _, d := range decisions {
			switch {
			case d.clear:
				if err := s.clearDecided(ctx, st, d, win); err != nil {
					return err
				}
				bt.cleared++
			case d.build:
				if err := s.buildDecided(ctx, st, d, lib, win, set, now, bt); err != nil {
					return err
				}
			default:
				bt.skipped[d.reason]++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return bt, nil
}

// contains reports whether the start of an hour lies in the batch's range.
func (b batchRange) contains(start time.Time) bool {
	return !start.Before(b.from) && start.Before(b.to)
}

// buildDecided builds one hour of a full pass with the history the window
// holds, settles its one-time inserts, stores its items, and records it in the
// window for the hours after it.
func (s *Service) buildDecided(ctx context.Context, st Store, d hourDecision, lib *Library, win *window,
	set PlanSettings, now time.Time, bt *fullTally,
) error {
	in := HourInput{
		Start: d.start, Location: set.Location, Clock: d.clock, HourInserts: d.inserts, Library: lib,
		History: win.forHour(d.start, lib.maxRuleInterval()), Settings: set.Build,
		Seed: uint64(d.start.Unix()), //nolint:gosec // G115: hour starts are after 1970
	}
	plan := Build(in)
	if err := settleInserts(ctx, st, d.inserts, &plan); err != nil {
		return fmt.Errorf("settle inserts of hour %s: %w", d.start.Format(time.RFC3339), err)
	}
	kept := slices.DeleteFunc(slices.Clone(d.inserts), func(ins Insert) bool {
		_, refused := plan.Refused[ins.ID]
		return refused
	})
	next := PlanHour{
		Start: d.start, ClockID: d.clock.ID, ClockUpdatedAt: d.clock.UpdatedAt, GridSlotID: new(d.slotID), BuiltAt: now,
		Seed:       int64(in.Seed), //nolint:gosec // G115: the seed is an hour start in seconds
		InputsHash: inputsHash(d.clock, kept, set.Build, locationName(set.Location), lib),
	}
	if err := st.ReplaceHour(ctx, next, plan.Items); err != nil {
		return fmt.Errorf("store hour %s: %w", d.start.Format(time.RFC3339), err)
	}
	if err := linkRescheduled(ctx, st, plan.Items, now); err != nil {
		return err
	}
	win.setHour(d.start, playsOf(d.start, plan.Items, lib))
	bt.built++
	bt.items += len(plan.Items)
	for _, w := range plan.Warnings {
		bt.byCode[w.Code]++
	}
	return nil
}

// clearDecided clears a built hour the grid no longer assigns, as the short
// pass does: its one-time inserts are removed, and their missed items open
// again.
func (s *Service) clearDecided(ctx context.Context, st Store, d hourDecision, win *window) error {
	if err := dropInserts(ctx, st, d.inserts); err != nil {
		return fmt.Errorf("drop inserts of hour %s: %w", d.start.Format(time.RFC3339), err)
	}
	if len(d.inserts) > 0 {
		s.log.Warn("the grid no longer plays an hour with one-time inserts; they are removed and their missed items are open again",
			"hour_start_utc", d.start, "count", len(d.inserts))
	}
	if err := st.ClearHour(ctx, d.start); err != nil {
		return fmt.Errorf("clear hour %s: %w", d.start.Format(time.RFC3339), err)
	}
	win.setHour(d.start, nil)
	return nil
}

// publishFull sends rotation.plan_full with the counts so far.
func (s *Service) publishFull(status string, t *fullTally, elapsed time.Duration) {
	s.pub.Publish(ws.TrackPlanFullEvent{
		Type: ws.EventTrackPlanFull, Time: s.now().UTC(),
		Data: ws.TrackPlanFullData{
			Status: status, HoursBuilt: t.built, HoursSkipped: t.skippedTotal(), Items: t.items,
			ElapsedMS: elapsed.Milliseconds(),
		},
	})
}

// NightlyDue reports whether the nightly full pass is due, and takes its claim
// when it is. It is due when rotation.full_pass_enabled is on, no full pass
// runs, the station local time has reached rotation.full_pass_time, and the
// station local date has no claim yet. The claim is a row in the database
// (rotation_nightly_passes), taken atomically: a restart or a second process
// does not run the night again, and a run that fails is not retried that day.
// A button pass takes no claim. The scheduler calls it on each check.
func (s *Service) NightlyDue(ctx context.Context) (bool, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return false, fmt.Errorf("read rotation settings: %w", err)
	}
	if !set.FullPassEnabled || s.full.Load() {
		return false, nil
	}
	at, err := time.Parse("15:04", set.FullPassTime)
	if err != nil {
		return false, fmt.Errorf("rotation.full_pass_time %q: %w", set.FullPassTime, err)
	}
	local := s.now().In(set.Location)
	if local.Hour()*60+local.Minute() < at.Hour()*60+at.Minute() {
		return false, nil
	}
	return s.store.ClaimNightly(rctx, local.Format(time.DateOnly))
}
