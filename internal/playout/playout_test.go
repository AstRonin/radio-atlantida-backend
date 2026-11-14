package playout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
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
	return NowPlaying{
		ItemID: new(id), TrackID: r.item.TrackID, Title: "T", Source: SourcePlan, StartedAt: r.started,
		Truncated: r.truncated,
	}, true, nil
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
	// commands are the raw commands sent (output.set); reply answers them,
	// "OK applied" when nil.
	commands []string
	reply    func(cmd string) []string
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

func (c *fakeControl) Command(_ context.Context, cmd string) ([]string, error) {
	if c.down {
		return nil, errDown
	}
	c.commands = append(c.commands, cmd)
	if c.reply != nil {
		return c.reply(cmd), nil
	}
	return []string{"OK applied"}, nil
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

// outputs records the output reports passed on to the broadcast state.
type outputs struct {
	mu      sync.Mutex
	reports []bool
	ats     []time.Time
}

func (o *outputs) OnOutput(_ context.Context, connected bool, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reports = append(o.reports, connected)
	o.ats = append(o.ats, at)
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
	out   *outputs
	now   time.Time
	set   Settings
}

func newHarness(t *testing.T, items ...Item) *harness {
	t.Helper()
	h := &harness{
		store: newFakeStore(items...), ctl: newFakeControl(), pub: &publisher{}, out: &outputs{},
		now: h10.Add(10 * time.Second),
		set: Settings{
			Location: yerevan, Lookahead: 2, Prepush: 5 * time.Minute, FadeOutMS: 2500, MediaRoot: "/media", MusicGain: 1,
			DuckGain: 0.2,
		},
	}
	h.svc = NewService(txRunner{}, h.store, h.ctl,
		func(context.Context) (Settings, error) { return h.set, nil }, h.pub, h.out,
		slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return h.now }, rand.New(rand.NewPCG(1, 2))) //nolint:gosec // G404: emergency picks, not security
	h.svc.push.shift, h.svc.push.duck = -1, -1
	return h
}

// item: ID id, planned at h10 + offset, 3 minutes long.
func item(id uint64, offset time.Duration) Item {
	return Item{ID: id, TrackID: id, FilePath: "a/b.mp3", Title: "Song", Artist: "Band", Start: h10.Add(offset), OutMS: 180000}
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
	require.Equal(t, `annotate:item_id="7",title="Song",artist="Band":/media/a/b.mp3`, RequestURI(item(7, 0), set))
	it := item(8, 0)
	it.Truncated, it.OutMS = true, 123456
	require.Equal(t, `annotate:item_id="8",liq_cue_out="123.456",liq_fade_out="2.5",title="Song",artist="Band":/media/a/b.mp3`,
		RequestURI(it, set))
	it.InMS = 500
	require.Contains(t, RequestURI(it, set), `liq_cue_in="0.5"`)

	// A jingle without an artist: the empty artist hides the file's tag.
	it = item(9, 0)
	it.Title, it.Artist = `Station "ID"`, ""
	require.Equal(t, `annotate:item_id="9",title="Station \"ID\"",artist="":/media/a/b.mp3`, RequestURI(it, set))
}

// The cut and fades of a track (docs/phase-22-plan.md §3.2).
func TestRequestURIWithCue(t *testing.T) {
	t.Parallel()
	set := Settings{MediaRoot: "/media", FadeOutMS: 2500}
	const tail = `title="Song",artist="Band":/media/a/b.mp3`
	at := func(in, out int, truncated bool, fadeIn, fadeOut int) Item {
		it := item(7, 0)
		it.InMS, it.OutMS, it.Truncated, it.FileDurationMS = in, out, truncated, new(180000)
		it.FadeInMS, it.FadeOutMS = fadeIn, fadeOut
		return it
	}
	tests := []struct {
		name string
		it   Item
		want string
	}{
		{"no cue: the whole file", at(0, 180000, false, 0, 0), `annotate:item_id="7",` + tail},
		{"cue in only", at(5000, 180000, false, 0, 0), `annotate:item_id="7",liq_cue_in="5.",` + tail},
		{"cue out before the end", at(0, 170000, false, 0, 0), `annotate:item_id="7",liq_cue_out="170.",` + tail},
		{
			"truncated with a cue: the setting's fade",
			at(5000, 65000, true, 1000, 2000),
			`annotate:item_id="7",liq_cue_in="5.",liq_cue_out="65.",liq_fade_in="1.",liq_fade_out="2.5",` + tail,
		},
		{
			"fades at the cue points",
			at(5000, 170000, false, 1000, 2000),
			`annotate:item_id="7",liq_cue_in="5.",liq_cue_out="170.",liq_fade_in="1.",liq_fade_out="2.",` + tail,
		},
		{"fades without a cut", at(0, 180000, false, 0, 1500), `annotate:item_id="7",liq_fade_out="1.5",` + tail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, RequestURI(tt.it, set))
		})
	}
}

func TestEmergencyURIWithCue(t *testing.T) {
	t.Parallel()
	set := Settings{MediaRoot: "/media"}
	tr := Track{ID: 3, Title: "Song", FilePath: "a/b.mp3", DurationMS: new(180000)}
	require.Equal(t, `annotate:track_id="3",title="Song",artist="":/media/a/b.mp3`, EmergencyURI(tr, set))
	tr.CueInMS, tr.CueOutMS, tr.FadeInMS, tr.FadeOutMS = 5000, new(45000), 1000, 2000
	require.Equal(t,
		`annotate:track_id="3",liq_cue_in="5.",liq_cue_out="45.",liq_fade_in="1.",liq_fade_out="2.",title="Song",artist="":/media/a/b.mp3`,
		EmergencyURI(tr, set))
	require.Equal(t, 45000, *tr.outMS())
}

func TestPassFillsLookahead(t *testing.T) {
	t.Parallel()
	h := newHarness(t, item(1, 0), item(2, 3*time.Minute), item(3, 6*time.Minute), item(4, 65*time.Minute))
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, []uint64{1, 2}, h.store.queued(), "lookahead 2")
	require.Len(t, h.ctl.pushed["hour_0"], 2)
	require.Equal(t, `annotate:item_id="1",title="Song",artist="Band":/media/a/b.mp3`, h.ctl.pushed["hour_0"][0])
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
	require.Equal(t, []string{`annotate:item_id="4",title="Song",artist="Band":/media/a/b.mp3`}, h.ctl.pushed["hour_1"])
}

func TestPassSkipsLateItems(t *testing.T) {
	t.Parallel()
	h := newHarness(t, item(1, 0), item(2, 3*time.Minute), item(3, 6*time.Minute), item(4, 9*time.Minute))
	h.svc.push.gen = 1                              // no resync: the late rules only
	h.now = h10.Add(3*time.Minute + 40*time.Second) // item 2 is 40 s late
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, []uint64{3, 4}, h.store.queued(), "no catch-up")

	h2 := newHarness(t, item(1, 0), item(2, 3*time.Minute))
	h2.svc.push.gen = 1
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
	require.True(t, h.svc.push.holdUntil.IsZero(), "item 1 is due: pushed at once, nothing held")
	require.Equal(t, "0.2", h.ctl.vars[varMusicGain], "the live gain survives a reconnect")
	require.Equal(t, "0.", h.ctl.vars[varHourShift])

	// A restart when the next item is in the future: item 1 (0:00-3:00)
	// ended, item 2 (3:00) started, items 3 and 4 were queued.
	c, d := item(3, 6*time.Minute), item(4, 9*time.Minute)
	c.Queued, d.Queued = true, true
	h = newHarness(t, c, d)
	h.svc.push.gen = 1
	h.ctl.gen = 2
	h.now = h10.Add(3*time.Minute + 40*time.Second)
	require.NoError(t, h.svc.pass(t.Context()))
	require.Empty(t, h.ctl.pushed["hour_0"], "item 3 is held until its planned start")
	require.Empty(t, h.store.queued())
	require.Equal(t, h10.Add(6*time.Minute-holdLead), h.svc.push.holdUntil)
	h.now = h10.Add(6*time.Minute - 600*time.Millisecond)
	require.NoError(t, h.svc.pass(t.Context()))
	require.Empty(t, h.ctl.pushed["hour_0"], "still held")
	h.now = h10.Add(6*time.Minute - holdLead)
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, []uint64{3, 4}, h.store.queued(), "the held item and the rest of the lookahead")
	require.True(t, h.svc.push.holdUntil.IsZero())
	require.Empty(t, h.svc.push.align)
}

// The edge cases of the hold after a resync
// (docs/output-state-and-resync-plan.md P2.5). Every case starts with a
// resync (the harness's first pass sees a new connection) with empty queues.
func TestResyncHoldEdgeCases(t *testing.T) {
	t.Parallel()
	pass := func(t *testing.T, h *harness, at time.Duration) {
		t.Helper()
		h.now = h10.Add(at)
		require.NoError(t, h.svc.pass(t.Context()))
	}
	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"held item late by 10 s: pushed at once", func(t *testing.T) {
			h := newHarness(t, item(2, 3*time.Minute), item(3, 6*time.Minute))
			pass(t, h, 3*time.Minute+10*time.Second)
			require.Equal(t, []uint64{2, 3}, h.store.queued())
			require.True(t, h.svc.push.holdUntil.IsZero())
		}},
		{"held item late by 40 s: skipped, the next one held", func(t *testing.T) {
			h := newHarness(t, item(2, 3*time.Minute), item(3, 6*time.Minute))
			pass(t, h, 3*time.Minute+40*time.Second)
			require.Empty(t, h.store.queued())
			require.Equal(t, h10.Add(6*time.Minute-holdLead), h.svc.push.holdUntil)
		}},
		{"the next hour in the pre-push window is pushed during the hold", func(t *testing.T) {
			h := newHarness(t, item(1, 54*time.Minute), item(2, 57*time.Minute), item(3, 60*time.Minute))
			pass(t, h, 55*time.Minute+30*time.Second)
			require.Empty(t, h.ctl.pushed["hour_0"])
			require.Len(t, h.ctl.pushed["hour_1"], 1, "the next hour starts by the script's switch")
			require.Equal(t, h10.Add(57*time.Minute-holdLead), h.svc.push.holdUntil)
		}},
		{"a new connection during the hold decides again", func(t *testing.T) {
			h := newHarness(t, item(3, 6*time.Minute), item(4, 9*time.Minute))
			pass(t, h, 3*time.Minute+40*time.Second)
			require.NotZero(t, h.svc.push.holdUntil)
			// A reconnect to a Liquidsoap whose queue still plays.
			h.ctl.gen++
			h.ctl.queues["hour_0"] = []int{42}
			pass(t, h, 4*time.Minute)
			require.Empty(t, h.svc.push.align)
			require.True(t, h.svc.push.holdUntil.IsZero())
			require.Equal(t, []uint64{3, 4}, h.store.queued(), "no hold: the queue plays on its own")
		}},
		{"a rebuilt hour moves the due time", func(t *testing.T) {
			h := newHarness(t, item(3, 6*time.Minute), item(4, 9*time.Minute))
			pass(t, h, 3*time.Minute+40*time.Second)
			h.store.mu.Lock()
			h.store.items = []Item{item(30, 7*time.Minute), item(40, 10*time.Minute)}
			h.store.mu.Unlock()
			pass(t, h, 6*time.Minute-holdLead)
			require.Empty(t, h.store.queued())
			require.Equal(t, h10.Add(7*time.Minute-holdLead), h.svc.push.holdUntil)
		}},
		{"a pass after the hour boundary drops the old hour", func(t *testing.T) {
			h := newHarness(t, item(1, 54*time.Minute))
			pass(t, h, 58*time.Minute)
			require.True(t, h.svc.push.align[h10], "nothing can air; the hour stays held")
			require.True(t, h.svc.push.holdUntil.IsZero())
			pass(t, h, 60*time.Minute+10*time.Second)
			require.Empty(t, h.svc.push.align)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.run(t)
		})
	}
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

func TestDuckGain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, "0.2", h.ctl.vars[varDuckGain])

	// A settings change is set at the next pass.
	h.set.DuckGain = 0
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, "0.", h.ctl.vars[varDuckGain])

	// A new connection (Liquidsoap restarted) gets it again.
	delete(h.ctl.vars, varDuckGain)
	require.NoError(t, h.svc.pass(t.Context()))
	require.NotContains(t, h.ctl.vars, varDuckGain, "same connection: not set again")
	h.ctl.gen++
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, "0.", h.ctl.vars[varDuckGain])
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
	require.Len(t, h.pub.events, 2, "one track_ended for the first end only")
	require.Equal(t, ws.TrackEndedEvent{
		Type: ws.EventTrackEnded, Time: h.pub.events[1].(ws.TrackEndedEvent).Time,
		Data: ws.TrackEndedData{
			ItemID: new(uint64(1)), TrackID: 1, Source: SourcePlan, Status: "aired", StartedAt: h10,
			EndedAt: h10.Add(3 * time.Minute), ActualDurationMS: 179900,
		},
	}, h.pub.events[1])
	st, err := h.svc.Status(ctx)
	require.NoError(t, err)
	require.Nil(t, st.NowPlaying, "the item ended")
	require.NotNil(t, st.LastCallbackAt)

	// Start lost: the end logs the item, started elapsed before.
	require.NoError(t, h.svc.Callback(ctx, end(2, h10.Add(4*time.Minute), 60000)))
	require.Equal(t, h10.Add(3*time.Minute), h.store.logged[2].started)
	require.True(t, h.store.logged[2].truncated)
	ended := h.pub.events[2].(ws.TrackEndedEvent).Data
	require.Equal(t, "truncated", ended.Status, "the status after the end")
	require.Equal(t, h10.Add(3*time.Minute), ended.StartedAt)

	// Cut by the hour switch.
	require.NoError(t, h.svc.Callback(ctx, start(3, h10.Add(4*time.Minute))))
	require.NoError(t, h.svc.Callback(ctx, end(3, h10.Add(5*time.Minute), 60000)))
	require.True(t, h.store.logged[3].truncated)
	require.Equal(t, "truncated", h.pub.events[4].(ws.TrackEndedEvent).Data.Status, "cut by the hour switch")
	require.Len(t, h.pub.events, 5)

	// Unknown items and events without one are ignored.
	require.NoError(t, h.svc.Callback(ctx, start(99, h10)))
	require.NoError(t, h.svc.Callback(ctx, end(99, h10, 1000)))
	require.NoError(t, h.svc.Callback(ctx, Event{Kind: EventStart, Source: SourcePlan, Time: h10}))
	require.Len(t, h.pub.events, 5, "nothing sent for unknown items")

	// A replayed journal end records the item but sends nothing: it is not
	// live.
	h2 := newHarness(t, item(4, 0))
	require.NoError(t, h2.svc.Replay(ctx, end(4, h10.Add(3*time.Minute), 180000)))
	require.True(t, h2.store.logged[4].ended)
	require.Empty(t, h2.pub.events)
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
	require.Equal(t, ws.IcecastStateEvent{
		Type: ws.EventIcecastState, Time: h10, Data: ws.IcecastStateData{State: ws.IcecastConnected},
	}, h.pub.events[0], "the event time is the reported time")
	require.Equal(t, []bool{true, true, false}, h.out.reports, "every report goes on to the broadcast state")
	st, err := h.svc.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, ws.IcecastDisconnected, st.Output.State)
	require.Equal(t, h10.Add(time.Second), st.Output.Since)
}

// icecastStates returns the states of the icecast.state events sent.
func icecastStates(h *harness) []string {
	h.pub.mu.Lock()
	defer h.pub.mu.Unlock()
	var got []string
	for _, e := range h.pub.events {
		if ev, ok := e.(ws.IcecastStateEvent); ok {
			got = append(got, ev.Data.State)
		}
	}
	return got
}

// The output state in the heartbeat (contract 2,
// docs/output-state-and-resync-plan.md P1).
func TestHeartbeatOutput(t *testing.T) {
	t.Parallel()
	since := h10.Add(-time.Hour)
	beat := func(n int, connected *bool, since *time.Time) Event {
		return Event{
			Kind: EventHeartbeat, Time: h10.Add(time.Duration(n) * 5 * time.Second), OutputConnected: connected, OutputSince: since,
		}
	}
	tests := []struct {
		name        string
		events      []Event
		wantState   string // empty: absent
		wantSince   time.Time
		wantEvents  []string
		wantReports []bool
	}{
		{
			name: "after a backend start the first heartbeat sets the state", events: []Event{beat(1, new(true), &since)},
			wantState: ws.IcecastConnected, wantSince: since, wantEvents: []string{"connected"}, wantReports: []bool{true},
		},
		{
			name:      "repeated identical heartbeats send nothing but reach the broadcast state",
			events:    []Event{beat(1, new(true), &since), beat(2, new(true), &since), beat(3, new(true), &since)},
			wantState: ws.IcecastConnected, wantSince: since, wantEvents: []string{"connected"}, wantReports: []bool{true, true, true},
		},
		{
			name:      "false after true sends disconnected",
			events:    []Event{beat(1, new(true), &since), beat(2, new(false), new(h10.Add(8*time.Second)))},
			wantState: ws.IcecastDisconnected, wantSince: h10.Add(8 * time.Second),
			wantEvents: []string{"connected", "disconnected"}, wantReports: []bool{true, false},
		},
		{
			name:      "the same state with a newer since sends one event",
			events:    []Event{beat(1, new(true), &since), beat(2, new(true), new(h10.Add(7*time.Second)))},
			wantState: ws.IcecastConnected, wantSince: h10.Add(7 * time.Second),
			wantEvents: []string{"connected", "connected"}, wantReports: []bool{true, true},
		},
		{name: "a heartbeat without the fields changes nothing", events: []Event{beat(1, nil, nil)}},
		{
			name:      "without output_since: the heartbeat time, kept while the state holds",
			events:    []Event{beat(1, new(false), nil), beat(2, new(false), nil)},
			wantState: ws.IcecastDisconnected, wantSince: h10.Add(5 * time.Second),
			wantEvents: []string{"disconnected"}, wantReports: []bool{false, false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			for _, e := range tt.events {
				require.NoError(t, h.svc.Callback(t.Context(), e))
			}
			st, err := h.svc.Status(t.Context())
			require.NoError(t, err)
			if tt.wantState == "" {
				require.Nil(t, st.Output)
			} else {
				require.Equal(t, Output{State: tt.wantState, Since: tt.wantSince}, *st.Output)
			}
			require.Equal(t, tt.wantEvents, icecastStates(h))
			require.Equal(t, tt.wantReports, h.out.reports)
			for i, at := range h.out.ats {
				require.Equal(t, tt.events[i].Time, at, "the broadcast state gets the heartbeat time")
			}
		})
	}
}

func TestOutputExpire(t *testing.T) {
	t.Parallel()
	const timeout = 10 * time.Second
	connected := Event{Kind: EventHeartbeat, OutputConnected: new(true), OutputSince: new(h10.Add(-time.Hour))}
	tests := []struct {
		name       string
		first      *Event // nil: no report before the silence
		wait       time.Duration
		linkDown   bool
		after      bool // a heartbeat with the state after the silence
		wantState  string
		wantEvents []string
	}{
		{
			name: "silent past the timeout: unknown", first: &connected, wait: timeout + time.Second,
			wantState: ws.IcecastUnknown, wantEvents: []string{"connected", "unknown"},
		},
		{
			name: "exactly the timeout is not silent", first: &connected, wait: timeout,
			wantState: ws.IcecastConnected, wantEvents: []string{"connected"},
		},
		{
			name: "the link down does not stop the reset", first: &connected, wait: timeout + time.Second, linkDown: true,
			wantState: ws.IcecastUnknown, wantEvents: []string{"connected", "unknown"},
		},
		{name: "nothing before the first heartbeat", wait: timeout + time.Second},
		{
			name: "an output callback alone expires too", wait: timeout + time.Second,
			first:     &Event{Kind: EventOutput, Connected: true},
			wantState: ws.IcecastUnknown, wantEvents: []string{"connected", "unknown"},
		},
		{
			name: "a heartbeat after the reset restores the state", first: &connected, wait: timeout + time.Second, after: true,
			wantState: ws.IcecastConnected, wantEvents: []string{"connected", "unknown", "connected"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.set.HeartbeatTimeout = timeout
			if tt.first != nil {
				e := *tt.first
				e.Time = h.now
				require.NoError(t, h.svc.Callback(t.Context(), e))
			}
			h.now = h.now.Add(tt.wait)
			h.ctl.down = tt.linkDown
			err := h.svc.pass(t.Context())
			if tt.linkDown {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			_ = h.svc.pass(t.Context()) // a second pass sends nothing more
			if tt.after {
				e := connected
				e.Time = h.now
				require.NoError(t, h.svc.Callback(t.Context(), e))
			}
			st, err := h.svc.Status(t.Context())
			require.NoError(t, err)
			if tt.wantState == "" {
				require.Nil(t, st.Output)
			} else {
				require.Equal(t, tt.wantState, st.Output.State)
			}
			if tt.wantState == ws.IcecastUnknown {
				require.Equal(t, h.now, st.Output.Since, "since: when the silence was found")
			}
			require.Equal(t, tt.wantEvents, icecastStates(h))
		})
	}

	// Heartbeats without the fields (a contract 1 script) keep an output
	// callback's state alive.
	h := newHarness(t)
	h.set.HeartbeatTimeout = timeout
	require.NoError(t, h.svc.Callback(t.Context(), Event{Kind: EventOutput, Time: h.now, Connected: true}))
	for range 5 {
		h.now = h.now.Add(5 * time.Second)
		require.NoError(t, h.svc.Callback(t.Context(), Event{Kind: EventHeartbeat, Time: h.now}))
		require.NoError(t, h.svc.pass(t.Context()))
	}
	require.Equal(t, []string{"connected"}, icecastStates(h))
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

	h.store.lists[9] = []Track{{ID: 1, Title: "E1", ArtistName: new("Band"), FilePath: "e/1.mp3"}, {ID: 2, Title: "E2", FilePath: "e/2.mp3"}}
	prev := ""
	for range 10 {
		uri, ok, err := h.svc.EmergencyNext(ctx)
		require.NoError(t, err)
		require.True(t, ok)
		require.NotEqual(t, prev, uri, "never the same track twice in a row")
		prev = uri
	}
	require.Contains(t, []string{
		`annotate:track_id="1",title="E1",artist="Band":/media/e/1.mp3`,
		`annotate:track_id="2",title="E2",artist="":/media/e/2.mp3`,
	}, prev)

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
	require.Equal(t, ws.TrackEndedData{
		TrackID: 1, Source: SourceEmergency, Status: "aired", StartedAt: h10, EndedAt: h10.Add(5 * time.Second),
		ActualDurationMS: 5000,
	}, h.pub.events[1].(ws.TrackEndedEvent).Data)
}

// airSim is a fake Liquidsoap that plays what the pusher queued in hour_0:
// a request starts at max(its push, the end of the one before); with the
// queue empty the emergency source plays, and a pushed item takes over from
// it at once (automation_main is track_sensitive=false).
type airSim struct {
	h       *harness
	lengths map[uint64]time.Duration
	// next is the first request of hour_0 not played yet.
	next int
	// playing is the plan item on air; 0: the emergency source or nothing.
	playing uint64
	until   time.Time
	starts  map[uint64]time.Time
	// emergency are the moments the emergency source was on air.
	emergency []time.Time
}

func (a *airSim) step(t *testing.T, alive bool) {
	t.Helper()
	now := a.h.now
	if !alive {
		a.playing = 0
		return
	}
	if a.playing != 0 && !now.Before(a.until) {
		a.playing = 0
	}
	pushed := a.h.ctl.pushed["hour_0"]
	if a.playing == 0 && a.next < len(pushed) {
		uri := pushed[a.next]
		a.next++
		a.h.ctl.queues["hour_0"] = a.h.ctl.queues["hour_0"][1:]
		var id uint64
		_, err := fmt.Sscanf(uri[strings.Index(uri, `item_id="`)+len(`item_id="`):], "%d", &id)
		require.NoError(t, err)
		a.playing, a.until, a.starts[id] = id, now.Add(a.lengths[id]), now
		require.NoError(t, a.h.svc.Callback(t.Context(), Event{Kind: EventStart, Source: SourcePlan, ItemID: new(id), Time: now}))
	}
	if a.playing == 0 {
		a.emergency = append(a.emergency, now)
	}
}

// TestResyncKeepsHourAligned: Liquidsoap dies at 01:00 and comes back with
// empty queues at 10:00; the rest of the hour, with a one-time insert at
// 30:00, airs at its planned seconds (docs/output-state-and-resync-plan.md
// P2). Before the fix the first item after the restart was pushed at once
// and the whole hour moved early by up to the length of a track.
func TestResyncKeepsHourAligned(t *testing.T) {
	t.Parallel()
	const insert = uint64(100)
	plan := func(lengths ...time.Duration) ([]Item, map[uint64]time.Duration) {
		var items []Item
		byID := map[uint64]time.Duration{}
		at := time.Duration(0)
		for i, l := range lengths {
			id := uint64(i + 1)
			if at == 30*time.Minute {
				id = insert
			}
			it := item(id, at)
			it.OutMS = int(l / time.Millisecond)
			items, byID[id] = append(items, it), l
			at += l
		}
		require.Equal(t, time.Hour, at)
		return items, byID
	}
	threes := func(n int) []time.Duration { return slices.Repeat([]time.Duration{3 * time.Minute}, n) }
	tests := []struct {
		name    string
		lengths []time.Duration
	}{
		{"3-minute items", threes(20)},
		{
			"a 10-minute track planned over the restart",
			slices.Concat(threes(2), []time.Duration{2 * time.Minute, 10 * time.Minute}, threes(4), threes(10)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			items, lengths := plan(tt.lengths...)
			h := newHarness(t, slices.Clone(items)...)
			h.now = h10
			sim := &airSim{h: h, lengths: lengths, starts: map[uint64]time.Time{}}
			down, back := h10.Add(time.Minute), h10.Add(10*time.Minute)
			var nextPass time.Time
			for ; h.now.Before(h10.Add(time.Hour)); h.now = h.now.Add(100 * time.Millisecond) {
				alive := h.now.Before(down) || !h.now.Before(back)
				if h.now.Equal(back) { // a new Liquidsoap: new connection, empty queues
					h.ctl.gen++
					h.ctl.queues, h.ctl.pushed, sim.next = map[string][]int{}, map[string][]string{}, 0
				}
				h.ctl.down = !alive
				hold := h.svc.push.holdUntil
				if !h.now.Before(nextPass) || (!hold.IsZero() && !h.now.Before(hold)) {
					err := h.svc.pass(t.Context())
					if alive {
						require.NoError(t, err)
					}
					nextPass = h.now.Add(passInterval)
				}
				sim.step(t, alive)
			}

			require.Contains(t, sim.starts, insert)
			require.WithinDuration(t, h10.Add(30*time.Minute), sim.starts[insert], time.Second, "the insert airs at its planned second")
			var resumed time.Time
			for id, at := range sim.starts {
				require.WithinDuration(t, items[slices.IndexFunc(items, func(it Item) bool { return it.ID == id })].Start, at, time.Second,
					"item %d airs at its planned start", id)
				if !at.Before(back) && (resumed.IsZero() || at.Before(resumed)) {
					resumed = at
				}
			}
			require.False(t, resumed.IsZero())
			require.LessOrEqual(t, resumed.Sub(back), 10*time.Minute, "the plan resumes within one track")
			// The fake resolves a request at once, so the held item starts
			// holdLead early and the hour ends holdLead early.
			for _, at := range sim.emergency {
				require.True(t, at.Before(resumed) || !at.Before(h10.Add(time.Hour-holdLead)),
					"no emergency fill after the plan resumed (at %s)", at.Sub(h10))
			}
			last := items[len(items)-1]
			require.Contains(t, sim.starts, last.ID)
			require.False(t, sim.starts[last.ID].Add(lengths[last.ID]).After(h10.Add(time.Hour)), "the hour ends on its plan")
		})
	}
}
