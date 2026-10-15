package outage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/broadcast"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

const (
	// tick is how often the signals are evaluated. Callbacks and the link
	// are in memory; the Icecast check and the writes have their own
	// intervals.
	tick = time.Second
	// tickTimeout bounds one evaluation: a settings read, at most one
	// Icecast check, a few queries.
	tickTimeout = 10 * time.Second
	// heartbeatEvery is how often the backend records that it is alive; it
	// bounds the error of the start of a backend outage.
	heartbeatEvery = 30 * time.Second
	// sweepEvery is how often unaired items are dropped and unmarked
	// outages retried.
	sweepEvery = 30 * time.Second
	// retryMarkEvery bounds the retries of a marking that failed.
	retryMarkEvery = 5 * time.Minute
	// staleAfter: an item that should have started this long ago and has
	// not did not air. Items start late only by drift (milliseconds).
	staleAfter = 2 * time.Minute
	// lateLimit is the pusher's no-catch-up limit (internal/playout): an
	// item this late is never pushed, so it can be resolved.
	lateLimit = 30 * time.Second
	// replayMargin widens a replay window to the past: an item that
	// overlaps the window may have started before it.
	replayMargin = time.Hour
	// opTimeout bounds a marking or the startup reconcile: a journal
	// replay and a few statements.
	opTimeout = 2 * time.Minute
	// retryLog bounds the WARN logs of a failure that repeats every tick.
	retryLog = time.Minute
	// backendReason is the reason of an outage recorded for backend
	// downtime.
	backendReason = "the backend was not running"
)

// Playout is the playout state the detector reads (*playout.Service).
type Playout interface {
	// LastCallback is when Liquidsoap last called back (any event,
	// heartbeats included); nil before the first.
	LastCallback() *time.Time
	// Link is the state of the control link.
	Link() liquidsoap.State
}

// MountChecker checks the Icecast mount (*IcecastChecker).
type MountChecker interface {
	Check(ctx context.Context, t IcecastTarget) error
}

// JournalReader replays the playout journal (*Journal).
type JournalReader interface {
	Replay(ctx context.Context, from, to time.Time) (ReplayResult, error)
}

// Operations runs an operation in the calling goroutine
// (*operations.Tracker).
type Operations interface {
	Do(ctx context.Context, kind operations.Kind, component logging.Component, fn operations.Func) (uint64, error)
}

// Broadcast derives the broadcast state from the detector's signals and
// outages (*broadcast.Tracker). It is told after the detector's own logic,
// so the outage behaviour does not depend on it.
type Broadcast interface {
	// OnMountCheck records an Icecast mount check.
	OnMountCheck(ctx context.Context, ok bool, at time.Time)
	// OnHeartbeat records whether Liquidsoap's callbacks are silent beyond
	// the heartbeat timeout.
	OnHeartbeat(ctx context.Context, silent bool, at time.Time)
	// OutageOpened runs open and stores the state in its transaction.
	OutageOpened(ctx context.Context, open func(ctx context.Context, tx db.Tx) (broadcast.Outage, error)) error
	// OutageClosed runs closeFn and stores the state in its transaction.
	OutageClosed(ctx context.Context, at time.Time, closeFn func(ctx context.Context, tx db.Tx) error) error
}

// Settings are the detection settings, read on every evaluation (§6.3).
type Settings struct {
	// HeartbeatTimeout: callbacks or a control link silent this long are
	// down (outage.heartbeat_timeout_seconds).
	HeartbeatTimeout time.Duration
	// IcecastInterval is the time between mount checks
	// (outage.icecast_check_interval_seconds).
	IcecastInterval time.Duration
	// FailedChecks failed mount checks in a row put Icecast down
	// (outage.failed_checks_to_open).
	FailedChecks int
	// Prepush is how far ahead the pusher feeds Liquidsoap: items that
	// start within it are due (playout.prepush_minutes).
	Prepush time.Duration
	Icecast IcecastTarget
}

// SettingsFunc reads the settings.
type SettingsFunc func(ctx context.Context) (Settings, error)

// Detector watches the broadcast and records outages (§7.1). Run owns all
// of its state; nothing else calls it.
type Detector struct {
	tx       TxRunner
	store    Store
	playout  Playout
	icecast  MountChecker
	journal  JournalReader
	ops      Operations
	pub      Publisher
	bc       Broadcast
	settings SettingsFunc
	log      *slog.Logger // component outage
	now      func() time.Time

	st detectorState
}

// detectorState belongs to the Run goroutine.
type detectorState struct {
	// startedAt stands in for the last callback and the last link success
	// until the first ones arrive.
	startedAt time.Time
	open      *Outage
	// openTold: the broadcast state knows the open outage. One adopted at
	// startup is told at the first evaluation.
	openTold bool
	// lastClosed is the end of the last outage closed by this process; a
	// new one never starts before it.
	lastClosed time.Time

	icecastFails     int
	icecastFirstFail time.Time
	icecastErr       string
	nextIcecast      time.Time

	nextHeartbeat time.Time
	nextSweep     time.Time
	nextRetryMark time.Time
	lastWarn      time.Time
	lastJournal   time.Time
}

// NewDetector returns the detector. log is the root logger.
func NewDetector(tx TxRunner, store Store, p Playout, icecast MountChecker, journal JournalReader, ops Operations,
	pub Publisher, bc Broadcast, settings SettingsFunc, log *slog.Logger, now func() time.Time,
) *Detector {
	return &Detector{
		tx: tx, store: store, playout: p, icecast: icecast, journal: journal, ops: ops, pub: pub, bc: bc, settings: settings,
		log: logging.For(log, logging.ComponentOutage), now: now,
	}
}

// Run reconciles what aired while the backend was down, then evaluates the
// signals every second until ctx ends.
func (d *Detector) Run(ctx context.Context) error {
	d.st.startedAt = d.now().UTC()
	d.startup(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		tctx, cancel := context.WithTimeout(ctx, tickTimeout)
		err := d.evaluate(tctx)
		cancel()
		d.warnFailure(err)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (d *Detector) warnFailure(err error) {
	now := d.now()
	switch {
	case err == nil:
		if !d.st.lastWarn.IsZero() {
			d.log.Info("outage detector recovered")
			d.st.lastWarn = time.Time{}
		}
	case errors.Is(err, context.Canceled):
	case now.Sub(d.st.lastWarn) >= retryLog:
		d.log.Warn("outage detection failed", logging.KeyError, err)
		d.st.lastWarn = now
	}
}

// signal is a failed signal: what it would open an outage with.
type signal struct {
	by     DetectedBy
	comp   Component
	since  time.Time
	reason string
}

// evaluate runs one round: the Icecast check when due, the signals, open
// or close, then the heartbeat and the sweep when due.
func (d *Detector) evaluate(ctx context.Context) error {
	set, err := d.settings(ctx)
	if err != nil {
		return fmt.Errorf("read outage settings: %w", err)
	}
	now := d.now().UTC()
	if !now.Before(d.st.nextIcecast) {
		d.checkIcecast(ctx, set, now)
		d.st.nextIcecast = now.Add(set.IcecastInterval)
	}
	down, err := d.signals(ctx, set, now)
	if err != nil {
		return err
	}
	// Before open and close: a heartbeat back is known when the outage
	// closes, so the state does not pass through unknown.
	d.bc.OnHeartbeat(ctx, now.Sub(d.lastCallback()) > set.HeartbeatTimeout, now)
	if d.st.open != nil && !d.st.openTold {
		o := *d.st.open
		err := d.bc.OutageOpened(ctx, func(context.Context, db.Tx) (broadcast.Outage, error) {
			return broadcastOutage(o), nil
		})
		if err != nil {
			return fmt.Errorf("report open outage %d: %w", o.ID, err)
		}
		d.st.openTold = true
	}
	switch {
	case d.st.open == nil && down != nil:
		if err := d.openOutage(ctx, *down); err != nil {
			return err
		}
	case d.st.open != nil && down == nil:
		if err := d.closeOutage(ctx, now); err != nil {
			return err
		}
	}
	if !now.Before(d.st.nextHeartbeat) {
		if err := d.store.SetHeartbeat(ctx, now); err != nil {
			return fmt.Errorf("record backend heartbeat: %w", err)
		}
		d.st.nextHeartbeat = now.Add(heartbeatEvery)
	}
	if d.st.open == nil && !now.Before(d.st.nextSweep) {
		d.st.nextSweep = now.Add(sweepEvery)
		if err := d.sweep(ctx, now); err != nil {
			return err
		}
	}
	return nil
}

func (d *Detector) checkIcecast(ctx context.Context, set Settings, now time.Time) {
	err := d.icecast.Check(ctx, set.Icecast)
	defer d.bc.OnMountCheck(ctx, err == nil, now)
	if err == nil {
		if d.st.icecastFails >= set.FailedChecks {
			d.log.Info("icecast mount reachable again", "mount", set.Icecast.Mount)
		}
		d.st.icecastFails, d.st.icecastErr = 0, ""
		return
	}
	if d.st.icecastFails == 0 {
		d.st.icecastFirstFail = now
	}
	d.st.icecastFails++
	d.st.icecastErr = err.Error()
}

// signals returns the first failed signal, in order: Liquidsoap stalled,
// control link lost while items are due, Icecast mount missing. Nil when
// all are up.
func (d *Detector) signals(ctx context.Context, set Settings, now time.Time) (*signal, error) {
	lastCB := d.lastCallback()
	if now.Sub(lastCB) > set.HeartbeatTimeout {
		return &signal{
			by: DetectedByLiquidsoapCallback, comp: ComponentLiquidsoap, since: lastCB,
			reason: fmt.Sprintf("no callback from Liquidsoap since %s", lastCB.Format(time.RFC3339)),
		}, nil
	}

	link := d.playout.Link()
	lastOK := d.st.startedAt
	if link.LastOK.After(lastOK) {
		lastOK = link.LastOK
	}
	if now.Sub(lastOK) > set.HeartbeatTimeout {
		// Liquidsoap still streams (its queue, then the emergency source),
		// but planned items cannot be pushed: an outage only while some
		// are due (decision 1).
		due, err := d.store.ItemsDue(ctx, now, now.Add(set.Prepush))
		if err != nil {
			return nil, fmt.Errorf("read due items: %w", err)
		}
		if due {
			return &signal{
				by: DetectedByLiquidsoapControl, comp: ComponentLiquidsoap, since: lastOK,
				reason: fmt.Sprintf("Liquidsoap control link lost since %s; planned items cannot be pushed", lastOK.Format(time.RFC3339)),
			}, nil
		}
	}

	if set.FailedChecks > 0 && d.st.icecastFails >= set.FailedChecks {
		return &signal{
			by: DetectedByIcecast, comp: ComponentIcecast, since: d.st.icecastFirstFail,
			reason: d.st.icecastErr,
		}, nil
	}
	return nil, nil
}

// lastCallback is when Liquidsoap last called back, or the start of the
// detector before the first callback.
func (d *Detector) lastCallback() time.Time {
	last := d.st.startedAt
	if t := d.playout.LastCallback(); t != nil && t.After(last) {
		last = *t
	}
	return last
}

// openOutage records the outage, and the broadcast state in the same
// transaction (docs/phase-16-plan.md, decision 6). An outage opened by an
// earlier run of the backend is adopted.
func (d *Detector) openOutage(ctx context.Context, s signal) error {
	since := s.since
	if since.Before(d.st.lastClosed) {
		since = d.st.lastClosed
	}
	var (
		o       Outage
		adopted bool
	)
	err := d.bc.OutageOpened(ctx, func(ctx context.Context, tx db.Tx) (broadcast.Outage, error) {
		st := d.store.WithTx(tx)
		found, isOpen, err := st.OpenOutage(ctx)
		if err != nil {
			return broadcast.Outage{}, fmt.Errorf("read open outage: %w", err)
		}
		if isOpen {
			o, adopted = found, true
			return broadcastOutage(o), nil
		}
		o, err = st.CreateOutage(ctx, NewOutage{StartedAt: since, DetectedBy: s.by, Component: s.comp, Reason: s.reason}, nil)
		if err != nil {
			return broadcast.Outage{}, fmt.Errorf("open outage: %w", err)
		}
		return broadcastOutage(o), nil
	})
	if err != nil {
		return err
	}
	d.st.open, d.st.openTold = &o, true
	if adopted {
		return nil
	}
	d.log.Warn("broadcast outage opened",
		"outage_id", o.ID, "failed_component", string(o.Component), "detected_by", string(o.DetectedBy),
		"started_at", o.StartedAt, "reason", s.reason)
	d.pub.Publish(ws.OutageStartedEvent{
		Type: ws.EventOutageStarted, Time: d.now().UTC(),
		Data: ws.OutageStartedData{
			OutageID: o.ID, StartedAt: o.StartedAt, Component: string(o.Component),
			DetectedBy: string(o.DetectedBy), Reason: o.Reason,
		},
	})
	return nil
}

// closeOutage ends the open outage, marks its missed items, and reports
// the end with their count.
func (d *Detector) closeOutage(ctx context.Context, now time.Time) error {
	o := *d.st.open
	err := d.bc.OutageClosed(ctx, now, func(ctx context.Context, tx db.Tx) error {
		return d.store.WithTx(tx).CloseOutage(ctx, o.ID, now)
	})
	if err != nil {
		return fmt.Errorf("close outage %d: %w", o.ID, err)
	}
	o.EndedAt = &now
	d.st.open, d.st.openTold, d.st.lastClosed = nil, false, now
	d.log.Info("broadcast outage closed",
		"outage_id", o.ID, "failed_component", string(o.Component), "duration_s", int(now.Sub(o.StartedAt).Seconds()))

	// The marking runs on the service context with its own deadline: the
	// tick's deadline is too short for a journal replay.
	n, err := d.markOp(context.WithoutCancel(ctx), o)
	data := ws.OutageEndedData{OutageID: o.ID, StartedAt: o.StartedAt, EndedAt: now, Component: string(o.Component)}
	if err == nil {
		data.MissedCount = &n
	}
	d.pub.Publish(ws.OutageEndedEvent{Type: ws.EventOutageEnded, Time: d.now().UTC(), Data: data})
	return nil
}

// broadcastOutage is the outage as the broadcast state shows it: its
// reason, or its component when it has none.
func broadcastOutage(o Outage) broadcast.Outage {
	reason := string(o.Component)
	if o.Reason != nil && *o.Reason != "" {
		reason = *o.Reason
	}
	return broadcast.Outage{ID: o.ID, StartedAt: o.StartedAt, Reason: reason}
}

// markOp marks the missed items of a closed outage as a missed_items
// operation. The error is the operation's failure (already logged and
// published by the tracker) or a failure to store it.
func (d *Detector) markOp(ctx context.Context, o Outage) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	var n int
	var markErr error
	_, err := d.ops.Do(ctx, operations.KindMissedItems, logging.ComponentOutage, func(ctx context.Context) (string, error) {
		n, markErr = d.mark(ctx, o)
		return fmt.Sprintf("outage %d: %d items marked missed", o.ID, n), markErr
	})
	return n, errors.Join(markErr, err)
}

// mark replays the journal of the outage window, then moves the items of
// the window that never started to the play log as missed (decision 4).
func (d *Detector) mark(ctx context.Context, o Outage) (int, error) {
	now := d.now().UTC()
	d.replay(ctx, o.StartedAt.Add(-replayMargin), now)
	var n int
	err := d.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := d.store.WithTx(tx)
		if err := st.LockPlan(ctx); err != nil {
			return err
		}
		var err error
		if n, err = st.MarkMissed(ctx, o, now.Add(-lateLimit)); err != nil {
			return err
		}
		return st.SetReconciled(ctx, o.ID, now)
	})
	if err != nil {
		return 0, fmt.Errorf("mark missed items of outage %d: %w", o.ID, err)
	}
	if n > 0 {
		d.log.Warn("plan items did not air and are marked missed", "outage_id", o.ID, "count", n)
	}
	return n, nil
}

// replay replays the journal of a window. The journal only adds starts
// whose callback was lost, so an unreadable journal does not stop the
// marking: it is logged (at most every retryMarkEvery) and the callbacks
// decide.
func (d *Detector) replay(ctx context.Context, from, to time.Time) {
	res, err := d.journal.Replay(ctx, from, to)
	if err == nil {
		if res.Lines > 0 {
			d.log.Debug("playout journal replayed", "from", from, "to", to, "lines", res.Lines)
		}
		return
	}
	now := d.now()
	if now.Sub(d.st.lastJournal) >= retryMarkEvery {
		d.log.Error("playout journal could not be replayed; using the callbacks only", logging.KeyError, err)
		d.st.lastJournal = now
	}
}

// sweep retries the marking of closed outages that are not marked, then
// drops the items that did not air while the stream was fine (decision 5).
func (d *Detector) sweep(ctx context.Context, now time.Time) error {
	if !now.Before(d.st.nextRetryMark) {
		d.st.nextRetryMark = now.Add(retryMarkEvery)
		unmarked, err := d.store.Unmarked(ctx)
		if err != nil {
			return fmt.Errorf("read unmarked outages: %w", err)
		}
		for _, o := range unmarked {
			// Failures are reported by the operation; the next retry runs
			// after retryMarkEvery.
			_, _ = d.markOp(context.WithoutCancel(ctx), o)
		}
	}

	cut := now.Add(-staleAfter)
	oldest, found, err := d.store.OldestUnaired(ctx, cut)
	if err != nil {
		return fmt.Errorf("read unaired items: %w", err)
	}
	if !found {
		return nil
	}
	d.replay(ctx, oldest.Add(-replayMargin), now)
	var n int
	err = d.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := d.store.WithTx(tx)
		if lockErr := st.LockPlan(ctx); lockErr != nil {
			return lockErr
		}
		var dropErr error
		n, dropErr = st.DropStale(ctx, cut)
		return dropErr
	})
	if err != nil {
		return fmt.Errorf("drop unaired items: %w", err)
	}
	if n > 0 {
		d.log.Warn("plan items did not air without an outage and are dropped", "count", n)
	}
	return nil
}

// startup runs the playout_reconcile operation: after the backend was down,
// the journal tells what aired; the downtime is recorded as an outage when
// planned items did not air, and closed outages not marked yet are marked
// (decision 7). Failures are reported by the operation; detection starts
// anyway.
func (d *Detector) startup(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	_, err := d.ops.Do(ctx, operations.KindPlayoutReconcile, logging.ComponentReconcile, d.reconcile)
	if err != nil {
		d.log.Warn("store playout reconcile operation failed", logging.KeyError, err)
	}
}

func (d *Detector) reconcile(ctx context.Context) (string, error) {
	set, err := d.settings(ctx)
	if err != nil {
		return "", fmt.Errorf("read outage settings: %w", err)
	}
	now := d.now().UTC()
	alive, hasAlive, err := d.store.Heartbeat(ctx)
	if err != nil {
		return "", fmt.Errorf("read backend heartbeat: %w", err)
	}
	oldest, hasOldest, err := d.store.OldestUnaired(ctx, now)
	if err != nil {
		return "", fmt.Errorf("read unaired items: %w", err)
	}
	from := now
	if hasAlive && alive.Before(from) {
		from = alive
	}
	if hasOldest && oldest.Before(from) {
		from = oldest
	}
	d.replay(ctx, from.Add(-replayMargin), now)

	open, isOpen, err := d.store.OpenOutage(ctx)
	if err != nil {
		return "", fmt.Errorf("read open outage: %w", err)
	}
	if isOpen {
		// The backend stopped during it; detection closes it.
		d.st.open = &open
	}
	created := false
	if hasAlive && !isOpen && now.Sub(alive) > set.HeartbeatTimeout {
		n, countErr := d.store.CountUnaired(ctx, alive, now.Add(-lateLimit))
		if countErr != nil {
			return "", fmt.Errorf("count unaired items: %w", countErr)
		}
		if n > 0 {
			o, createErr := d.store.CreateOutage(ctx, NewOutage{
				StartedAt: alive, DetectedBy: DetectedByBackend, Component: ComponentBackend, Reason: backendReason,
			}, &now)
			if createErr != nil {
				return "", fmt.Errorf("record backend outage: %w", createErr)
			}
			created = true
			d.st.lastClosed = now
			d.log.Warn("backend downtime recorded as an outage",
				"outage_id", o.ID, "started_at", o.StartedAt, "duration_s", int(now.Sub(alive).Seconds()))
		}
	}

	unmarked, err := d.store.Unmarked(ctx)
	if err != nil {
		return "", fmt.Errorf("read unmarked outages: %w", err)
	}
	missed := 0
	var errs []error
	for _, o := range unmarked {
		n, err := d.mark(ctx, o)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		missed += n
	}
	d.st.nextRetryMark = now.Add(retryMarkEvery)
	summary := fmt.Sprintf("%d outages marked, %d items missed", len(unmarked)-len(errs), missed)
	if created {
		summary = "backend downtime recorded as an outage; " + summary
	}
	return summary, errors.Join(errs...)
}
