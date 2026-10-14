package playout

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// fakeStore keeps the plan items and the play log in memory.
type fakeStore struct {
	mu     sync.Mutex
	items  []Item // plan rows: planned or queued
	logged map[uint64]*logRow
	emerg  []logRow
	tracks map[uint64]Track
	lists  map[uint64][]Track
}

type logRow struct {
	item      Item
	started   time.Time
	ended     bool
	actual    int
	truncated bool
}

func newFakeStore(items ...Item) *fakeStore {
	return &fakeStore{items: items, logged: map[uint64]*logRow{}, tracks: map[uint64]Track{}, lists: map[uint64][]Track{}}
}

func (f *fakeStore) WithTx(db.Tx) Store             { return f }
func (f *fakeStore) LockPlan(context.Context) error { return nil }

func (f *fakeStore) Items(_ context.Context, from, to time.Time) ([]Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Item
	for _, it := range f.items {
		if !it.Start.Before(from) && it.Start.Before(to) {
			out = append(out, it)
		}
	}
	slices.SortFunc(out, func(a, b Item) int { return a.Start.Compare(b.Start) })
	return out, nil
}

func (f *fakeStore) set(id uint64, from, to bool) bool {
	for i := range f.items {
		if f.items[i].ID == id && f.items[i].Queued == from {
			f.items[i].Queued = to
			return true
		}
	}
	return false
}

func (f *fakeStore) MarkQueued(_ context.Context, id uint64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.set(id, false, true), nil
}

func (f *fakeStore) MarkPlanned(_ context.Context, id uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.set(id, true, false)
	return nil
}

func (f *fakeStore) Unqueue(_ context.Context, from, to time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for i := range f.items {
		if f.items[i].Queued && !f.items[i].Start.Before(from) && f.items[i].Start.Before(to) {
			f.items[i].Queued = false
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) Start(_ context.Context, id uint64, at time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.items, func(it Item) bool { return it.ID == id })
	if i < 0 {
		return false, nil
	}
	it := f.items[i]
	f.items = slices.Delete(f.items, i, i+1)
	f.logged[id] = &logRow{item: it, started: at, actual: it.OutMS - it.InMS, truncated: it.Truncated}
	return true, nil
}

func (f *fakeStore) End(_ context.Context, id uint64, elapsedMS, margin int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.logged[id]
	if !ok || r.ended {
		return false, nil
	}
	r.ended, r.actual = true, elapsedMS
	r.truncated = r.truncated || elapsedMS < r.item.OutMS-margin
	return true, nil
}

func (f *fakeStore) LogEmergency(_ context.Context, trackID uint64, at time.Time, elapsedMS int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emerg = append(f.emerg, logRow{item: Item{TrackID: trackID}, started: at, ended: true, actual: elapsedMS})
	return nil
}

func (f *fakeStore) Logged(_ context.Context, id uint64) (NowPlaying, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.logged[id]
	if !ok {
		return NowPlaying{}, false, nil
	}
	return NowPlaying{ItemID: new(id), TrackID: r.item.TrackID, Title: "T", Source: SourcePlan, StartedAt: r.started}, true, nil
}

func (f *fakeStore) Track(_ context.Context, id uint64) (Track, bool, error) {
	t, ok := f.tracks[id]
	return t, ok, nil
}

func (f *fakeStore) PlaylistTracks(_ context.Context, id uint64) ([]Track, error) {
	return f.lists[id], nil
}

func (f *fakeStore) Queued(_ context.Context, limit int) ([]QueuedItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []QueuedItem
	for _, it := range f.items {
		if it.Queued && len(out) < limit {
			out = append(out, QueuedItem{ID: it.ID, TrackID: it.TrackID, Start: it.Start, OutMS: it.OutMS})
		}
	}
	return out, nil
}

func (f *fakeStore) queued() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []uint64
	for _, it := range f.items {
		if it.Queued {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// fakeControl records the commands; queues are what Liquidsoap holds.
type fakeControl struct {
	gen     uint64
	down    bool
	failOn  string
	pushed  map[string][]string
	queues  map[string][]int
	vars    map[string]string
	nextRID int
}

func newFakeControl() *fakeControl {
	return &fakeControl{gen: 1, pushed: map[string][]string{}, queues: map[string][]int{}, vars: map[string]string{}}
}

var errDown = errors.Join(liquidsoap.ErrUnavailable, errors.New("connection refused"))

func (c *fakeControl) Push(_ context.Context, queue, uri string) (int, error) {
	if c.down || c.failOn == "push" {
		return 0, errDown
	}
	c.pushed[queue] = append(c.pushed[queue], uri)
	c.nextRID++
	c.queues[queue] = append(c.queues[queue], c.nextRID)
	return c.nextRID, nil
}

func (c *fakeControl) Queue(_ context.Context, queue string) ([]int, error) {
	if c.down {
		return nil, errDown
	}
	return c.queues[queue], nil
}

func (c *fakeControl) SetVar(_ context.Context, name, value string) error {
	if c.down {
		return errDown
	}
	c.vars[name] = value
	return nil
}

func (c *fakeControl) Ping(context.Context) error {
	if c.down {
		return errDown
	}
	return nil
}

func (c *fakeControl) Generation() uint64      { return c.gen }
func (c *fakeControl) State() liquidsoap.State { return liquidsoap.State{Connected: !c.down} }

type txRunner struct{}

func (txRunner) WithTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	return fn(ctx, db.Tx{})
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

var yerevan = mustLoad("Asia/Yerevan")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// h10 is 10:00 UTC, 14:00 in Yerevan; parity 0.
var h10 = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

type harness struct {
	svc   *Service
	store *fakeStore
	ctl   *fakeControl
	pub   *publisher
	now   time.Time
	set   Settings
}

func newHarness(t *testing.T, items ...Item) *harness {
	t.Helper()
	h := &harness{
		store: newFakeStore(items...), ctl: newFakeControl(), pub: &publisher{}, now: h10.Add(10 * time.Second),
		set: Settings{
			Location: yerevan, Lookahead: 2, Prepush: 5 * time.Minute, FadeOutMS: 2500, MediaRoot: "/media", MusicGain: 1,
		},
	}
	h.svc = NewService(txRunner{}, h.store, h.ctl,
		func(context.Context) (Settings, error) { return h.set, nil }, h.pub,
		slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return h.now }, rand.New(rand.NewPCG(1, 2))) //nolint:gosec // G404: emergency picks, not security
	h.svc.push.shift = -1
	return h
}

// item: ID id, planned at h10 + offset, 3 minutes long.
func item(id uint64, offset time.Duration) Item {
	return Item{ID: id, TrackID: id, FilePath: "a/b.mp3", Start: h10.Add(offset), OutMS: 180000}
}

func TestQueueNameAndHourShift(t *testing.T) {
	t.Parallel()
	kolkata := mustLoad("Asia/Kolkata") // UTC+05:30
	tests := []struct {
		name  string
		hour  time.Time
		loc   *time.Location
		shift int
		queue string
	}{
		{"whole-hour offset, even UTC hour", h10, yerevan, 0, "hour_0"},
		{"whole-hour offset, odd UTC hour", h10.Add(time.Hour), yerevan, 0, "hour_1"},
		{"UTC", h10.Add(time.Hour), time.UTC, 0, "hour_1"},
		{"half-hour offset", time.Date(2026, 10, 15, 10, 30, 0, 0, time.UTC), kolkata, 1800, "hour_0"},
		{"half-hour offset, next hour", time.Date(2026, 10, 15, 11, 30, 0, 0, time.UTC), kolkata, 1800, "hour_1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.shift, hourShift(tt.hour, tt.loc))
			require.Equal(t, tt.queue, QueueName(tt.hour, tt.loc))
			require.Equal(t, tt.hour, hourStart(tt.hour.Add(59*time.Minute), tt.loc))
		})
	}
}

func TestRequestURI(t *testing.T) {
	t.Parallel()
	set := Settings{MediaRoot: "/media", FadeOutMS: 2500}
	require.Equal(t, `annotate:item_id="7":/media/a/b.mp3`, RequestURI(item(7, 0), set))
	it := item(8, 0)
	it.Truncated, it.OutMS = true, 123456
	require.Equal(t, `annotate:item_id="8",liq_cue_out="123.456",liq_fade_out="2.5":/media/a/b.mp3`, RequestURI(it, set))
	it.InMS = 500
	require.Contains(t, RequestURI(it, set), `liq_cue_in="0.5"`)
}

func TestPassFillsLookahead(t *testing.T) {
	t.Parallel()
	h := newHarness(t, item(1, 0), item(2, 3*time.Minute), item(3, 6*time.Minute), item(4, 65*time.Minute))
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, []uint64{1, 2}, h.store.queued(), "lookahead 2")
	require.Len(t, h.ctl.pushed["hour_0"], 2)
	require.Equal(t, `annotate:item_id="1":/media/a/b.mp3`, h.ctl.pushed["hour_0"][0])
	require.Equal(t, "0.", h.ctl.vars[varHourShift])
	require.Equal(t, "1.", h.ctl.vars[varMusicGain], "the setting on the first connect")

	// Item 1 starts: a slot is free, item 3 is pushed after item 2.
	require.NoError(t, h.svc.Callback(t.Context(), Event{Kind: EventStart, Source: SourcePlan, ItemID: new(uint64(1)), Time: h10}))
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, []uint64{2, 3}, h.store.queued())
	require.Empty(t, h.ctl.pushed["hour_1"], "the next hour is outside the pre-push window")

	// Within the pre-push window the next hour fills its own queue.
	h.now = h10.Add(56 * time.Minute)
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, []string{`annotate:item_id="4":/media/a/b.mp3`}, h.ctl.pushed["hour_1"])
}

func TestPassSkipsLateItems(t *testing.T) {
	t.Parallel()
	h := newHarness(t, item(1, 0), item(2, 3*time.Minute), item(3, 6*time.Minute), item(4, 9*time.Minute))
	h.now = h10.Add(3*time.Minute + 40*time.Second) // item 2 is 40 s late
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, []uint64{3, 4}, h.store.queued(), "no catch-up")

	h2 := newHarness(t, item(1, 0), item(2, 3*time.Minute))
	h2.now = h10.Add(3*time.Minute + 10*time.Second)
	require.NoError(t, h2.svc.pass(t.Context()))
	require.Equal(t, []uint64{2}, h2.store.queued(), "a little late is still pushed")
}

func TestPassNeverPushesBeforeAQueuedItem(t *testing.T) {
	t.Parallel()
	first := item(1, 0)
	second := item(2, 3*time.Minute)
	second.Queued = true
	h := newHarness(t, first, second, item(3, 6*time.Minute))
	h.ctl.queues["hour_0"] = []int{99}
	h.svc.push.gen = 1 // no resync
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, []uint64{2, 3}, h.store.queued(), "item 1 is behind the queue; it stays planned")
}

func TestPassCutoffAtHourEnd(t *testing.T) {
	t.Parallel()
	h := newHarness(t, item(1, 58*time.Minute), item(2, 59*time.Minute))
	h.set.Prepush = time.Minute
	h.now = h10.Add(time.Hour - 3*time.Second)
	require.NoError(t, h.svc.pass(t.Context()))
	require.Empty(t, h.ctl.pushed["hour_0"], "the switch flushes this queue in a moment")
}

func TestPushFailureRevertsItem(t *testing.T) {
	t.Parallel()
	h := newHarness(t, item(1, 0))
	h.ctl.failOn = "push"
	require.ErrorIs(t, h.svc.pass(t.Context()), liquidsoap.ErrUnavailable)
	require.Empty(t, h.store.queued(), "planned again, pushed at the next pass")
}

func TestResyncAfterLiquidsoapRestart(t *testing.T) {
	t.Parallel()
	a, b := item(1, 0), item(2, 3*time.Minute)
	a.Queued, b.Queued = true, true
	h := newHarness(t, a, b, item(3, 6*time.Minute))
	h.svc.push.gen = 1
	h.ctl.queues["hour_0"] = []int{5, 6}
	require.NoError(t, h.svc.pass(t.Context()))
	require.Empty(t, h.ctl.pushed["hour_0"], "the queue holds the items")

	// A new connection to a Liquidsoap that lost its queues.
	h.ctl.gen, h.ctl.queues = 2, map[string][]int{}
	require.NoError(t, h.svc.SetMusicGain(t.Context(), 0.2))
	h.ctl.vars = map[string]string{}
	require.NoError(t, h.svc.pass(t.Context()))
	require.Len(t, h.ctl.pushed["hour_0"], 2, "items 1 and 2 are pushed again")
	require.Equal(t, "0.2", h.ctl.vars[varMusicGain], "the live gain survives a reconnect")
	require.Equal(t, "0.", h.ctl.vars[varHourShift])
}

func TestPassLinkDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, item(1, 0))
	h.ctl.down = true
	require.ErrorIs(t, h.svc.pass(t.Context()), liquidsoap.ErrUnavailable)
	require.ErrorIs(t, h.svc.SetMusicGain(t.Context(), 0.5), ErrUnavailable)
	require.Empty(t, h.store.queued())
}

func TestMusicGainSettingChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.NoError(t, h.svc.pass(t.Context()))
	h.svc.MusicGainSettingChanged(0.5)
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, "0.5", h.ctl.vars[varMusicGain])
	st, err := h.svc.Status(t.Context())
	require.NoError(t, err)
	require.InDelta(t, 0.5, *st.MusicGain, 0)
}

func TestCallbacks(t *testing.T) {
	t.Parallel()
	trunc := item(2, 3*time.Minute)
	trunc.Truncated, trunc.OutMS = true, 60000
	h := newHarness(t, item(1, 0), trunc, item(3, 6*time.Minute))
	ctx := t.Context()
	start := func(id uint64, at time.Time) Event {
		return Event{Kind: EventStart, Source: SourcePlan, ItemID: new(id), Time: at}
	}
	end := func(id uint64, at time.Time, elapsed int) Event {
		return Event{Kind: EventEnd, Source: SourcePlan, ItemID: new(id), Time: at, ElapsedMS: elapsed}
	}

	require.NoError(t, h.svc.Callback(ctx, start(1, h10)))
	require.NoError(t, h.svc.Callback(ctx, start(1, h10.Add(time.Second))), "a repeated start")
	require.Len(t, h.pub.events, 1)
	np := h.pub.events[0].(ws.NowPlayingEvent)
	require.Equal(t, uint64(1), *np.Data.ItemID)
	require.Equal(t, h10, h.store.logged[1].started)

	require.NoError(t, h.svc.Callback(ctx, end(1, h10.Add(3*time.Minute), 179900)))
	require.NoError(t, h.svc.Callback(ctx, end(1, h10.Add(time.Hour), 3000)), "a late duplicate end")
	require.Equal(t, 179900, h.store.logged[1].actual)
	require.False(t, h.store.logged[1].truncated)
	st, err := h.svc.Status(ctx)
	require.NoError(t, err)
	require.Nil(t, st.NowPlaying, "the item ended")
	require.NotNil(t, st.LastCallbackAt)

	// Start lost: the end logs the item, started elapsed before.
	require.NoError(t, h.svc.Callback(ctx, end(2, h10.Add(4*time.Minute), 60000)))
	require.Equal(t, h10.Add(3*time.Minute), h.store.logged[2].started)
	require.True(t, h.store.logged[2].truncated)

	// Cut by the hour switch.
	require.NoError(t, h.svc.Callback(ctx, start(3, h10.Add(4*time.Minute))))
	require.NoError(t, h.svc.Callback(ctx, end(3, h10.Add(5*time.Minute), 60000)))
	require.True(t, h.store.logged[3].truncated)

	// Unknown items and events without one are ignored.
	require.NoError(t, h.svc.Callback(ctx, start(99, h10)))
	require.NoError(t, h.svc.Callback(ctx, end(99, h10, 1000)))
	require.NoError(t, h.svc.Callback(ctx, Event{Kind: EventStart, Source: SourcePlan, Time: h10}))
}

func TestOutputCallback(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	require.NoError(t, h.svc.Callback(ctx, Event{Kind: EventOutput, Time: h10, Connected: true}))
	require.NoError(t, h.svc.Callback(ctx, Event{Kind: EventOutput, Time: h10, Connected: true}))
	require.NoError(t, h.svc.Callback(ctx, Event{Kind: EventOutput, Time: h10.Add(time.Second), Connected: false}))
	require.Len(t, h.pub.events, 2, "only changes are sent")
	require.Equal(t, ws.IcecastDisconnected, h.pub.events[1].(ws.IcecastStateEvent).Data.State)
	st, err := h.svc.Status(ctx)
	require.NoError(t, err)
	require.False(t, st.Output.Connected)
	require.Equal(t, h10.Add(time.Second), st.Output.Since)
}

func TestEmergency(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := t.Context()
	_, ok, err := h.svc.EmergencyNext(ctx)
	require.NoError(t, err)
	require.False(t, ok, "no emergency playlist")

	h.set.EmergencyPlaylistID = new(uint64(9))
	_, ok, err = h.svc.EmergencyNext(ctx)
	require.NoError(t, err)
	require.False(t, ok, "an empty playlist")

	h.store.lists[9] = []Track{{ID: 1, FilePath: "e/1.mp3"}, {ID: 2, FilePath: "e/2.mp3"}}
	prev := ""
	for range 10 {
		uri, ok, err := h.svc.EmergencyNext(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		require.NotEqual(t, prev, uri, "never the same track twice in a row")
		prev = uri
	}
	require.Contains(t, []string{`annotate:track_id="1":/media/e/1.mp3`, `annotate:track_id="2":/media/e/2.mp3`}, prev)

	h.store.tracks[1] = Track{ID: 1, Title: "E1", DurationMS: new(20000)}
	require.NoError(t, h.svc.Callback(ctx, Event{Kind: EventStart, Source: SourceEmergency, TrackID: new(uint64(1)), Time: h10}))
	np := h.pub.events[0].(ws.NowPlayingEvent)
	require.Equal(t, SourceEmergency, np.Data.Source)
	require.Nil(t, np.Data.ItemID)
	require.NoError(t, h.svc.Callback(ctx, Event{
		Kind: EventEnd, Source: SourceEmergency, TrackID: new(uint64(1)), Time: h10.Add(5 * time.Second), ElapsedMS: 5000,
	}))
	require.Len(t, h.store.emerg, 1)
	require.Equal(t, h10, h.store.emerg[0].started)
	require.Equal(t, 5000, h.store.emerg[0].actual)
}
