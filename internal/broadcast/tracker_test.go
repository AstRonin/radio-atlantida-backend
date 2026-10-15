package broadcast

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

var t0 = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

// at is t0 plus s seconds.
func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

type fakeStore struct {
	rec    *Record
	saves  []Record
	gets   int
	errGet error
	errSav error
}

func (f *fakeStore) WithTx(db.Tx) Store { return f }

func (f *fakeStore) Get(context.Context) (Record, bool, error) {
	f.gets++
	if f.errGet != nil {
		return Record{}, false, f.errGet
	}
	if f.rec == nil {
		return Record{}, false, nil
	}
	return *f.rec, true, nil
}

func (f *fakeStore) Save(_ context.Context, r Record) error {
	if f.errSav != nil {
		return f.errSav
	}
	f.rec = &r
	f.saves = append(f.saves, r)
	return nil
}

type txRunner struct{}

func (txRunner) WithTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	return fn(ctx, db.Tx{})
}

type publisher struct {
	mu     sync.Mutex
	events []ws.BroadcastStateEvent
}

func (p *publisher) Publish(e any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e.(ws.BroadcastStateEvent))
}

type harness struct {
	tr    *Tracker
	store *fakeStore
	pub   *publisher
	now   time.Time
}

func newHarness(stored *Record) *harness {
	h := &harness{store: &fakeStore{rec: stored}, pub: &publisher{}, now: t0}
	h.tr = NewTracker(txRunner{}, h.store, h.pub, slog.New(slog.DiscardHandler), func() time.Time { return h.now })
	return h
}

// step is one input; it moves the clock to its time first.
type step func(t *testing.T, h *harness)

func output(s int, connected bool) step {
	return func(t *testing.T, h *harness) {
		h.now = at(s)
		h.tr.OnOutput(t.Context(), connected, at(s))
	}
}

func check(s int, ok bool) step {
	return func(t *testing.T, h *harness) {
		h.now = at(s)
		h.tr.OnMountCheck(t.Context(), ok, at(s))
	}
}

func heartbeat(s int, silent bool) step {
	return func(t *testing.T, h *harness) {
		h.now = at(s)
		h.tr.OnHeartbeat(t.Context(), silent, at(s))
	}
}

func opened(s int, o Outage) step {
	return func(t *testing.T, h *harness) {
		h.now = at(s)
		require.NoError(t, h.tr.OutageOpened(t.Context(), func(context.Context, db.Tx) (Outage, error) { return o, nil }))
	}
}

func closed(s int) step {
	return func(t *testing.T, h *harness) {
		h.now = at(s)
		require.NoError(t, h.tr.OutageClosed(t.Context(), at(s), func(context.Context, db.Tx) error { return nil }))
	}
}

func TestTransitions(t *testing.T) {
	t.Parallel()
	icecastDown := Outage{ID: 3, StartedAt: at(2), Reason: "mount not found"}
	yesterday := t0.Add(-24 * time.Hour)
	tests := []struct {
		name   string
		stored *Record
		steps  []step
		state  State
		since  *time.Time
		// events are the states published, in order.
		events []State
		// saves are the states written, in order.
		saves []State
	}{
		{
			name:  "unknown until the first evidence",
			steps: []step{heartbeat(1, false), heartbeat(2, false)},
			state: StateUnknown,
		},
		{
			name:  "output connected",
			steps: []step{output(1, true)},
			state: StateConnected, since: new(at(1)),
			events: []State{StateConnected}, saves: []State{StateConnected},
		},
		{
			name:  "output lost without an outage",
			steps: []step{output(1, true), output(5, false)},
			state: StateReconnecting, since: new(at(5)),
			events: []State{StateConnected, StateReconnecting}, saves: []State{StateConnected, StateReconnecting},
		},
		{
			name: "output lost with an open outage stays unreachable",
			steps: []step{
				output(1, true), opened(4, Outage{ID: 1, StartedAt: at(3), Reason: "icecast"}), output(5, false),
			},
			state: StateUnreachable, since: new(at(3)),
			events: []State{StateConnected, StateUnreachable}, saves: []State{StateConnected, StateUnreachable},
		},
		{
			name:  "outage opened: since its first failed check",
			steps: []step{output(0, false), opened(10, icecastDown)},
			state: StateUnreachable, since: new(at(2)),
			events: []State{StateReconnecting, StateUnreachable}, saves: []State{StateReconnecting, StateUnreachable},
		},
		{
			name: "outage dated before the last change: since the change",
			steps: []step{
				output(0, true), output(5, false), opened(10, icecastDown),
			},
			state: StateUnreachable, since: new(at(10)),
			events: []State{StateConnected, StateReconnecting, StateUnreachable},
			saves:  []State{StateConnected, StateReconnecting, StateUnreachable},
		},
		{
			name: "outage closed with the output back",
			steps: []step{
				output(0, false), opened(10, icecastDown), output(15, true), closed(20),
			},
			state: StateConnected, since: new(at(20)),
			events: []State{StateReconnecting, StateUnreachable, StateConnected},
			saves:  []State{StateReconnecting, StateUnreachable, StateConnected},
		},
		{
			name: "outage closed with the output still lost",
			steps: []step{
				output(0, false), opened(10, icecastDown), closed(20),
			},
			state: StateReconnecting, since: new(at(20)),
			events: []State{StateReconnecting, StateUnreachable, StateReconnecting},
			saves:  []State{StateReconnecting, StateUnreachable, StateReconnecting},
		},
		{
			name:   "restart with the source present keeps the stored timer",
			stored: &Record{State: StateConnected, Since: yesterday},
			steps:  []step{heartbeat(1, false), check(2, true), check(7, true)},
			state:  StateConnected, since: &yesterday,
			events: []State{StateConnected},
		},
		{
			name:   "mount check contradicting the stored state",
			stored: &Record{State: StateUnreachable, Since: yesterday, Reason: new("icecast")},
			steps:  []step{check(2, true)},
			state:  StateConnected, since: new(at(2)),
			events: []State{StateConnected}, saves: []State{StateConnected},
		},
		{
			name:   "restart with the stream down: since the first failed check",
			stored: &Record{State: StateConnected, Since: yesterday},
			steps:  []step{check(2, false), opened(7, icecastDown)},
			state:  StateUnreachable, since: new(at(2)),
			events: []State{StateReconnecting, StateUnreachable}, saves: []State{StateReconnecting, StateUnreachable},
		},
		{
			name:   "restart during an outage keeps the stored timer",
			stored: &Record{State: StateUnreachable, Since: yesterday, Reason: new("mount not found")},
			steps:  []step{opened(1, Outage{ID: 3, StartedAt: yesterday.Add(-time.Minute), Reason: "mount not found"})},
			state:  StateUnreachable, since: &yesterday,
			events: []State{StateUnreachable},
		},
		{
			name:  "the output report wins over a failed mount check",
			steps: []step{output(0, true), check(2, false), check(7, true)},
			state: StateConnected, since: new(at(0)),
			events: []State{StateConnected}, saves: []State{StateConnected},
		},
		{
			name:   "heartbeat silence: unknown, not stored",
			steps:  []step{output(0, true), heartbeat(20, true)},
			state:  StateUnknown,
			events: []State{StateConnected, StateUnknown}, saves: []State{StateConnected},
		},
		{
			name:  "heartbeat back: the output report was forgotten, the mount check decides",
			steps: []step{output(0, false), heartbeat(20, true), heartbeat(25, false), check(26, true)},
			state: StateConnected, since: new(at(26)),
			events: []State{StateReconnecting, StateUnknown, StateConnected},
			saves:  []State{StateReconnecting, StateConnected},
		},
		{
			name:  "heartbeat back with the same state keeps the timer",
			steps: []step{output(0, true), heartbeat(20, true), heartbeat(25, false), output(26, true)},
			state: StateConnected, since: new(at(0)),
			events: []State{StateConnected, StateUnknown, StateConnected},
			saves:  []State{StateConnected},
		},
		{
			name:  "a silent heartbeat during an outage stays unreachable",
			steps: []step{opened(5, icecastDown), heartbeat(20, true)},
			state: StateUnreachable, since: new(at(2)),
			events: []State{StateUnreachable}, saves: []State{StateUnreachable},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(tt.stored)
			for _, s := range tt.steps {
				s(t, h)
			}
			st := h.tr.Status()
			require.Equal(t, tt.state, st.State)
			require.Equal(t, tt.since, st.Since)

			var events []State
			for _, e := range h.pub.events {
				events = append(events, State(e.Data.State))
			}
			require.Equal(t, tt.events, events)
			var saves []State
			for _, r := range h.store.saves {
				saves = append(saves, r.State)
			}
			require.Equal(t, tt.saves, saves)
			if len(h.store.saves) > 0 {
				last := h.store.saves[len(h.store.saves)-1]
				if tt.since != nil && last.State == tt.state {
					require.Equal(t, *tt.since, last.Since, "the stored since is the shown one")
				}
			}
			require.LessOrEqual(t, h.store.gets, 1, "the stored state is read once")
		})
	}
}

func TestStatusAndEventPayload(t *testing.T) {
	t.Parallel()
	h := newHarness(nil)
	check(1, false)(t, h)
	opened(6, Outage{ID: 9, StartedAt: at(1), Reason: "mount not found"})(t, h)

	st := h.tr.Status()
	require.Equal(t, Status{
		State: StateUnreachable, Since: new(at(1)), Reason: new("mount not found"), OutageID: new(uint64(9)),
		MountOK: new(false), LastCheckAt: new(at(1)),
	}, st)
	e := h.pub.events[len(h.pub.events)-1]
	require.Equal(t, ws.EventBroadcastState, e.Type)
	require.Equal(t, at(6), e.Time)
	require.Equal(t, ws.BroadcastStateData{
		State: "unreachable", Since: new(at(1)), Reason: new("mount not found"), OutageID: new(uint64(9)),
		MountOK: new(false), LastCheckAt: new(at(1)),
	}, e.Data)
	require.Equal(t, new("mount not found"), h.store.rec.Reason)
	require.Equal(t, at(6), h.store.rec.UpdatedAt)

	closed(20)(t, h)
	st = h.tr.Status()
	require.Nil(t, st.OutageID)
	require.Nil(t, st.Reason)
	require.Nil(t, h.store.rec.Reason)
}

func TestFailedWriteIsRetriedAtTheNextEvent(t *testing.T) {
	t.Parallel()
	h := newHarness(nil)
	h.store.errSav = errors.New("database down")
	output(1, true)(t, h)
	require.Equal(t, StateUnknown, h.tr.Status().State, "not shown before it is stored")
	require.Empty(t, h.pub.events)

	h.store.errSav = nil
	heartbeat(2, false)(t, h)
	st := h.tr.Status()
	require.Equal(t, StateConnected, st.State)
	require.Equal(t, at(2), *st.Since, "since the write that succeeded")
	require.Len(t, h.pub.events, 1)
}

func TestFailedReadIsRetried(t *testing.T) {
	t.Parallel()
	h := newHarness(&Record{State: StateConnected, Since: t0.Add(-time.Hour)})
	h.store.errGet = errors.New("database down")
	check(1, true)(t, h)
	require.Equal(t, StateUnknown, h.tr.Status().State)

	h.store.errGet = nil
	check(6, true)(t, h)
	require.Equal(t, t0.Add(-time.Hour), *h.tr.Status().Since)
}

func TestFailedOutageTransactionLeavesTheStateUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(nil)
	output(0, false)(t, h)

	errOpen := errors.New("unique violation")
	err := h.tr.OutageOpened(t.Context(), func(context.Context, db.Tx) (Outage, error) { return Outage{}, errOpen })
	require.ErrorIs(t, err, errOpen)
	h.store.errSav = errors.New("database down")
	err = h.tr.OutageOpened(t.Context(), func(context.Context, db.Tx) (Outage, error) {
		return Outage{ID: 1, StartedAt: at(0)}, nil
	})
	require.ErrorIs(t, err, h.store.errSav)

	h.store.errSav = nil
	heartbeat(5, false)(t, h)
	st := h.tr.Status()
	require.Equal(t, StateReconnecting, st.State, "the outage of the failed transaction is forgotten")
	require.Len(t, h.pub.events, 1)

	errClose := errors.New("close failed")
	opened(6, Outage{ID: 2, StartedAt: at(6), Reason: "icecast"})(t, h)
	err = h.tr.OutageClosed(t.Context(), at(9), func(context.Context, db.Tx) error { return errClose })
	require.ErrorIs(t, err, errClose)
	require.Equal(t, StateUnreachable, h.tr.Status().State, "still open")
}
