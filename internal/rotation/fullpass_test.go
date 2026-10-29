package rotation

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
)

var fullBase = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)

// fullSettings are the settings of the service tests: two days of grid, a
// batch of 24 hours.
func fullSettings() PlanSettings {
	return PlanSettings{
		Location: time.UTC, PlanHorizonDays: 31, GridHorizonDays: 2, FullPassEnabled: true,
		FullPassTime: "03:00", FullBatchHours: 24, FullSlowMS: 60000, FullHeapWarnMB: 512,
		Freeze: 5 * time.Minute, Build: Settings{MinGapMS: 10 * sec},
	}
}

func testClock() clocks.Clock {
	return clocks.Clock{
		ID: 7, Name: "Day", UpdatedAt: fullBase.Add(-24 * time.Hour),
		Playlists: []clocks.ClockPlaylist{pl(music, clocks.RuleFill)},
	}
}

// testService wires a service to the fakes. now is read on each call.
type testService struct {
	svc   *Service
	store *fakeStore
	grid  *fakeGrid
	ops   *fakeOps
	pub   *fakePub
}

func newTestService(t *testing.T, set PlanSettings, now func() time.Time, log *slog.Logger) testService {
	t.Helper()
	// Track 999 is a jingle no clock plays: a one-time insert of it is never
	// refused by the rotation rule of the music.
	store := newFakeStore(*library(append(songs(1, 60, 4*mn, music), trackSpec{id: 999, duration: 60 * sec, playlist: 2})...))
	g := &fakeGrid{clock: 7}
	ops := &fakeOps{}
	pub := &fakePub{}
	clk := fakeClocks{7: testClock()}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	svc := NewService(fakeTx{}, store, clk, g, func(context.Context) (PlanSettings, error) { return set, nil },
		ops, pub, log, now)
	return testService{svc: svc, store: store, grid: g, ops: ops, pub: pub}
}

func fixedNow(at time.Time) func() time.Time { return func() time.Time { return at } }

// The skip rules of the full pass, one case per row (decideHour).
func TestDecideHour(t *testing.T) {
	t.Parallel()
	now := fullBase
	clock := testClock()
	start := now.Add(10 * time.Hour)
	stored := func(hash string) *PlanHour {
		return &PlanHour{Start: start, ClockID: 7, ClockUpdatedAt: clock.UpdatedAt, InputsHash: hash}
	}
	for _, tc := range []struct {
		name   string
		facts  hourFacts
		build  bool
		reason SkipReason
	}{
		{
			name:  "missing hour in the future is built",
			facts: hourFacts{Start: start, Now: now, Freeze: 5 * time.Minute, Clock: &clock, InputsHash: "h1"},
			build: true,
		},
		{
			name:   "an hour that started is skipped",
			facts:  hourFacts{Start: now, Now: now, Freeze: 5 * time.Minute, Clock: &clock, InputsHash: "h1"},
			reason: SkipStarted,
		},
		{
			name:   "a built hour inside the freeze is skipped",
			facts:  hourFacts{Start: now.Add(3 * time.Minute), Now: now, Freeze: 5 * time.Minute, Clock: &clock, Stored: stored("h1"), InputsHash: "h1"},
			reason: SkipStarted,
		},
		{
			name:  "a missing hour inside the freeze is built, as the planner does",
			facts: hourFacts{Start: now.Add(3 * time.Minute), Now: now, Freeze: 5 * time.Minute, Clock: &clock, InputsHash: "h1"},
			build: true,
		},
		{
			name:   "no clock",
			facts:  hourFacts{Start: start, Now: now, Freeze: 5 * time.Minute, Clock: nil, Stored: stored("h1"), InputsHash: "h1"},
			reason: SkipNoClock,
		},
		{
			name:   "an item in the queue",
			facts:  hourFacts{Start: start, Now: now, Freeze: 5 * time.Minute, Clock: &clock, Stored: stored("h1"), Queued: true, InputsHash: "h1"},
			reason: SkipQueued,
		},
		{
			name:   "same inputs: unchanged",
			facts:  hourFacts{Start: start, Now: now, Freeze: 5 * time.Minute, Clock: &clock, Stored: stored("h1"), InputsHash: "h1"},
			reason: SkipUnchanged,
		},
		{
			name:  "changed inputs: built",
			facts: hourFacts{Start: start, Now: now, Freeze: 5 * time.Minute, Clock: &clock, Stored: stored("h1"), InputsHash: "h2"},
			build: true,
		},
		{
			name:  "built before migration 0016 (no fingerprint): built once",
			facts: hourFacts{Start: start, Now: now, Freeze: 5 * time.Minute, Clock: &clock, Stored: stored(""), InputsHash: "h1"},
			build: true,
		},
	} {
		build, reason := decideHour(tc.facts)
		require.Equal(t, tc.build, build, tc.name)
		require.Equal(t, tc.reason, reason, tc.name)
	}
}

// The fingerprint of an hour changes with each of its inputs: the clock, its
// one-time inserts, the build settings, and the tracks of the clock's playlists.
// A track of another playlist does not change it.
func TestInputsHashCoversItsInputs(t *testing.T) {
	t.Parallel()
	c := testClock()
	specs := append(songs(1, 3, 4*mn, music), trackSpec{id: 9, duration: 4 * mn, playlist: 2})
	lib := library(specs...)
	ins := []Insert{{ID: 1, TrackID: 2, Placement: PlacementFixed, OffsetMS: new(1800000), ToleranceMS: 60000}}
	base := inputsHash(c, ins, Settings{MinGapMS: 10 * sec}, "UTC", lib)
	require.Equal(t, base, inputsHash(c, ins, Settings{MinGapMS: 10 * sec}, "UTC", lib), "same inputs")

	later := c
	later.UpdatedAt = c.UpdatedAt.Add(time.Second)
	require.NotEqual(t, base, inputsHash(later, ins, Settings{MinGapMS: 10 * sec}, "UTC", lib), "clock version")
	require.NotEqual(t, base, inputsHash(c, nil, Settings{MinGapMS: 10 * sec}, "UTC", lib), "one-time inserts")
	require.NotEqual(t, base, inputsHash(c, ins, Settings{MinGapMS: 20 * sec}, "UTC", lib), "build settings")
	require.NotEqual(t, base, inputsHash(c, ins, Settings{MinGapMS: 10 * sec}, "Asia/Yerevan", lib), "timezone")

	longer := library(append(songs(1, 3, 4*mn, music), trackSpec{id: 9, duration: 4 * mn, playlist: 2})...)
	longer.Tracks[0].DurationMS += sec
	require.NotEqual(t, base, inputsHash(c, ins, Settings{MinGapMS: 10 * sec}, "UTC", longer), "a track of the clock's playlist")

	other := library(append(songs(1, 3, 4*mn, music), trackSpec{id: 9, duration: 4 * mn, playlist: 2})...)
	other.Tracks[3].DurationMS += sec // track 9, in playlist 2 which the clock does not use
	require.Equal(t, base, inputsHash(c, ins, Settings{MinGapMS: 10 * sec}, "UTC", other), "a track outside the clock is not an input")
}

// Batches are contiguous and cover the range; a built hour the grid no longer
// assigns falls into the batch before it.
func TestBatchRangesCoverTheRange(t *testing.T) {
	t.Parallel()
	from, to := fullBase.Add(time.Nanosecond), fullBase.Add(48*time.Hour)
	var hours []grid.ScheduledHour
	for h := range 47 {
		hours = append(hours, grid.ScheduledHour{StartUTC: fullBase.Add(time.Duration(h+1) * time.Hour)})
	}
	bs := batchRanges(hours, from, to, 24)
	require.Len(t, bs, 2)
	require.Equal(t, from, bs[0].from)
	require.Equal(t, bs[1].from, bs[0].to, "contiguous")
	require.Equal(t, to, bs[1].to)
	require.Equal(t, [2]int{0, 24}, [2]int{bs[0].lo, bs[0].hi})
	require.Equal(t, [2]int{24, 47}, [2]int{bs[1].lo, bs[1].hi})
	require.Len(t, batchRanges(nil, from, to, 24), 1, "no grid hours: one batch for the stored hours")
}

// The window leaves out the planned plays of the hour it builds and takes its
// new items in their place.
func TestWindowReplacesThePlannedPlaysOfAnHour(t *testing.T) {
	t.Parallel()
	lib := library(songs(1, 5, 4*mn, music)...)
	h := fullBase.Add(10 * time.Hour)
	w := newWindow([]Play{
		{TrackID: 1, Start: h.Add(5 * time.Minute), Planned: true},
		{TrackID: 2, Start: h.Add(5 * time.Minute)}, // aired: kept
	})
	require.Len(t, w.forHour(h, 0), 1, "the hour's planned plays are left out, the aired play stays")
	w.setHour(h, playsOf(h, []Item{{TrackID: 3, OffsetMS: 60_000}}, lib))
	// The next hour sees the aired play and the new item, not the old plan.
	plays := w.forHour(h.Add(time.Hour), 2*time.Hour)
	require.Len(t, plays, 2)
	ids := []uint64{plays[0].TrackID, plays[1].TrackID}
	require.ElementsMatch(t, []uint64{2, 3}, ids)
}

// Only one full pass runs at a time, and the request refuses while one runs.
func TestFullPassRefusesWhileRunning(t *testing.T) {
	t.Parallel()
	ts := newTestService(t, fullSettings(), fixedNow(fullBase), nil)
	ts.svc.full.Store(true)
	_, err := ts.svc.FullPass(t.Context(), TriggerNightly)
	require.ErrorIs(t, err, ErrFullPassRunning)
	_, err = ts.svc.RebuildFull(t.Context())
	require.ErrorIs(t, err, ErrFullPassRunning)
}

// A full pass builds the range in batches, keeps the one-time inserts, reads
// the library and the inserts once per batch, and a second pass builds nothing.
func TestFullPassKeepsInsertsAndSkipsUnchangedOnTheSecondPass(t *testing.T) {
	t.Parallel()
	ts := newTestService(t, fullSettings(), fixedNow(fullBase), nil)
	insertHour := fullBase.Add(10 * time.Hour)
	ts.store.inserts = []Insert{{
		ID: 500, HourStart: insertHour, TrackID: 999, Placement: PlacementFixed, OffsetMS: new(1800000), ToleranceMS: 60000,
	}}

	summary, err := ts.svc.FullPass(t.Context(), TriggerNightly)
	require.NoError(t, err)
	require.Contains(t, summary, "built 47 hours")
	require.Len(t, ts.store.hours, 47)
	require.Empty(t, ts.store.deleted, "the one-time insert is kept")
	require.Len(t, ts.store.inserts, 1)
	require.Equal(t, 1, ts.store.calls["library"], "the snapshot is read once per run")
	require.Equal(t, 3, ts.store.calls["insertsRange"], "one per batch and one for the pre-scan")
	require.Equal(t, [][2]int{{24, 47}, {47, 47}}, ts.ops.checkpoints, "progress per batch")
	var placed bool
	for _, it := range ts.store.items[insertHour.Unix()] {
		placed = placed || (it.HourInsertID != nil && *it.HourInsertID == 500)
	}
	require.True(t, placed, "the insert is in its hour")

	replaces := ts.store.calls["replace"]
	again, err := ts.svc.FullPass(t.Context(), TriggerNightly)
	require.NoError(t, err)
	require.Contains(t, again, "built 0 hours, skipped 47")
	require.Equal(t, replaces, ts.store.calls["replace"], "nothing was written")
	events := ts.pub.fullEvents()
	require.Len(t, events, 4, "started and completed, twice")
	require.Equal(t, "started", events[2].Data.Status)
	require.Equal(t, 0, events[3].Data.HoursBuilt)
	require.Equal(t, 47, events[3].Data.HoursSkipped)
}

// A changed insert rebuilds its hour only.
func TestFullPassRebuildsOnlyTheChangedHour(t *testing.T) {
	t.Parallel()
	ts := newTestService(t, fullSettings(), fixedNow(fullBase), nil)
	insertHour := fullBase.Add(10 * time.Hour)
	ts.store.inserts = []Insert{{
		ID: 500, HourStart: insertHour, TrackID: 999, Placement: PlacementFixed, OffsetMS: new(1800000), ToleranceMS: 60000,
	}}
	_, err := ts.svc.FullPass(t.Context(), TriggerNightly)
	require.NoError(t, err)

	ts.store.inserts = nil // the manager removed the insert
	summary, err := ts.svc.FullPass(t.Context(), TriggerButton)
	require.NoError(t, err)
	require.Contains(t, summary, "built 1 hours, skipped 46")
}

// The start line counts the hours to build and to skip, and the end line has
// duration, counts, and memory; a slow run and a heap above the threshold are
// warned.
func TestFullPassLogsDurationAndMemory(t *testing.T) {
	t.Parallel()
	set := fullSettings()
	set.FullSlowMS = 1000
	set.FullHeapWarnMB = 1
	var lines capturedLines
	log := slog.New(lines.handler())
	cur := fullBase
	now := func() time.Time { // two seconds per reading: the run is slow
		cur = cur.Add(2 * time.Second)
		return cur
	}
	ts := newTestService(t, set, now, log)

	_, err := ts.svc.FullPass(t.Context(), TriggerNightly)
	require.NoError(t, err)

	// The clock of this test moves on each reading, so the grid holds one
	// hour more than in the other tests; the counts follow the grid read.
	hours := ts.grid.hoursRead
	start := lines.find(t, "track plan pass started")
	require.EqualValues(t, hours, start["hours_to_build"])
	require.EqualValues(t, 0, start["hours_to_skip"])
	require.Equal(t, "full", start["pass"])
	require.Equal(t, "nightly", start["trigger"])

	end := lines.find(t, "track plan pass finished")
	require.Positive(t, end["duration_ms"])
	require.EqualValues(t, hours, end["hours_built"])
	require.EqualValues(t, 2, end["batches"])
	require.Positive(t, end["queries"])
	require.Contains(t, end, "warnings_by_code")
	require.Contains(t, end, "heap_alloc_mb")
	require.Contains(t, end, "heap_sys_mb")
	require.Contains(t, end, "num_gc")
	require.Contains(t, end, "num_gc_delta")
	require.NotContains(t, end, "total_alloc_mb", "the cumulative figure is not logged")
	delta, ok := end["total_alloc_delta_mb"].(float64)
	require.True(t, ok, "the allocation of the run is a delta")
	require.GreaterOrEqual(t, delta, 0.0)
	batch := lines.find(t, "track plan batch finished")
	require.Contains(t, batch, "total_alloc_delta_mb")
	require.NotContains(t, batch, "total_alloc_mb")

	lines.find(t, "track plan pass slow")
	lines.find(t, "track plan heap above threshold")
	require.Equal(t, 2, lines.count(t, "track plan batch finished"))
}

// The nightly claim: once per station day, from the configured time, only
// when enabled, kept when the run fails, and not taken by a button pass.
func TestNightlyDue(t *testing.T) {
	t.Parallel()
	at := func(day, hhmm string) time.Time {
		v, err := time.Parse("2006-01-02 15:04", day+" "+hhmm)
		require.NoError(t, err)
		return v.UTC()
	}
	for _, tc := range []struct {
		name  string
		steps func(t *testing.T, ts *testService, cur *time.Time)
	}{
		{"before the time: not due, no claim", func(t *testing.T, ts *testService, cur *time.Time) {
			*cur = at("2026-10-15", "02:59")
			require.False(t, nightly(t, ts))
			require.False(t, ts.store.claims["2026-10-15"])
		}},
		{"after the time with no claim: due, claimed", func(t *testing.T, ts *testService, cur *time.Time) {
			*cur = at("2026-10-15", "03:00")
			require.True(t, nightly(t, ts))
			require.True(t, ts.store.claims["2026-10-15"])
		}},
		{"after the time with a claim: not due", func(t *testing.T, ts *testService, cur *time.Time) {
			*cur = at("2026-10-15", "03:00")
			require.True(t, nightly(t, ts))
			*cur = at("2026-10-15", "09:00")
			require.False(t, nightly(t, ts))
		}},
		{"a failed run keeps its claim", func(t *testing.T, ts *testService, cur *time.Time) {
			*cur = at("2026-10-15", "03:00")
			require.True(t, nightly(t, ts))
			ts.grid.err = errors.New("grid unavailable")
			_, err := ts.svc.RunNightly(t.Context())
			require.Error(t, err)
			*cur = at("2026-10-15", "03:30")
			require.False(t, nightly(t, ts), "the failed night is not retried the same day")
		}},
		{"a button pass takes no claim", func(t *testing.T, ts *testService, cur *time.Time) {
			*cur = at("2026-10-15", "10:00")
			_, err := ts.svc.FullPass(t.Context(), TriggerButton)
			require.NoError(t, err)
			require.False(t, ts.store.claims["2026-10-15"])
			*cur = at("2026-10-15", "03:00")
			require.True(t, nightly(t, ts), "the night's pass is still due")
		}},
		{"the next station day is due again", func(t *testing.T, ts *testService, cur *time.Time) {
			*cur = at("2026-10-15", "03:00")
			require.True(t, nightly(t, ts))
			*cur = at("2026-10-16", "03:01")
			require.True(t, nightly(t, ts))
			require.False(t, nightly(t, ts))
		}},
		{"disabled: not due, no claim", func(t *testing.T, ts *testService, cur *time.Time) {
			ts.svc.settings = func(context.Context) (PlanSettings, error) {
				set := fullSettings()
				set.FullPassEnabled = false
				return set, nil
			}
			*cur = at("2026-10-15", "04:00")
			require.False(t, nightly(t, ts))
			require.Empty(t, ts.store.claims)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cur := fullBase
			ts := newTestService(t, fullSettings(), func() time.Time { return cur }, nil)
			tc.steps(t, &ts, &cur)
		})
	}
}

// nightly asks NightlyDue and fails the test on an error.
func nightly(t *testing.T, ts *testService) bool {
	t.Helper()
	ok, err := ts.svc.NightlyDue(t.Context())
	require.NoError(t, err)
	return ok
}

// The 5-minute pass covers rotation.plan_horizon_days, from now.
func TestShortPassHorizonIsInDays(t *testing.T) {
	t.Parallel()
	set := fullSettings()
	set.PlanHorizonDays = 31
	ts := newTestService(t, set, fixedNow(fullBase), nil)
	_, err := ts.svc.RunJob(t.Context())
	require.NoError(t, err)
	require.Equal(t, fullBase.Add(time.Nanosecond), ts.grid.lastFrom)
	require.Equal(t, fullBase.Add(31*24*time.Hour), ts.grid.lastTo)
}

// The rebuild by range takes at most MaxPlanDays (31) days.
func TestRebuildRangeLimitIs31Days(t *testing.T) {
	t.Parallel()
	ts := newTestService(t, fullSettings(), fixedNow(fullBase), nil)
	ok := Range{From: fullBase, To: fullBase.AddDate(0, 0, 31)}
	_, err := ts.svc.RebuildRange(t.Context(), ok)
	require.NoError(t, err)
	tooLong := Range{From: fullBase, To: fullBase.AddDate(0, 0, 32)}
	_, err = ts.svc.RebuildRange(t.Context(), tooLong)
	require.Error(t, err)
	require.Equal(t, 31, MaxPlanDays)
}

// capturedLines records the log lines of a logger as attribute maps.
type capturedLines struct {
	mu    sync.Mutex
	lines []capturedLine
}

type capturedLine struct {
	msg   string
	level slog.Level
	attrs map[string]any
}

type capturingHandler struct {
	into *capturedLines
	pre  []slog.Attr
}

func (lines *capturedLines) handler() slog.Handler { return capturingHandler{into: lines} }

func (h capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h capturingHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	for _, a := range h.pre {
		attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.into.mu.Lock()
	defer h.into.mu.Unlock()
	h.into.lines = append(h.into.lines, capturedLine{msg: r.Message, level: r.Level, attrs: attrs})
	return nil
}

func (h capturingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return capturingHandler{into: h.into, pre: append(slices.Clip(h.pre), attrs...)}
}

func (h capturingHandler) WithGroup(string) slog.Handler { return h }

// find returns the attributes of the first line with msg, and fails if none.
func (lines *capturedLines) find(t *testing.T, msg string) map[string]any {
	t.Helper()
	lines.mu.Lock()
	defer lines.mu.Unlock()
	for _, l := range lines.lines {
		if l.msg == msg {
			return l.attrs
		}
	}
	require.Failf(t, "no log line", "%q", msg)
	return nil
}

func (lines *capturedLines) count(t *testing.T, msg string) int {
	t.Helper()
	lines.mu.Lock()
	defer lines.mu.Unlock()
	n := 0
	for _, l := range lines.lines {
		if l.msg == msg {
			n++
		}
	}
	return n
}
