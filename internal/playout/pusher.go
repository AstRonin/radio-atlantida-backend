package playout

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"strconv"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

const (
	// passInterval is how often the pusher checks the queues without being
	// woken (a start callback wakes it at once). The hour switch runs in
	// Liquidsoap, so a pass only needs to be on time for the pre-push
	// (minutes) and as the heartbeat of the link; every pass reads the
	// settings from the database, so it is not more frequent than needed.
	passInterval = 5 * time.Second
	// passTimeout bounds one pass: a few queries and telnet commands.
	passTimeout = 10 * time.Second
	// pushCutoff: nothing is pushed into the queue of an hour this close to
	// its end. The script flushes that queue at the boundary; a request
	// still being resolved then would survive the flush.
	pushCutoff = 5 * time.Second
	// lateLimit: an item that should have started longer ago than this is
	// not pushed (no catch-up after a downtime, §7.1); it stays planned.
	lateLimit = 30 * time.Second
	// holdLead: an item held after a resync is pushed this long before its
	// planned start; the request must resolve before it plays.
	holdLead = 500 * time.Millisecond
	// retryLog bounds the WARN logs of a link that stays down: one per
	// this interval, not one per pass.
	retryLog = time.Minute

	varHourShift = "hour_shift"
	varMusicGain = "music_gain"
	varDuckGain  = "duck_gain"
)

// pusherState belongs to the Run goroutine.
type pusherState struct {
	// gen is the control connection the queues were last checked on.
	gen uint64
	// shift is the hour shift last set in Liquidsoap; -1: not set.
	shift int
	// duck is the duck gain last set in Liquidsoap; -1: not set.
	duck float64
	// output is the output.set command Liquidsoap last accepted; empty:
	// not sent on this connection.
	output string
	// outputErr and outputErrAt are the last refusal of output.set and
	// when it was logged.
	outputErr   string
	outputErrAt time.Time
	// lastWarn is when a failed pass was last logged.
	lastWarn time.Time
	// align holds the hours (UTC hour starts) whose Liquidsoap queue was
	// empty at the last resync: their first item is held until its planned
	// start, so the hour stays on its plan (fill).
	align map[time.Time]bool
	// holdUntil is when the held item is due (its planned start - holdLead);
	// zero when nothing is held. Every pass computes it again.
	holdUntil time.Time
}

// Run is the pusher: a pass every passInterval and whenever Wake is called,
// until ctx ends. A failed pass is logged and retried at the next one.
func (s *Service) Run(ctx context.Context) error {
	s.push.shift, s.push.duck = -1, -1
	t := time.NewTicker(passInterval)
	defer t.Stop()
	for {
		pctx, cancel := context.WithTimeout(ctx, passTimeout)
		err := s.pass(pctx)
		cancel()
		s.passDone(err)
		// A held item is pushed at its moment, not at the next tick.
		var held <-chan time.Time
		var timer *time.Timer
		if !s.push.holdUntil.IsZero() {
			timer = time.NewTimer(max(0, s.push.holdUntil.Sub(s.now())))
			held = timer.C
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-s.wake:
		case <-held:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (s *Service) passDone(err error) {
	now := s.now()
	switch {
	case err == nil:
		if !s.push.lastWarn.IsZero() {
			s.log.Info("playout pusher recovered")
			s.push.lastWarn = time.Time{}
		}
	case errors.Is(err, context.Canceled):
	case now.Sub(s.push.lastWarn) >= retryLog:
		// WARN, not ERROR: a Liquidsoap restart is routine, and the outage
		// detector (Phase 10) raises the alert when air is affected.
		s.log.Warn("playout pass failed", logging.KeyError, err)
		s.push.lastWarn = now
	}
}

// pass keeps the link set up and the queues of the hour on air and, within
// the pre-push window, of the next hour filled.
func (s *Service) pass(ctx context.Context) error {
	set, err := s.settings(ctx)
	if err != nil {
		return fmt.Errorf("read playout settings: %w", err)
	}
	now := s.now().UTC()
	// Cleared before anything can fail: a failed pass must not leave a due
	// time in the past, which would make Run pass again at once.
	s.push.holdUntil = time.Time{}
	// Before the link: heartbeats are lost exactly when Liquidsoap is gone.
	s.safetyExpire(now, set.HeartbeatTimeout)
	cur := hourStart(now, set.Location)
	// An hour that ended while its first item was held needs no alignment:
	// the next hour starts at the boundary by the script's switch.
	maps.DeleteFunc(s.push.align, func(h time.Time, _ bool) bool { return h.Before(cur) })
	next := cur.Add(time.Hour)
	hours := []time.Time{cur}
	if next.Sub(now) <= set.Prepush {
		hours = append(hours, next)
	}

	// Ping connects when needed and is the heartbeat of the link.
	if err := s.ctl.Ping(ctx); err != nil {
		return err
	}
	if gen := s.ctl.Generation(); gen != s.push.gen {
		if err := s.resync(ctx, hours, set); err != nil {
			return err
		}
		s.push.gen = gen
	}
	if err := s.applyShift(ctx, now, set); err != nil {
		return err
	}
	if err := s.applyGain(ctx); err != nil {
		return err
	}
	if err := s.applyDuck(ctx, set); err != nil {
		return err
	}
	if err := s.applyOutput(ctx, set); err != nil {
		return err
	}
	for _, h := range hours {
		if err := s.fill(ctx, h, now, set); err != nil {
			return err
		}
	}
	return nil
}

// resync runs on a new control connection: Liquidsoap may have restarted
// and lost its queues and variables. The variables and the stream output
// are set again (the script ignores an output equal to its own); the
// queued items of an hour whose queue is empty go back to planned, so they
// are pushed again. An empty queue of the hour on air holds its first item
// until its planned start (fill), so the hour stays on its plan; the gap
// before it is the emergency source. The next hour is not held: its queue
// does not play before the boundary. (If a start callback of the last
// queued item is in flight at that moment, that item may play twice; a
// connection is new only after a failure, so this is rare.)
func (s *Service) resync(ctx context.Context, hours []time.Time, set Settings) error {
	s.push.shift, s.push.duck, s.push.output = -1, -1, ""
	// A new resync decides again from the queues it finds.
	clear(s.push.align)
	s.push.holdUntil = time.Time{}
	s.mu.Lock()
	if s.gain == nil {
		s.gain = new(set.MusicGain)
	}
	s.gainPending = true
	s.mu.Unlock()

	for i, h := range hours {
		rids, err := s.ctl.Queue(ctx, QueueName(h, set.Location))
		if err != nil {
			return err
		}
		if len(rids) > 0 {
			continue
		}
		if i == 0 {
			// Held whether or not items come back: a first start mid-hour has
			// nothing queued and the same problem.
			s.push.align[h] = true
		}
		dctx, cancel := context.WithTimeout(ctx, dbTimeout)
		n, err := s.store.Unqueue(dctx, h, h.Add(time.Hour))
		cancel()
		if err != nil {
			return fmt.Errorf("unqueue hour %s: %w", h.Format(time.RFC3339), err)
		}
		if n > 0 {
			s.log.Warn("liquidsoap queue lost its items, pushing them again",
				"queue", QueueName(h, set.Location), "hour_start_utc", h, "items", n)
		}
	}
	s.log.Info("liquidsoap control link set up", "generation", s.ctl.Generation())
	return nil
}

// applyShift sets hour_shift in Liquidsoap when the station offset changes
// (and after a reconnect), so the script switches queues on the station's
// hour boundaries.
func (s *Service) applyShift(ctx context.Context, now time.Time, set Settings) error {
	shift := hourShift(now, set.Location)
	if shift == s.push.shift {
		return nil
	}
	if err := s.ctl.SetVar(ctx, varHourShift, liquidsoap.FormatFloat(float64(shift))); err != nil {
		return fmt.Errorf("set hour shift: %w", err)
	}
	s.push.shift = shift
	return nil
}

// applyDuck sets the music level under live voice (setting
// liquidsoap.duck_gain) after a reconnect or a settings change; the script
// fades the music to it while the harbor has a source (§8.5).
func (s *Service) applyDuck(ctx context.Context, set Settings) error {
	if set.DuckGain == s.push.duck {
		return nil
	}
	if err := s.ctl.SetVar(ctx, varDuckGain, liquidsoap.FormatFloat(set.DuckGain)); err != nil {
		return fmt.Errorf("set duck gain: %w", err)
	}
	s.push.duck = set.DuckGain
	return nil
}

// applyGain sets the music gain after a reconnect or a settings change.
func (s *Service) applyGain(ctx context.Context) error {
	s.mu.Lock()
	pending, gain := s.gainPending, s.gain
	s.mu.Unlock()
	if !pending || gain == nil {
		return nil
	}
	if err := s.ctl.SetVar(ctx, varMusicGain, liquidsoap.FormatFloat(*gain)); err != nil {
		return fmt.Errorf("set music gain: %w", err)
	}
	s.mu.Lock()
	if s.gain != nil && *s.gain == *gain {
		s.gainPending = false
	}
	s.mu.Unlock()
	return nil
}

// fill pushes the items of the hour that starts at h until its queue holds
// Lookahead items, in planned order after the last queued one. After a
// resync found the queue of the hour empty, its first item that can still
// air is held until its planned start - holdLead (holdUntil), so it starts
// on its second and not at once. The edge cases
// (docs/output-state-and-resync-plan.md P2.5):
//   - a held item already late (within lateLimit) is pushed at once, as
//     before; one later than that is skipped and the next one is held;
//   - every pass reads the items again, so a plan change during the hold
//     moves the held item and its due time;
//   - live voice and the music gain do not touch the hour queue, so they
//     do not change the hold;
//   - the held item cuts the emergency track at once (track_sensitive=false),
//     as whenever the plan resumes.
func (s *Service) fill(ctx context.Context, h, now time.Time, set Settings) error {
	if !now.Before(h.Add(time.Hour - pushCutoff)) {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	items, err := s.store.Items(dctx, h, h.Add(time.Hour))
	cancel()
	if err != nil {
		return fmt.Errorf("read items of hour %s: %w", h.Format(time.RFC3339), err)
	}
	queued, after := 0, 0
	for i, it := range items {
		if it.Queued {
			queued++
			after = i + 1
		}
	}
	if s.push.align[h] {
		if queued > 0 {
			delete(s.push.align, h) // pushed meanwhile: the queue plays on its own
		} else if due, ok := holdDue(items[after:], now); !ok || now.Before(due) {
			if ok && (s.push.holdUntil.IsZero() || due.Before(s.push.holdUntil)) {
				s.push.holdUntil = due
			}
			return nil
		}
	}
	queue := QueueName(h, set.Location)
	for _, it := range items[after:] {
		if queued >= set.Lookahead {
			break
		}
		if !canAir(it, now) {
			continue // too late to air; Phase 10 marks it missed
		}
		ok, err := s.markQueued(ctx, it.ID)
		if err != nil {
			return err
		}
		if !ok {
			if s.push.align[h] {
				return nil // the held item changed; the next pass holds again
			}
			continue // rebuilt or started meanwhile; the next pass reads again
		}
		if _, err := s.ctl.Push(ctx, queue, RequestURI(it, set)); err != nil {
			// Not pushed: planned again, so the next pass retries it.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
			rerr := s.store.MarkPlanned(rctx, it.ID)
			cancel()
			return errors.Join(fmt.Errorf("push item %d: %w", it.ID, err), rerr)
		}
		queued++
		delete(s.push.align, h)
		s.log.Debug("item pushed", "item_id", it.ID, "queue", queue, "planned_start_utc", it.Start)
	}
	return nil
}

// canAir reports whether an item may still be pushed at now: it has not
// ended, and it should have started at most lateLimit ago (§7.1: no
// catch-up).
func canAir(it Item, now time.Time) bool {
	return it.end().After(now) && !it.Start.Before(now.Add(-lateLimit))
}

// holdDue returns when the first item that can still air is pushed after a
// resync: its planned start - holdLead. False when none can air.
func holdDue(items []Item, now time.Time) (time.Time, bool) {
	for _, it := range items {
		if canAir(it, now) {
			return it.Start.Add(-holdLead), true
		}
	}
	return time.Time{}, false
}

// markQueued marks an item queued under the plan lock, so a build of its
// hour that runs at the same moment either finishes first (and the item is
// gone) or sees the hour frozen.
func (s *Service) markQueued(ctx context.Context, id uint64) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var ok bool
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.LockPlan(ctx); err != nil {
			return err
		}
		var err error
		ok, err = st.MarkQueued(ctx, id)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("mark item %d queued: %w", id, err)
	}
	return ok, nil
}

// hourStart returns the start of the station-local hour that contains t,
// in UTC.
func hourStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc).UTC()
}

// hourShift is the station UTC offset at t modulo one hour, in seconds: the
// UTC second within the hour at which station hours start.
func hourShift(t time.Time, loc *time.Location) int {
	_, off := t.In(loc).Zone()
	return ((off % 3600) + 3600) % 3600
}

// QueueName returns the Liquidsoap queue of the station hour that starts at
// h: hour_0 or hour_1 by the parity of the hour. The script selects the
// queue with the same formula, floor((t - hour_shift) / 3600) mod 2.
func QueueName(h time.Time, loc *time.Location) string {
	idx := (h.Unix() - int64(hourShift(h, loc))) / 3600
	return "hour_" + strconv.FormatInt(idx%2, 10)
}

// RequestURI returns the request pushed for an item (§8.1): the file under
// the media root, annotated with the item ID; the in point; the out point
// when the item is truncated or ends before the end of the file; the fades
// (the track's own at its cue points, or the fade that ends a truncated
// item); and the library title and artist, so the stream metadata is what
// the manager entered, never the file tags. The artist is always sent: an
// empty one hides the artist tag of the file.
func RequestURI(it Item, set Settings) string {
	ann := []liquidsoap.Pair{{Key: "item_id", Value: strconv.FormatUint(it.ID, 10)}}
	if it.InMS > 0 {
		ann = append(ann, liquidsoap.Pair{Key: "liq_cue_in", Value: seconds(it.InMS)})
	}
	if it.Truncated || (it.FileDurationMS != nil && it.OutMS < *it.FileDurationMS) {
		ann = append(ann, liquidsoap.Pair{Key: "liq_cue_out", Value: seconds(it.OutMS)})
	}
	if it.FadeInMS > 0 {
		ann = append(ann, liquidsoap.Pair{Key: "liq_fade_in", Value: seconds(it.FadeInMS)})
	}
	switch {
	case it.Truncated:
		ann = append(ann, liquidsoap.Pair{Key: "liq_fade_out", Value: seconds(set.FadeOutMS)})
	case it.FadeOutMS > 0:
		ann = append(ann, liquidsoap.Pair{Key: "liq_fade_out", Value: seconds(it.FadeOutMS)})
	}
	ann = append(ann, liquidsoap.Pair{Key: "title", Value: it.Title}, liquidsoap.Pair{Key: "artist", Value: it.Artist})
	return liquidsoap.Annotate(path.Join(set.MediaRoot, it.FilePath), ann...)
}

func seconds(ms int) string {
	return liquidsoap.FormatFloat(float64(ms) / 1000)
}
