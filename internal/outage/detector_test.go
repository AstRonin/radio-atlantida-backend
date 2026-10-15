package outage

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/broadcast"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

var t0 = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

// fakeStore keeps outages in memory; the plan is reduced to what the
// detector asks.
type fakeStore struct {
	outages   []Outage
	heartbeat *time.Time
	// oldest is the start of the oldest unaired item; nil: none.
	oldest   *time.Time
	unaired  int
	due      bool
	missed   int
	marked   []uint64
	markedTo []time.Time
	dropped  []time.Time
	beats    []time.Time
}

func (f *fakeStore) WithTx(db.Tx) Store             { return f }
func (f *fakeStore) LockPlan(context.Context) error { return nil }

func (f *fakeStore) OpenOutage(context.Context) (Outage, bool, error) {
	for _, o := range f.outages {
		if o.EndedAt == nil {
			return o, true, nil
		}
	}
	return Outage{}, false, nil
}

func (f *fakeStore) CreateOutage(ctx context.Context, n NewOutage, endedAt *time.Time) (Outage, error) {
	if _, open, _ := f.OpenOutage(ctx); open && endedAt == nil {
		return Outage{}, ErrOutageOpen
	}
	o := Outage{
		ID: uint64(len(f.outages) + 1), StartedAt: n.StartedAt, EndedAt: endedAt,
		DetectedBy: n.DetectedBy, Component: n.Component, Reason: &n.Reason,
	}
	f.outages = append(f.outages, o)
	return o, nil
}

func (f *fakeStore) CloseOutage(_ context.Context, id uint64, at time.Time) error {
	f.outages[id-1].EndedAt = &at
	return nil
}

func (f *fakeStore) Unmarked(context.Context) ([]Outage, error) {
	var out []Outage
	for _, o := range f.outages {
		if o.EndedAt != nil && o.ReconciledAt == nil {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeStore) MarkMissed(_ context.Context, o Outage, before time.Time) (int, error) {
	f.marked, f.markedTo = append(f.marked, o.ID), append(f.markedTo, before)
	return f.missed, nil
}

func (f *fakeStore) SetReconciled(_ context.Context, id uint64, at time.Time) error {
	f.outages[id-1].ReconciledAt = &at
	return nil
}

func (f *fakeStore) OldestUnaired(_ context.Context, before time.Time) (time.Time, bool, error) {
	if f.oldest == nil || !f.oldest.Before(before) {
		return time.Time{}, false, nil
	}
	return *f.oldest, true, nil
}

func (f *fakeStore) CountUnaired(context.Context, time.Time, time.Time) (int, error) {
	return f.unaired, nil
}

func (f *fakeStore) DropStale(_ context.Context, before time.Time) (int, error) {
	f.dropped = append(f.dropped, before)
	return 2, nil
}

func (f *fakeStore) ItemsDue(context.Context, time.Time, time.Time) (bool, error) { return f.due, nil }

func (f *fakeStore) Heartbeat(context.Context) (time.Time, bool, error) {
	if f.heartbeat == nil {
		return time.Time{}, false, nil
	}
	return *f.heartbeat, true, nil
}

func (f *fakeStore) SetHeartbeat(_ context.Context, at time.Time) error {
	f.beats = append(f.beats, at)
	return nil
}

func (f *fakeStore) History(context.Context, HistoryFilter) ([]Entry, error) { return nil, nil }

func (f *fakeStore) Missed(context.Context, Resolution, int) ([]Entry, error) { return nil, nil }

func (f *fakeStore) Dismiss(context.Context, uint64, time.Time) (Entry, error) { return Entry{}, nil }

func (f *fakeStore) Outages(context.Context, time.Time, time.Time) ([]Outage, error) { return nil, nil }

type txRunner struct{}

func (txRunner) WithTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	return fn(ctx, db.Tx{})
}

type fakePlayout struct {
	lastCB *time.Time
	link   liquidsoap.State
}

func (p *fakePlayout) LastCallback() *time.Time { return p.lastCB }
func (p *fakePlayout) Link() liquidsoap.State   { return p.link }

type fakeIcecast struct{ err error }

func (c *fakeIcecast) Check(context.Context, IcecastTarget) error { return c.err }

type fakeJournal struct {
	err     error
	windows [][2]time.Time
}

func (j *fakeJournal) Replay(_ context.Context, from, to time.Time) (ReplayResult, error) {
	j.windows = append(j.windows, [2]time.Time{from, to})
	return ReplayResult{}, j.err
}

type fakeOps struct {
	kinds []operations.Kind
	errs  []error
}

func (o *fakeOps) Do(ctx context.Context, kind operations.Kind, _ logging.Component, fn operations.Func) (uint64, error) {
	_, err := fn(ctx)
	o.kinds, o.errs = append(o.kinds, kind), append(o.errs, err)
	return uint64(len(o.kinds)), nil
}

type publisher struct {
	mu     sync.Mutex
	events []any
}

func (p *publisher) Publish(e any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}

// fakeBroadcast records what the detector tells the broadcast state; err
// fails its transactions.
type fakeBroadcast struct {
	checks []bool
	silent []bool
	opened []broadcast.Outage
	closed []time.Time
	err    error
}

func (b *fakeBroadcast) OnMountCheck(_ context.Context, ok bool, _ time.Time) {
	b.checks = append(b.checks, ok)
}

func (b *fakeBroadcast) OnHeartbeat(_ context.Context, silent bool, _ time.Time) {
	b.silent = append(b.silent, silent)
}

func (b *fakeBroadcast) OutageOpened(ctx context.Context, open func(context.Context, db.Tx) (broadcast.Outage, error)) error {
	if b.err != nil {
		return b.err
	}
	o, err := open(ctx, db.Tx{})
	if err != nil {
		return err
	}
	b.opened = append(b.opened, o)
	return nil
}

func (b *fakeBroadcast) OutageClosed(ctx context.Context, at time.Time, closeFn func(context.Context, db.Tx) error) error {
	if b.err != nil {
		return b.err
	}
	if err := closeFn(ctx, db.Tx{}); err != nil {
		return err
	}
	b.closed = append(b.closed, at)
	return nil
}

type harness struct {
	d       *Detector
	store   *fakeStore
	playout *fakePlayout
	icecast *fakeIcecast
	journal *fakeJournal
	ops     *fakeOps
	pub     *publisher
	bc      *fakeBroadcast
	now     time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		store: &fakeStore{}, icecast: &fakeIcecast{}, journal: &fakeJournal{}, ops: &fakeOps{}, pub: &publisher{},
		bc: &fakeBroadcast{}, now: t0,
	}
	h.playout = &fakePlayout{lastCB: new(t0), link: liquidsoap.State{Connected: true, LastOK: t0}}
	set := func(context.Context) (Settings, error) {
		return Settings{
			HeartbeatTimeout: 10 * time.Second, IcecastInterval: 5 * time.Second, FailedChecks: 2,
			Prepush: 5 * time.Minute, Icecast: IcecastTarget{Host: "icecast", Port: 8000, Mount: "/radio"},
		}, nil
	}
	h.d = NewDetector(txRunner{}, h.store, h.playout, h.icecast, h.journal, h.ops, h.pub, h.bc, set, slog.New(slog.DiscardHandler),
		func() time.Time { return h.now })
	h.d.st.startedAt = t0
	return h
}

// step moves the clock and runs one evaluation; callbacks and the link stay
// fresh unless the test stops them.
func (h *harness) step(t *testing.T, d time.Duration) {
	t.Helper()
	h.now = h.now.Add(d)
	require.NoError(t, h.d.evaluate(t.Context()))
}

func (h *harness) alive() {
	h.playout.lastCB = new(h.now)
	h.playout.link.LastOK = h.now
}

func TestIcecastOutageOpensAfterFailedChecksAndCloses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.store.missed = 3
	h.icecast.err = errors.New("mount not found")

	h.step(t, 0) // first failed check
	require.Empty(t, h.store.outages)
	h.alive()
	h.step(t, 5*time.Second) // second
	require.Len(t, h.store.outages, 1)
	o := h.store.outages[0]
	require.Equal(t, t0, o.StartedAt, "started at the first failed check")
	require.Equal(t, ComponentIcecast, o.Component)
	require.Equal(t, DetectedByIcecast, o.DetectedBy)
	started := h.pub.events[0].(ws.OutageStartedEvent)
	require.Equal(t, o.ID, started.Data.OutageID)

	h.icecast.err = nil
	h.alive()
	h.step(t, 5*time.Second)
	require.NotNil(t, h.store.outages[0].EndedAt)
	require.Equal(t, []uint64{1}, h.store.marked)
	require.Equal(t, h.now.Add(-lateLimit), h.store.markedTo[0])
	require.NotNil(t, h.store.outages[0].ReconciledAt)
	require.Equal(t, []operations.Kind{operations.KindMissedItems}, h.ops.kinds)
	ended := h.pub.events[1].(ws.OutageEndedEvent)
	require.Equal(t, 3, *ended.Data.MissedCount)
	// The broadcast state saw every check, the outage, and its end.
	require.Equal(t, []bool{false, false, true}, h.bc.checks)
	require.Equal(t, []broadcast.Outage{{ID: 1, StartedAt: t0, Reason: "mount not found"}}, h.bc.opened)
	require.Equal(t, []time.Time{h.now}, h.bc.closed)
	require.Equal(t, []bool{false, false, false}, h.bc.silent)
	// The journal of the window was replayed before marking.
	require.Equal(t, t0.Add(-replayMargin), h.journal.windows[0][0])
}

func TestCallbacksStalledOpensLiquidsoapOutage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	last := t0
	h.playout.lastCB = &last
	h.icecast.err = errors.New("down too") // callbacks win over Icecast

	h.step(t, 10*time.Second)
	require.Empty(t, h.store.outages, "not past the timeout yet")
	h.step(t, time.Second)
	require.Len(t, h.store.outages, 1)
	require.Equal(t, []bool{false, true}, h.bc.silent, "silent past the timeout")
	require.Equal(t, DetectedByLiquidsoapCallback, h.store.outages[0].DetectedBy)
	require.Equal(t, ComponentLiquidsoap, h.store.outages[0].Component)
	require.Equal(t, last, h.store.outages[0].StartedAt)

	// Callbacks back, but Icecast down by now: the outage stays open.
	h.d.st.icecastFails = 2
	h.alive()
	h.step(t, time.Second)
	require.Nil(t, h.store.outages[0].EndedAt)
	h.icecast.err = nil
	h.d.st.nextIcecast = time.Time{}
	h.alive()
	h.step(t, time.Second)
	require.NotNil(t, h.store.outages[0].EndedAt)
}

func TestControlLinkOutageOnlyWhenItemsAreDue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.playout.link = liquidsoap.State{Connected: false, LastOK: t0}

	h.playout.lastCB = new(t0.Add(20 * time.Second))
	h.step(t, 20*time.Second)
	require.Empty(t, h.store.outages, "nothing due: the stream is not missing planned content")

	h.store.due = true
	h.playout.lastCB = new(h.now.Add(time.Second))
	h.step(t, time.Second)
	require.Len(t, h.store.outages, 1)
	require.Equal(t, DetectedByLiquidsoapControl, h.store.outages[0].DetectedBy)
	require.Equal(t, t0, h.store.outages[0].StartedAt)
}

func TestOpenOutageOfAnEarlierRunIsAdopted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.store.outages = []Outage{{ID: 1, StartedAt: t0.Add(-time.Hour), Component: ComponentIcecast}}
	h.icecast.err = errors.New("down")

	h.step(t, 0)
	h.alive()
	h.step(t, 5*time.Second)
	require.Len(t, h.store.outages, 1)
	require.NotNil(t, h.d.st.open)
	require.Equal(t, uint64(1), h.d.st.open.ID)
	require.Empty(t, h.pub.events, "no second started event")
}

func TestSweepDropsStaleItemsAfterReplay(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.store.oldest = new(t0.Add(-10 * time.Minute))

	h.step(t, 0)
	require.Equal(t, []time.Time{t0.Add(-staleAfter)}, h.store.dropped)
	require.Equal(t, [2]time.Time{t0.Add(-10*time.Minute - replayMargin), t0}, h.journal.windows[0])
	require.Equal(t, []time.Time{t0}, h.store.beats)

	// Not again before sweepEvery.
	h.alive()
	h.step(t, 10*time.Second)
	require.Len(t, h.store.dropped, 1)
}

func TestSweepWaitsWhileAnOutageIsOpen(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.store.oldest = new(t0.Add(-10 * time.Minute))
	h.d.st.open = &Outage{ID: 9}
	h.icecast.err = errors.New("down")
	h.d.st.icecastFails = 5

	h.step(t, 0)
	require.Empty(t, h.store.dropped)
}

func TestMarkingGoesOnWhenTheJournalCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.journal.err = errors.New("permission denied")
	end := t0.Add(time.Minute)
	h.store.outages = []Outage{{ID: 1, StartedAt: t0, EndedAt: &end}}

	n, err := h.d.mark(t.Context(), h.store.outages[0])
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, []uint64{1}, h.store.marked)
}

func TestStartupRecordsBackendDowntime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		alive    *time.Time
		unaired  int
		open     bool
		wantNew  bool
		wantMark int
	}{
		{name: "down with unaired items", alive: new(t0.Add(-10 * time.Minute)), unaired: 4, wantNew: true, wantMark: 1},
		{name: "down, everything aired", alive: new(t0.Add(-10 * time.Minute)), unaired: 0},
		{name: "short restart", alive: new(t0.Add(-5 * time.Second)), unaired: 4},
		{name: "first start", unaired: 4},
		{name: "an outage is open", alive: new(t0.Add(-10 * time.Minute)), unaired: 4, open: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.store.heartbeat, h.store.unaired = tt.alive, tt.unaired
			if tt.open {
				h.store.outages = []Outage{{ID: 1, StartedAt: t0.Add(-time.Hour), Component: ComponentIcecast}}
			}

			h.d.startup(t.Context())

			require.Equal(t, []operations.Kind{operations.KindPlayoutReconcile}, h.ops.kinds)
			require.NoError(t, h.ops.errs[0])
			if tt.wantNew {
				o := h.store.outages[len(h.store.outages)-1]
				require.Equal(t, ComponentBackend, o.Component)
				require.Equal(t, DetectedByBackend, o.DetectedBy)
				require.Equal(t, *tt.alive, o.StartedAt)
				require.Equal(t, t0, *o.EndedAt)
			} else {
				require.Len(t, h.store.outages, map[bool]int{true: 1, false: 0}[tt.open])
			}
			require.Len(t, h.store.marked, tt.wantMark)
			require.Equal(t, tt.open, h.d.st.open != nil)
			if tt.alive != nil {
				require.Equal(t, tt.alive.Add(-replayMargin), h.journal.windows[0][0], "the replay covers the downtime")
			}
		})
	}
}

func TestOutageIsNotOpenWhenTheBroadcastStateIsNotStored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.icecast.err = errors.New("mount not found")
	h.bc.err = errors.New("database down")

	h.step(t, 0)
	h.alive()
	h.now = h.now.Add(5 * time.Second)
	require.ErrorIs(t, h.d.evaluate(t.Context()), h.bc.err)
	require.Nil(t, h.d.st.open, "retried at the next evaluation")
	require.Empty(t, h.pub.events)

	h.bc.err = nil
	h.alive()
	h.step(t, time.Second)
	require.NotNil(t, h.d.st.open)
	require.Len(t, h.bc.opened, 1)
}

func TestOutageAdoptedAtStartupIsToldToTheBroadcastState(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	reason := "mount gone"
	h.store.outages = []Outage{{ID: 1, StartedAt: t0.Add(-time.Hour), Component: ComponentIcecast, Reason: &reason}}
	h.icecast.err = errors.New("down")
	h.d.startup(t.Context())
	require.Empty(t, h.bc.opened, "told by the evaluation, not the reconcile operation")

	h.bc.err = errors.New("database down")
	require.ErrorIs(t, h.d.evaluate(t.Context()), h.bc.err)
	h.bc.err = nil
	h.alive()
	h.step(t, time.Second)
	h.alive()
	h.step(t, time.Second)
	require.Equal(t, []broadcast.Outage{{ID: 1, StartedAt: t0.Add(-time.Hour), Reason: reason}}, h.bc.opened, "told once")
}
