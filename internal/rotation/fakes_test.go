package rotation

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// fakeStore is an in-memory Store for the service tests. It keeps the stored
// hours and their items, and counts the calls by method.
type fakeStore struct {
	lib     Library
	hours   map[int64]PlanHour
	items   map[int64][]Item
	inserts []Insert
	queued  []time.Time
	base    []Play
	calls   map[string]int
	deleted []uint64
	cleared []time.Time
	// claims are the nightly claims by station date.
	claims map[string]bool
}

// ClaimNightly takes the claim of a date once, as the repository does.
func (f *fakeStore) ClaimNightly(_ context.Context, day string) (bool, error) {
	f.count("claim")
	if f.claims == nil {
		f.claims = map[string]bool{}
	}
	if f.claims[day] {
		return false, nil
	}
	f.claims[day] = true
	return true, nil
}

func newFakeStore(lib Library) *fakeStore {
	return &fakeStore{lib: lib, hours: map[int64]PlanHour{}, items: map[int64][]Item{}, calls: map[string]int{}}
}

func (f *fakeStore) count(name string) { f.calls[name]++ }

func (f *fakeStore) WithTx(db.Tx) Store { return f }

func (f *fakeStore) LockPlan(context.Context) error { f.count("lock"); return nil }

// Library returns the tracks of the scope, as the repository's query does:
// every track with a duration when All, else the tracks of its playlists and
// the tracks it names.
func (f *fakeStore) Library(_ context.Context, scope LibraryScope) (Library, error) {
	f.count("library")
	out := Library{Rules: f.lib.Rules}
	for _, t := range f.lib.Tracks {
		keep := scope.All || slices.Contains(scope.Tracks, t.ID)
		for _, p := range t.PlaylistIDs {
			keep = keep || slices.Contains(scope.Playlists, p)
		}
		if keep {
			out.Tracks = append(out.Tracks, t)
		}
	}
	return out, nil
}

// TracksOf returns the tracks among ids, with their titles.
func (f *fakeStore) TracksOf(_ context.Context, ids []uint64) ([]Track, error) {
	f.count("tracksOf")
	var out []Track
	for _, t := range f.lib.Tracks {
		if slices.Contains(ids, t.ID) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeStore) History(ctx context.Context, from, to, hourStart time.Time) ([]Play, error) {
	f.count("history")
	all, err := f.Window(ctx, from, to)
	if err != nil {
		return nil, err
	}
	var out []Play
	for _, p := range all {
		if p.Planned && !p.Start.Before(hourStart) && p.Start.Before(hourStart.Add(time.Hour)) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeStore) Window(_ context.Context, from, to time.Time) ([]Play, error) {
	f.count("window")
	var out []Play
	for _, p := range f.base {
		if !p.Start.Before(from) && p.Start.Before(to) {
			out = append(out, p)
		}
	}
	for key, items := range f.items {
		start := time.Unix(key, 0).UTC()
		for _, p := range playsOf(start, items, &f.lib) {
			if !p.Start.Before(from) && p.Start.Before(to) {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func (f *fakeStore) PlanHours(_ context.Context, from, to time.Time) ([]PlanHour, error) {
	f.count("planHours")
	var out []PlanHour
	for _, h := range f.hours {
		if !h.Start.Before(from) && h.Start.Before(to) {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b PlanHour) int { return a.Start.Compare(b.Start) })
	return out, nil
}

func (f *fakeStore) PlanHour(_ context.Context, start time.Time) (PlanHour, bool, error) {
	h, ok := f.hours[start.Unix()]
	return h, ok, nil
}

func (f *fakeStore) ReplaceHour(_ context.Context, h PlanHour, items []Item) error {
	f.count("replace")
	f.hours[h.Start.Unix()] = h
	f.items[h.Start.Unix()] = slices.Clone(items)
	return nil
}

func (f *fakeStore) ClearHour(_ context.Context, start time.Time) error {
	f.count("clear")
	delete(f.hours, start.Unix())
	delete(f.items, start.Unix())
	f.cleared = append(f.cleared, start)
	return nil
}

func (f *fakeStore) Plan(context.Context, time.Time, time.Time) ([]PlannedHour, error) {
	return nil, errors.New("fake: Plan is not used by the service tests")
}

func (f *fakeStore) Missed(context.Context, uint64) (MissedItem, error) {
	return MissedItem{}, errors.New("fake: Missed is not used by the service tests")
}

func (f *fakeStore) LockMissed(context.Context, uint64) (MissedItem, error) {
	return MissedItem{}, errors.New("fake: LockMissed is not used by the service tests")
}

func (f *fakeStore) Inserts(_ context.Context, hourStart time.Time) ([]Insert, error) {
	var out []Insert
	for _, ins := range f.inserts {
		if ins.HourStart.Equal(hourStart) {
			out = append(out, ins)
		}
	}
	return out, nil
}

func (f *fakeStore) InsertsRange(_ context.Context, from, to time.Time) ([]Insert, error) {
	f.count("insertsRange")
	var out []Insert
	for _, ins := range f.inserts {
		if !ins.HourStart.Before(from) && ins.HourStart.Before(to) {
			out = append(out, ins)
		}
	}
	slices.SortStableFunc(out, func(a, b Insert) int { return a.HourStart.Compare(b.HourStart) })
	return out, nil
}

func (f *fakeStore) QueuedHours(_ context.Context, from, to time.Time) ([]time.Time, error) {
	f.count("queued")
	var out []time.Time
	for _, t := range f.queued {
		if !t.Before(from) && t.Before(to) {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeStore) Insert(context.Context, uint64) (Insert, error) {
	return Insert{}, ErrInsertNotFound
}

func (f *fakeStore) CreateInsert(context.Context, Insert) (uint64, error) {
	return 0, errors.New("fake: CreateInsert is not used by the service tests")
}

func (f *fakeStore) DeleteInsert(_ context.Context, id uint64) error {
	f.count("deleteInsert")
	f.deleted = append(f.deleted, id)
	f.inserts = slices.DeleteFunc(f.inserts, func(ins Insert) bool { return ins.ID == id })
	return nil
}

func (f *fakeStore) ReopenMissed(context.Context, uint64) error { return nil }

func (f *fakeStore) LinkRescheduled(context.Context, uint64, time.Time) error { return nil }

// fakeTx runs the transaction without a database; the fake store ignores it.
type fakeTx struct{}

func (fakeTx) WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error {
	return fn(ctx, db.Tx{})
}

// fakeClocks serves the clocks by ID.
type fakeClocks map[uint64]clocks.Clock

func (f fakeClocks) Get(_ context.Context, id uint64) (clocks.Clock, error) {
	c, ok := f[id]
	if !ok {
		return clocks.Clock{}, clocks.ErrNotFound
	}
	return c, nil
}

// fakeGrid assigns clock ID clock to every hour from ... to ..., in time
// order, and records the last range asked for.
type fakeGrid struct {
	clock     uint64
	lastFrom  time.Time
	lastTo    time.Time
	hoursRead int
	// err, if set, fails the read: a run that fails.
	err error
}

func (g *fakeGrid) Hours(_ context.Context, from, to time.Time) ([]grid.ScheduledHour, error) {
	if g.err != nil {
		return nil, g.err
	}
	g.lastFrom, g.lastTo = from, to
	var out []grid.ScheduledHour
	for h := from.Truncate(time.Hour).Add(time.Hour); h.Before(to); h = h.Add(time.Hour) {
		if h.Before(from) {
			continue
		}
		out = append(out, grid.ScheduledHour{StartUTC: h, SlotID: 9, Clock: grid.ClockRef{ID: g.clock, Name: "Day"}})
	}
	g.hoursRead += len(out)
	return out, nil
}

// fakeOps runs the operations inline and records their checkpoints.
type fakeOps struct {
	next        uint64
	checkpoints [][2]int
	failed      []string
}

func (o *fakeOps) Start(context.Context, operations.Kind) (uint64, error) {
	o.next++
	return o.next, nil
}

func (o *fakeOps) Execute(ctx context.Context, _ uint64, _ logging.Component, fn operations.Func) error {
	if _, err := fn(ctx); err != nil {
		o.failed = append(o.failed, err.Error())
	}
	return nil
}

func (o *fakeOps) Checkpoint(_ context.Context, _ operations.Kind, done, total int) error {
	o.checkpoints = append(o.checkpoints, [2]int{done, total})
	return nil
}

// fakePub records the events.
type fakePub struct {
	events []any
}

func (p *fakePub) Publish(event any) { p.events = append(p.events, event) }

// fullEvents returns the rotation.plan_full events in order.
func (p *fakePub) fullEvents() []ws.TrackPlanFullEvent {
	var out []ws.TrackPlanFullEvent
	for _, e := range p.events {
		if ev, ok := e.(ws.TrackPlanFullEvent); ok {
			out = append(out, ev)
		}
	}
	return out
}
