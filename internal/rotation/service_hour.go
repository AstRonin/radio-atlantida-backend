package rotation

import (
	"context"
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// hourJob is one hour to build.
type hourJob struct {
	start  time.Time
	slotID *uint64
	clock  clocks.Clock
	// force rebuilds the hour even when it is current.
	force bool
	// started is the first build of an hour that has started: it covers what
	// is left of the hour (startedOffset).
	started bool
	// add stores this one-time insert before the build and fails the build
	// when it cannot be placed whole.
	add *Insert
	// remove deletes this one-time insert before the build.
	remove *Insert
	// move sets the offset and tolerance of this one-time insert before the
	// build, and fails the build (rolling it back) when it cannot be placed.
	move *Insert
	// count counts the store calls of the build; nil counts nothing.
	count *queryCount
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
		out, err = s.buildTx(ctx, countingStore{Store: s.store.WithTx(tx), n: j.count}, j, lib, set)
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
	case (j.add != nil || j.remove != nil || j.move != nil) && frozen:
		return hourOutcome{}, ErrHourStarted
	case found && frozen:
		// Its items may be in Liquidsoap's queue already.
		return hourOutcome{skipped: true}, nil
	case !j.force && found && ph.current(c):
		return hourOutcome{skipped: true}, nil
	}
	// From the time under the lock: the sync read may be seconds old. The
	// found && frozen case above makes this build one-shot.
	fromMS := 0
	if j.started {
		var ok bool
		if fromMS, ok = startedOffset(j.start, now); !ok {
			return hourOutcome{skipped: true}, nil
		}
	}

	// ownID is the one-time insert the job adds or moves; its refusal fails
	// the job instead of dropping the insert with a warning.
	var ownID uint64
	if j.add != nil {
		if ownID, err = s.storeInsert(ctx, st, *j.add); err != nil {
			return hourOutcome{}, err
		}
	}
	if j.move != nil {
		if err = st.UpdateInsert(ctx, *j.move); err != nil {
			return hourOutcome{}, err
		}
		ownID = j.move.ID
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
	if err = ensureLibrary(ctx, st, lib, scopeOf([]clocks.Clock{c}, inserts)); err != nil {
		return hourOutcome{}, err
	}
	span := lib.maxRuleInterval()
	// The window spans the whole hour, so a partial build also counts what
	// has aired in the hour up to now.
	history, err := st.History(ctx, j.start.Add(-span), j.start.Add(time.Hour+span), j.start)
	if err != nil {
		return hourOutcome{}, err
	}
	in := HourInput{
		Start: j.start, Location: set.Location, Clock: c, HourInserts: inserts, Library: lib, History: history,
		Settings: set.Build, Seed: uint64(j.start.Unix()), //nolint:gosec // G115: hour starts are after 1970
		FromMS: fromMS,
	}
	plan := Build(in)
	out := hourOutcome{plan: plan}
	if ownID != 0 {
		if r, ok := plan.Refused[ownID]; ok {
			return hourOutcome{}, &InsertError{Reason: r.Reason, Message: r.Message}
		}
		i := slices.IndexFunc(plan.Items, func(it Item) bool { return it.HourInsertID != nil && *it.HourInsertID == ownID })
		if i >= 0 {
			out.placed = &plan.Items[i]
		}
	}
	if err := settleInserts(ctx, st, inserts, &out.plan); err != nil {
		return hourOutcome{}, err
	}
	// The one-time inserts that refused are gone now; the fingerprint is of
	// the inserts that stay, so the next full pass finds the hour unchanged.
	kept := slices.DeleteFunc(slices.Clone(inserts), func(ins Insert) bool { _, refused := plan.Refused[ins.ID]; return refused })
	next := PlanHour{
		Start: j.start, ClockID: c.ID, ClockUpdatedAt: c.UpdatedAt, GridSlotID: j.slotID, BuiltAt: now,
		Seed:       int64(in.Seed), //nolint:gosec // G115: the seed is an hour start in seconds
		InputsHash: inputsHash(c, kept, set.Build, locationName(set.Location), lib),
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
func (s *Service) clear(ctx context.Context, start time.Time, count *queryCount) error {
	ctx, cancel := context.WithTimeout(ctx, hourTimeout)
	defer cancel()
	return s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := countingStore{Store: s.store.WithTx(tx), n: count}
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
