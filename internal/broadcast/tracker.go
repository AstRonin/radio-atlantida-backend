package broadcast

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

const (
	// dbTimeout bounds the read or the write of the state outside an
	// outage transaction.
	dbTimeout = 5 * time.Second
	// retryLog bounds the WARN logs of a write that fails on every event;
	// the next event retries it.
	retryLog = time.Minute
)

// Tracker derives the broadcast state (decisions 2 to 5) and stores it
// when it changes (decision 6). It has no goroutine: the playout callbacks
// and the outage detector call it from their own goroutines, so mu guards
// all of its state. A transition is stored before it is published; a
// failed write leaves the state as it was, and the next call retries it
// (the detector calls OnHeartbeat every second).
type Tracker struct {
	tx    TxRunner
	store Store
	pub   Publisher
	log   *slog.Logger // component icecast
	now   func() time.Time

	mu sync.Mutex
	st trackerState
}

// trackerState is copied whole to roll a failed outage transaction back.
type trackerState struct {
	// Inputs.
	output    *bool // Liquidsoap's last output report; nil: none since the start or since its heartbeat went silent
	mountOK   *bool
	lastCheck *time.Time
	silent    bool
	outage    *Outage

	// loaded is set once stored was read; stored is nil when no row exists.
	loaded bool
	stored *Record

	// What the panel was told.
	state  State
	since  time.Time
	reason *string

	lastWarn time.Time
}

// NewTracker returns the tracker in state unknown. log is the root logger.
func NewTracker(tx TxRunner, store Store, pub Publisher, log *slog.Logger, now func() time.Time) *Tracker {
	return &Tracker{
		tx: tx, store: store, pub: pub, log: logging.For(log, logging.ComponentIcecast), now: now,
		st: trackerState{state: StateUnknown},
	}
}

// Status returns the current state.
func (t *Tracker) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.st.status()
}

// OnOutput records Liquidsoap's report of its Icecast output, made at at
// (the playout callback).
func (t *Tracker) OnOutput(ctx context.Context, connected bool, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.st.output = &connected
	t.update(ctx, at.UTC())
}

// OnMountCheck records the result of an Icecast mount check made at at
// (the outage detector). It decides the state only while Liquidsoap has
// not reported its output since the start: Liquidsoap reports the output
// only when it changes, so after a restart of the backend the check is the
// first evidence (decision 8).
func (t *Tracker) OnMountCheck(ctx context.Context, ok bool, at time.Time) {
	at = at.UTC()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.st.mountOK, t.st.lastCheck = &ok, &at
	t.update(ctx, at)
}

// OnHeartbeat records whether Liquidsoap's heartbeat is silent beyond the
// timeout, evaluated at at (the outage detector, every second). While it
// is silent the state is unknown, and the last output report is forgotten:
// a restarted Liquidsoap reports its output again.
func (t *Tracker) OnHeartbeat(ctx context.Context, silent bool, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.st.silent = silent
	if silent {
		t.st.output = nil
	}
	t.update(ctx, at.UTC())
}

// OutageOpened runs open, which records or adopts the open outage, and
// stores the resulting state in the same transaction (decision 6). The
// error is open's, or the failed write; the state is then unchanged.
func (t *Tracker) OutageOpened(ctx context.Context, open func(ctx context.Context, tx db.Tx) (Outage, error)) error {
	return t.inOutageTx(ctx, func(ctx context.Context, tx db.Tx) (time.Time, error) {
		o, err := open(ctx, tx)
		if err != nil {
			return time.Time{}, err
		}
		o.StartedAt = o.StartedAt.UTC()
		t.st.outage = &o
		return t.now().UTC(), nil
	})
}

// OutageClosed runs close, which ends the open outage at at, and stores
// the resulting state in the same transaction.
func (t *Tracker) OutageClosed(ctx context.Context, at time.Time, closeFn func(ctx context.Context, tx db.Tx) error) error {
	return t.inOutageTx(ctx, func(ctx context.Context, tx db.Tx) (time.Time, error) {
		if err := closeFn(ctx, tx); err != nil {
			return time.Time{}, err
		}
		t.st.outage = nil
		return at.UTC(), nil
	})
}

// inOutageTx runs change and the write of the state in one transaction.
// change sets the outage input and returns the time of the transition.
func (t *Tracker) inOutageTx(ctx context.Context, change func(ctx context.Context, tx db.Tx) (time.Time, error)) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	saved := t.st
	var changed bool
	err := t.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		at, err := change(ctx, tx)
		if err != nil {
			return err
		}
		changed, err = t.apply(ctx, t.store.WithTx(tx), at)
		return err
	})
	if err != nil {
		t.st = saved
		return err
	}
	if changed {
		t.publish()
	}
	return nil
}

// update applies the inputs outside a transaction. A failed write is
// logged and retried at the next call.
func (t *Tracker) update(ctx context.Context, at time.Time) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	changed, err := t.apply(ctx, t.store, at)
	if err != nil {
		if now := t.now(); now.Sub(t.st.lastWarn) >= retryLog {
			t.log.Warn("broadcast state not stored; retried at the next event", logging.KeyError, err)
			t.st.lastWarn = now
		}
		return
	}
	if changed {
		t.publish()
	}
}

// derive returns the state the inputs give: an open outage first, then a
// silent heartbeat, then the output report, then the last mount check.
func (s *trackerState) derive() (State, *string) {
	switch {
	case s.outage != nil:
		return StateUnreachable, new(s.outage.Reason)
	case s.silent:
		return StateUnknown, nil
	case s.output != nil:
		return connectedOr(*s.output), nil
	case s.mountOK != nil:
		return connectedOr(*s.mountOK), nil
	}
	return StateUnknown, nil
}

func connectedOr(ok bool) State {
	if ok {
		return StateConnected
	}
	return StateReconnecting
}

// apply moves to the derived state, storing it first; at is the time of
// the transition. It changes the state only when the write succeeded and
// reports whether the state changed.
func (t *Tracker) apply(ctx context.Context, store Store, at time.Time) (bool, error) {
	s := &t.st
	next, reason := s.derive()
	if next == s.state {
		return false, nil
	}
	if next == StateUnknown {
		s.state, s.since, s.reason = StateUnknown, time.Time{}, nil
		t.log.Info("broadcast state unknown: no heartbeat from Liquidsoap")
		return true, nil
	}
	if !s.loaded {
		rec, found, err := store.Get(ctx)
		if err != nil {
			return false, fmt.Errorf("read broadcast state: %w", err)
		}
		s.loaded = true
		if found {
			s.stored = &rec
		}
	}

	// The last known change: what the panel was told, or what was stored
	// before the start.
	var prev time.Time
	switch {
	case s.state != StateUnknown:
		prev = s.since
	case s.stored != nil:
		prev = s.stored.Since
	}

	if s.state == StateUnknown && s.stored != nil && s.stored.State == next {
		// The first evidence confirms the stored state: its timer goes on
		// (decision 8).
		s.state, s.since, s.reason = next, s.stored.Since, reason
		t.log.Info("broadcast state confirmed", "state", string(next), "since", s.since)
		return true, nil
	}

	since := at
	if next == StateUnreachable && !s.outage.StartedAt.Before(prev) && !s.outage.StartedAt.After(at) {
		// The detector dates an outage from its first failed signal (the
		// first failed mount check), which is when the stream went down.
		since = s.outage.StartedAt
	}
	rec := Record{State: next, Since: since, Reason: reason, UpdatedAt: t.now().UTC()}
	if err := store.Save(ctx, rec); err != nil {
		return false, fmt.Errorf("store broadcast state %s: %w", next, err)
	}
	s.stored = &rec
	s.state, s.since, s.reason = next, since, reason
	t.log.Info("broadcast state changed", "state", string(next), "since", since)
	return true, nil
}

func (s *trackerState) status() Status {
	st := Status{State: s.state, Reason: s.reason}
	if s.state != StateUnknown {
		st.Since = new(s.since)
	}
	if s.state == StateUnreachable && s.outage != nil {
		st.OutageID = new(s.outage.ID)
	}
	if s.mountOK != nil {
		st.MountOK = new(*s.mountOK)
	}
	if s.lastCheck != nil {
		st.LastCheckAt = new(*s.lastCheck)
	}
	return st
}

func (t *Tracker) publish() {
	t.pub.Publish(ws.BroadcastStateEvent{Type: ws.EventBroadcastState, Time: t.now().UTC(), Data: stateData(t.st.status())})
}

// stateData maps a status to the payload of broadcast.state.
func stateData(s Status) ws.BroadcastStateData {
	return ws.BroadcastStateData{
		State: string(s.State), Since: s.Since, Reason: s.Reason, OutageID: s.OutageID,
		MountOK: s.MountOK, LastCheckAt: s.LastCheckAt,
	}
}
