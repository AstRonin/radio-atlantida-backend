package rotation

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// pinHour is the hour the pin tests build and pin: 10:00 UTC, after the
// freeze of the test clock (fullBase is its start).
var pinHour = fullBase.Add(10 * time.Hour)

// pinSettings are the settings of the pin tests: one day of plan, so the
// test builds a few hours.
func pinSettings() PlanSettings {
	set := fullSettings()
	set.PlanHorizonDays = 1
	set.DefaultFixedToleranceMS = mn
	return set
}

// newPinTest wires the service to the fakes with a library large enough for
// a day of plan: every track plays once per 72 h, and the day plays about
// 300 tracks.
func newPinTest(t *testing.T, now time.Time) testService {
	t.Helper()
	ts := newTestService(t, pinSettings(), fixedNow(now), nil)
	ts.store.lib = *library(append(songs(1, 500, 4*mn, music), trackSpec{id: 999, duration: 60 * sec, playlist: jingles})...)
	return ts
}

// rollbackTx runs fn and restores the fake store when fn fails, as a rolled
// back transaction does.
type rollbackTx struct{ store *fakeStore }

func (r rollbackTx) WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error {
	snap := r.store.snapshot()
	if err := fn(ctx, db.Tx{}); err != nil {
		r.store.restore(snap)
		return err
	}
	return nil
}

type storeSnapshot struct {
	inserts   []Insert
	hours     map[int64]PlanHour
	items     map[int64][]Item
	planItems map[uint64]PlanItemRef
	itemIDs   map[int64][]uint64
}

func (f *fakeStore) snapshot() storeSnapshot {
	items := make(map[int64][]Item, len(f.items))
	for k, v := range f.items {
		items[k] = slices.Clone(v)
	}
	return storeSnapshot{
		inserts: slices.Clone(f.inserts), hours: maps.Clone(f.hours), items: items,
		planItems: maps.Clone(f.planItems), itemIDs: maps.Clone(f.itemIDs),
	}
}

func (f *fakeStore) restore(s storeSnapshot) {
	f.inserts, f.hours, f.items, f.planItems, f.itemIDs = s.inserts, s.hours, s.items, s.planItems, s.itemIDs
}

// drainPlanner runs the queued operations as the planner does, until the
// queue is empty.
func drainPlanner(t *testing.T, svc *Service) {
	t.Helper()
	for {
		select {
		case r := <-svc.queue:
			svc.runBatch(t.Context(), append([]rebuild{r}, svc.drain()...))
		default:
			return
		}
	}
}

// fillItem returns the ID and the item of the first fill track of the hour
// that starts at offset minOffsetMS or later.
func fillItem(t *testing.T, ts testService, hour time.Time, minOffsetMS int) (uint64, Item) {
	t.Helper()
	ids := ts.store.itemIDs[hour.Unix()]
	for i, it := range ts.store.items[hour.Unix()] {
		if it.Source == SourceFill && it.OffsetMS >= minOffsetMS {
			return ids[i], it
		}
	}
	require.FailNow(t, "the hour has no fill track after the offset")
	return 0, Item{}
}

// hourInsertItem returns the item of the one-time insert ID in the hour.
func hourInsertItem(t *testing.T, ts testService, hour time.Time, insertID uint64) Item {
	t.Helper()
	for _, it := range ts.store.items[hour.Unix()] {
		if it.HourInsertID != nil && *it.HourInsertID == insertID {
			return it
		}
	}
	require.FailNow(t, "the one-time insert is not placed in the hour")
	return Item{}
}

// buildPlan builds the test plan of the day, at the test clock.
func buildPlan(t *testing.T, ts testService) {
	t.Helper()
	_, err := ts.svc.RunJob(t.Context())
	require.NoError(t, err)
	require.Contains(t, ts.store.hours, pinHour.Unix(), "the hour is built")
}

func TestPinCreatesAOneTimeInsertWithTheFillTrack(t *testing.T) {
	t.Parallel()
	ts := newPinTest(t, fullBase)
	buildPlan(t, ts)
	itemID, fill := fillItem(t, ts, pinHour, 0)

	_, err := ts.svc.Pin(t.Context(), PinRequest{PlanItemID: itemID, OffsetMS: 30 * mn})
	require.NoError(t, err)
	drainPlanner(t, ts.svc)
	require.Empty(t, ts.ops.failed)

	require.Len(t, ts.store.inserts, 1)
	ins := ts.store.inserts[0]
	require.Equal(t, fill.TrackID, ins.TrackID, "the pin keeps the track of the fill item")
	require.Equal(t, PlacementFixed, ins.Placement)
	require.Equal(t, 30*mn, *ins.OffsetMS)
	require.Equal(t, mn, ins.ToleranceMS, "the default start tolerance")
	require.True(t, ins.HourStart.Equal(pinHour))

	placed := hourInsertItem(t, ts, pinHour, ins.ID)
	require.Equal(t, fill.TrackID, placed.TrackID)
	require.Equal(t, SourceHourInsert, placed.Source)
	require.LessOrEqual(t, abs(placed.OffsetMS-30*mn), mn, "placed within the tolerance of its offset")
	var plays int
	for _, it := range ts.store.items[pinHour.Unix()] {
		if it.TrackID == fill.TrackID {
			plays++
		}
	}
	require.Equal(t, 1, plays, "the track is not planned again in the same hour (Music rotation)")
}

func TestPinAndMoveRefusedForAFrozenHour(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		now  time.Time
	}{
		{"within the freeze before its start", pinHour.Add(-time.Minute)},
		{"after its start", pinHour.Add(10 * time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := newPinTest(t, fullBase)
			buildPlan(t, ts)
			itemID, _ := fillItem(t, ts, pinHour, 0)
			ts.store.inserts = append(ts.store.inserts, Insert{
				ID: 500, HourStart: pinHour, TrackID: 999, Placement: PlacementFixed, OffsetMS: new(20 * mn), ToleranceMS: mn,
			})
			ts.svc.now = fixedNow(tt.now)

			_, err := ts.svc.Pin(t.Context(), PinRequest{PlanItemID: itemID, OffsetMS: 30 * mn})
			require.ErrorIs(t, err, ErrHourStarted)
			_, err = ts.svc.MoveInsert(t.Context(), MoveInsertRequest{ID: 500, OffsetMS: 30 * mn})
			require.ErrorIs(t, err, ErrHourStarted)
			require.Equal(t, 20*mn, *ts.store.inserts[0].OffsetMS, "the frozen insert is not moved")
			require.Zero(t, ts.store.calls["updateInsert"])
		})
	}
}

func TestPinRefusesAnItemThatIsNotAFillTrack(t *testing.T) {
	t.Parallel()
	ts := newPinTest(t, fullBase)
	buildPlan(t, ts)
	ts.store.planItems[77] = PlanItemRef{ID: 77, HourStart: pinHour, TrackID: 999, Source: SourceRule, Status: planItemPlanned}

	_, err := ts.svc.Pin(t.Context(), PinRequest{PlanItemID: 77, OffsetMS: 30 * mn})
	require.ErrorIs(t, err, ErrNotMovable)
	_, err = ts.svc.Pin(t.Context(), PinRequest{PlanItemID: 999999, OffsetMS: 30 * mn})
	require.ErrorIs(t, err, ErrPlanItemNotFound)

	ts.store.inserts = append(ts.store.inserts, Insert{
		ID: 501, HourStart: pinHour, TrackID: 999, Placement: PlacementStart, ToleranceMS: mn,
	})
	_, err = ts.svc.MoveInsert(t.Context(), MoveInsertRequest{ID: 501, OffsetMS: 30 * mn})
	require.ErrorIs(t, err, ErrNotMovable, "an insert with no fixed time is not moved")
}

// A move the builder cannot place fails the operation with the refusal and
// changes nothing: the insert keeps its offset and the hour is not replaced.
func TestUnplaceableMoveIsRolledBackAndRefused(t *testing.T) {
	t.Parallel()
	ts := newPinTest(t, fullBase)
	ts.svc.tx = rollbackTx{store: ts.store}
	buildPlan(t, ts)

	// A track played in the hour before is blocked for 72 h (Music rotation),
	// so the one-time insert of it cannot be placed in pinHour at any time.
	_, earlier := fillItem(t, ts, pinHour.Add(-time.Hour), 0)
	ts.store.inserts = append(ts.store.inserts, Insert{
		ID: 502, HourStart: pinHour, TrackID: earlier.TrackID, Placement: PlacementFixed, OffsetMS: new(50 * mn), ToleranceMS: mn,
	})
	before := ts.store.snapshot()

	opID, err := ts.svc.MoveInsert(t.Context(), MoveInsertRequest{ID: 502, OffsetMS: 10 * mn})
	require.NoError(t, err, "the checks that need no build pass; the build refuses")
	require.NotZero(t, opID)
	drainPlanner(t, ts.svc)

	require.Len(t, ts.ops.failed, 1)
	require.Contains(t, ts.ops.failed[0], "the insert cannot be placed in that hour")
	require.Contains(t, ts.ops.failed[0], "breaks a play rule")
	require.Equal(t, 50*mn, *ts.store.inserts[0].OffsetMS, "the insert keeps its offset")
	require.Equal(t, before.items, ts.store.items, "the hour is not replaced")
	require.Equal(t, before.hours, ts.store.hours)
	require.Empty(t, ts.store.deleted, "a refused move does not drop the insert")
}

// The same pin gives the same hour on every build: the builder is seeded by
// the hour start, and the pinned track keeps its track and its place.
func TestRebuildOfAPinnedHourIsDeterministic(t *testing.T) {
	t.Parallel()
	ts := newPinTest(t, fullBase)
	buildPlan(t, ts)
	itemID, fill := fillItem(t, ts, pinHour, 0)
	_, err := ts.svc.Pin(t.Context(), PinRequest{PlanItemID: itemID, OffsetMS: 30 * mn})
	require.NoError(t, err)
	drainPlanner(t, ts.svc)
	require.Empty(t, ts.ops.failed)
	pinned := ts.store.inserts[0]

	forced := []Range{{From: pinHour, To: pinHour.Add(time.Hour)}}
	_, err = ts.svc.sync(t.Context(), TriggerClockBuild, forced)
	require.NoError(t, err)
	first := slices.Clone(ts.store.items[pinHour.Unix()])
	_, err = ts.svc.sync(t.Context(), TriggerClockBuild, forced)
	require.NoError(t, err)
	require.Equal(t, first, ts.store.items[pinHour.Unix()], "the same pin gives the same hour")

	require.Equal(t, []Insert{pinned}, ts.store.inserts, "the rebuild keeps the pin")
	placed := hourInsertItem(t, ts, pinHour, pinned.ID)
	require.Equal(t, fill.TrackID, placed.TrackID, "the pinned track is kept on rebuild")
}

// The pin tolerance is in milliseconds: nil takes the default, 0 to 600000
// are accepted and stored as they are, and a wider one is refused per field.
func TestPinToleranceIsValidatedInMilliseconds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		tol   *int
		want  int
		field string
	}{
		{"absent takes the default", nil, mn, ""},
		{"zero", new(0), 0, ""},
		{"the widest allowed", new(600_000), 600_000, ""},
		{"wider than 600000 ms", new(600_001), 0, "tolerance_ms"},
		{"negative", new(-1), 0, "tolerance_ms"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ts := newPinTest(t, fullBase)
			ts.svc.tx = rollbackTx{store: ts.store}
			buildPlan(t, ts)
			itemID, _ := fillItem(t, ts, pinHour, 0)

			_, err := ts.svc.Pin(t.Context(), PinRequest{PlanItemID: itemID, OffsetMS: 30 * mn, ToleranceMS: tt.tol})
			if tt.field != "" {
				var ve *validation.Error
				require.ErrorAs(t, err, &ve)
				require.Equal(t, tt.field, ve.Fields[0].Field, ve.Error())
				require.Empty(t, ts.store.inserts)
				return
			}
			require.NoError(t, err)
			drainPlanner(t, ts.svc)
			require.Empty(t, ts.ops.failed)
			require.Len(t, ts.store.inserts, 1)
			require.Equal(t, tt.want, ts.store.inserts[0].ToleranceMS)
		})
	}
}

// pinnedHour builds one hour of four tracks of 5 min without any play rule,
// with the track 1 fixed at 30:00. Nothing but the pin keeps track 1 out of
// the rest of the hour.
func pinnedHour(seed uint64) HourInput {
	var specs []trackSpec
	for id := uint64(1); id <= 4; id++ {
		specs = append(specs, trackSpec{id: id, duration: 5 * mn, playlist: music})
	}
	in := input(library(specs...), []clocks.ClockPlaylist{pl(music, clocks.RuleFill)})
	in.Seed = seed
	in.HourInserts = []Insert{{ID: 1, HourStart: hour10, TrackID: 1, Placement: PlacementFixed, OffsetMS: new(30 * mn), ToleranceMS: mn}}
	return in
}

// A pinned track is excluded from the rest of its hour even where no play
// rule covers it: it is placed once, at its pin, for every seed.
func TestPinnedTrackIsNotDrawnAgainInItsHour(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 30; seed++ {
		p := Build(pinnedHour(seed))
		requireTimeline(t, p)

		var pinned []Item
		plays := 0
		for _, it := range p.Items {
			if it.TrackID == 1 {
				plays++
				pinned = append(pinned, it)
			}
		}
		require.Equal(t, 1, plays, "seed %d: the pinned track plays once in the hour", seed)
		require.Equal(t, SourceHourInsert, pinned[0].Source, "seed %d", seed)
		require.LessOrEqual(t, abs(pinned[0].OffsetMS-30*mn), mn, "seed %d: placed within its tolerance", seed)
		require.Greater(t, len(p.Items), 2, "seed %d: the rest of the hour is filled", seed)
	}
}

// The same inputs give the same plan on every build, with pins in the hour.
func TestBuildWithPinsIsDeterministic(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 10; seed++ {
		in := pinnedHour(seed)
		in.Library = library(songs(1, 40, 6*mn, music)...)
		in.HourInserts = []Insert{{ID: 2, HourStart: hour10, TrackID: 3, Placement: PlacementFixed, OffsetMS: new(20 * mn), ToleranceMS: mn}}
		first := Build(in)
		require.Equal(t, first, Build(in), "seed %d: the same inputs give the same plan", seed)
	}
}
