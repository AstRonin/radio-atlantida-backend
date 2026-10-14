package playout

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
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
	// lastWarn is when a failed pass was last logged.
	lastWarn time.Time
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
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-s.wake:
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
	cur := hourStart(now, set.Location)
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
	for _, h := range hours {
		if err := s.fill(ctx, h, now, set); err != nil {
			return err
		}
	}
	return nil
}

// resync runs on a new control connection: Liquidsoap may have restarted
// and lost its queues and variables. The variables are set again; the
// queued items of an hour whose queue is empty go back to planned, so they
// are pushed again. (If a start callback of the last queued item is in
// flight at that moment, that item may play twice; a connection is new only
// after a failure, so this is rare.)
func (s *Service) resync(ctx context.Context, hours []time.Time, set Settings) error {
	s.push.shift, s.push.duck = -1, -1
	s.mu.Lock()
	if s.gain == nil {
		s.gain = new(set.MusicGain)
	}
	s.gainPending = true
	s.mu.Unlock()

	for _, h := range hours {
		rids, err := s.ctl.Queue(ctx, QueueName(h, set.Location))
		if err != nil {
			return err
		}
		if len(rids) > 0 {
			continue
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
// Lookahead items, in planned order after the last queued one.
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
	queue := QueueName(h, set.Location)
	for _, it := range items[after:] {
		if queued >= set.Lookahead {
			break
		}
		if !it.end().After(now) || it.Start.Before(now.Add(-lateLimit)) {
			continue // too late to air; Phase 10 marks it missed
		}
		ok, err := s.markQueued(ctx, it.ID)
		if err != nil {
			return err
		}
		if !ok {
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
		s.log.Debug("item pushed", "item_id", it.ID, "queue", queue, "planned_start_utc", it.Start)
	}
	return nil
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
// the media root, annotated with the item ID and, for a truncated item, the
// out point and the fade that ends there.
func RequestURI(it Item, set Settings) string {
	ann := []string{`item_id="` + strconv.FormatUint(it.ID, 10) + `"`}
	if it.InMS > 0 {
		ann = append(ann, `liq_cue_in="`+seconds(it.InMS)+`"`)
	}
	if it.Truncated {
		ann = append(ann, `liq_cue_out="`+seconds(it.OutMS)+`"`, `liq_fade_out="`+seconds(set.FadeOutMS)+`"`)
	}
	return "annotate:" + strings.Join(ann, ",") + ":" + path.Join(set.MediaRoot, it.FilePath)
}

func seconds(ms int) string {
	return liquidsoap.FormatFloat(float64(ms) / 1000)
}
