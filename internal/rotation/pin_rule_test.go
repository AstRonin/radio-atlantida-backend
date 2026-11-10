package rotation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// The jingles of the rule pin tests have no play rule, so nothing but the
// claim of a pin decides how many of them an hour gets.
const (
	jingleA uint64 = 501
	jingleB uint64 = 502
)

func ruleTracks() []trackSpec {
	var out []trackSpec
	for id := jingleA; id <= 505; id++ {
		out = append(out, trackSpec{id: id, duration: 30 * sec, playlist: jingles})
	}
	return out
}

// ruleLibrary is a music library with five jingles, in ID order.
func ruleLibrary() *Library { return library(append(songs(1, 200, 4*mn, music), ruleTracks()...)...) }

// ruleInput is an hour of a fill playlist and the jingles with the rule p.
func ruleInput(seed uint64, p clocks.ClockPlaylist) HourInput {
	in := input(ruleLibrary(), []clocks.ClockPlaylist{pl(music, clocks.RuleFill), p})
	in.Seed = seed
	return in
}

// rulePin is the pin of a jingle row at offset, with its claim.
func rulePin(id, track uint64, origin, offset int) Insert {
	return Insert{
		ID: id, HourStart: hour10, TrackID: track, Placement: PlacementFixed, OffsetMS: new(offset), ToleranceMS: mn,
		RulePlaylistID: new(jingles), RuleOriginMS: new(origin),
	}
}

func ruleItems(p HourPlan) []Item {
	return slices.DeleteFunc(slices.Clone(p.Items), func(it Item) bool { return it.Source != SourceRule })
}

// opened is the builder after everything before the fill.
func opened(in HourInput) *builder {
	b := newBuilder(in)
	b.opening()
	return b
}

// A claim removes the slot whose window holds the origin. The others keep
// their windows, as their random draws are still made and the pin takes no
// free time from them.
func TestRulePinSpreadKeepsTheOtherSlots(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 30; seed++ {
		in := ruleInput(seed, pl(jingles, clocks.RuleSpread, 4))
		base := opened(in).softs
		require.Len(t, base, 4)
		for k, slot := range base {
			in.HourInserts = []Insert{rulePin(1, jingleA, slot.target, 7*mn)}
			want := slices.Delete(slices.Clone(base), k, k+1)
			require.Equal(t, want, opened(in).softs, "seed %d: slot %d is the one removed", seed, k)
		}
	}
}

// An origin outside every window claims the nearest slot.
func TestNearestSlot(t *testing.T) {
	t.Parallel()
	slots := []soft{{earliest: 10, latest: 20}, {earliest: 40, latest: 50}, {earliest: 70, latest: 80}}
	for origin, want := range map[int]int{0: 0, 15: 0, 25: 0, 30: 0, 31: 1, 45: 1, 60: 1, 61: 2, 99: 2} {
		require.Equal(t, want, nearestSlot(slots, origin), "origin %d", origin)
	}
}

// The draw of a claimed slot is still made, so the draws after it are the
// same as without the pin, for every placement rule.
func TestRulePinKeepsTheDrawsOfLaterPlaylists(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		p      clocks.ClockPlaylist
		origin int
	}{
		{"spread", pl(jingles, clocks.RuleSpread, 3), 25 * mn},
		{"middle", pl(jingles, clocks.RuleMiddle), 30 * mn},
		{"start", pl(jingles, clocks.RuleStart), 0},
		{"end", pl(jingles, clocks.RuleEnd), 59 * mn},
		{"start_end, the start", pl(jingles, clocks.RuleStartEnd), 0},
		{"start_end, the end", pl(jingles, clocks.RuleStartEnd), 59 * mn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(1); seed <= 20; seed++ {
				in := ruleInput(seed, tt.p)
				base := opened(in).rng.Uint64()
				in.HourInserts = []Insert{rulePin(1, jingleA, tt.origin, 20*mn)}
				require.Equal(t, base, opened(in).rng.Uint64(), "seed %d", seed)
			}
		})
	}
}

// pinRow is a row of the plan a test pins.
type pinRow func(t *testing.T, rows []Item) Item

func firstRow(t *testing.T, rows []Item) Item {
	t.Helper()
	require.NotEmpty(t, rows)
	return rows[0]
}

func lastRow(t *testing.T, rows []Item) Item {
	t.Helper()
	require.NotEmpty(t, rows)
	return rows[len(rows)-1]
}

// The hour has as many rule tracks with the pin as before it, for each
// placement rule and for both ends of start_end, and the claim removes the
// slot of its own end.
func TestRulePinKeepsTheNumberOfRuleTracks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		p                  clocks.ClockPlaylist
		row                pinRow
		wantStart, wantEnd bool
	}{
		{"spread", pl(jingles, clocks.RuleSpread, 3), firstRow, false, false},
		{"middle", pl(jingles, clocks.RuleMiddle), firstRow, false, false},
		{"start", pl(jingles, clocks.RuleStart), firstRow, false, false},
		{"end", pl(jingles, clocks.RuleEnd), lastRow, false, false},
		{"start_end, the start", pl(jingles, clocks.RuleStartEnd), firstRow, false, true},
		{"start_end, the end", pl(jingles, clocks.RuleStartEnd), lastRow, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(1); seed <= 30; seed++ {
				in := ruleInput(seed, tt.p)
				before := Build(in)
				rows := ruleItems(before)
				row := tt.row(t, rows)

				in.HourInserts = []Insert{rulePin(1, row.TrackID, row.OffsetMS, 20*mn)}
				after := Build(in)
				requireTimeline(t, after)
				require.Empty(t, after.Refused, "seed %d", seed)
				require.Len(t, ruleItems(after), len(rows)-1, "seed %d: the pin took over one slot", seed)
				pin := slices.IndexFunc(after.Items, func(it Item) bool { return it.HourInsertID != nil })
				require.GreaterOrEqual(t, pin, 0, "seed %d: the pin is placed", seed)
				require.LessOrEqual(t, abs(after.Items[pin].OffsetMS-20*mn), mn, "seed %d", seed)

				left := ruleItems(after)
				hasStart := slices.ContainsFunc(left, func(it Item) bool { return it.OffsetMS == 0 })
				hasEnd := slices.ContainsFunc(left, func(it Item) bool { return end(it) == clocks.HourMS })
				require.Equal(t, tt.wantStart, hasStart, "seed %d: the start slot", seed)
				require.Equal(t, tt.wantEnd, hasEnd, "seed %d: the end slot", seed)
			}
		})
	}
}

// A one-time insert the manager adds, of a track of a rule playlist, has no
// claim, so every slot of the rule stays.
func TestManualInsertOfARuleTrackConsumesNoSlot(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 30; seed++ {
		in := ruleInput(seed, pl(jingles, clocks.RuleSpread, 3))
		want := len(ruleItems(Build(in)))
		in.HourInserts = []Insert{{ID: 1, HourStart: hour10, TrackID: jingleA, Placement: PlacementFixed, OffsetMS: new(20 * mn), ToleranceMS: mn}}
		require.Len(t, ruleItems(Build(in)), want, "seed %d", seed)
	}
}

// A refused claiming pin gives its slot back in the same build: the plan is
// the plan of the hour without the pin, and the refusal is reported.
func TestRefusedRulePinRestoresTheSlot(t *testing.T) {
	t.Parallel()
	blocked := func(in *HourInput) {
		in.History = []Play{{TrackID: 1, Start: hour10.Add(-time.Minute)}}
	}
	chained := func(in *HourInput) {
		in.Clock.Inserts = []clocks.Insert{trackInsert(1, 5*mn, 0, 2)}
	}
	tests := []struct {
		name   string
		setup  func(*HourInput)
		track  uint64
		offset int
		reason RefusalReason
	}{
		{"refused when its track is resolved", blocked, 1, 40 * mn, ReasonBlocked},
		{"refused late, by the chain before it", chained, jingleA, 5*mn + 30*sec, ReasonDoesNotFit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(1); seed <= 30; seed++ {
				in := ruleInput(seed, pl(jingles, clocks.RuleSpread, 3))
				tt.setup(&in)
				without := Build(in)
				require.NotEmpty(t, ruleItems(without), "seed %d", seed)

				pin := rulePin(9, tt.track, 25*mn, tt.offset)
				pin.ToleranceMS = 0
				in.HourInserts = []Insert{pin}
				got := Build(in)
				require.Contains(t, got.Refused, uint64(9), "seed %d", seed)
				require.Equal(t, tt.reason, got.Refused[9].Reason, "seed %d", seed)
				require.Equal(t, without.Items, got.Items, "seed %d: the slot is back", seed)
			}
		})
	}
}

// Every refused insert of every attempt is reported, and a refused insert
// with no claim is not placed by the attempt after it.
func TestRefusedRulePinMergesTheRefusals(t *testing.T) {
	t.Parallel()
	in := ruleInput(5, pl(jingles, clocks.RuleSpread, 3))
	in.Clock.Inserts = []clocks.Insert{trackInsert(1, 5*mn, 0, 2)}
	pin := rulePin(9, jingleA, 25*mn, 5*mn+30*sec)
	pin.ToleranceMS = 0
	in.HourInserts = []Insert{
		pin,
		{ID: 10, HourStart: hour10, TrackID: 9999, Placement: PlacementFixed, OffsetMS: new(40 * mn), ToleranceMS: mn},
	}
	before := slices.Clone(in.HourInserts)
	got := Build(in)
	require.Contains(t, got.Refused, uint64(9))
	require.Contains(t, got.Refused, uint64(10))
	require.False(t, slices.ContainsFunc(got.Items, func(it Item) bool { return it.HourInsertID != nil }))
	require.Equal(t, before, in.HourInserts, "the input is not changed")
}

// Rule draws avoid the track of a claiming pin, unless it is the only one.
func TestRuleDrawsAvoidTheClaimingPin(t *testing.T) {
	t.Parallel()
	two := []trackSpec{{id: jingleA, duration: 30 * sec, playlist: jingles}, {id: jingleB, duration: 30 * sec, playlist: jingles}}
	tests := []struct {
		name string
		lib  []trackSpec
		p    clocks.ClockPlaylist
		want uint64
	}{
		{"spread", two, pl(jingles, clocks.RuleSpread, 2), jingleB},
		{"start_end, the other end", two, pl(jingles, clocks.RuleStartEnd), jingleB},
		{"the only allowed track", two[:1], pl(jingles, clocks.RuleSpread, 2), jingleA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(1); seed <= 30; seed++ {
				in := input(library(append(songs(1, 200, 4*mn, music), tt.lib...)...),
					[]clocks.ClockPlaylist{pl(music, clocks.RuleFill), tt.p})
				in.Seed = seed
				in.HourInserts = []Insert{rulePin(1, jingleA, 0, 20*mn)}
				rows := ruleItems(Build(in))
				require.Len(t, rows, 1, "seed %d", seed)
				require.Equal(t, tt.want, rows[0].TrackID, "seed %d", seed)
			}
		})
	}
}

// A claim that finds no slot claims nothing: the slots of the hour stay,
// and a warning says why the hour has one track more.
func TestStaleClaimWarns(t *testing.T) {
	t.Parallel()
	left := rulePin(1, jingleA, 0, 20*mn)
	left.RulePlaylistID = new(extra)
	deleted := rulePin(1, jingleA, 0, 20*mn)
	deleted.RulePlaylistID = nil
	tests := []struct {
		name  string
		p     clocks.ClockPlaylist
		pins  []Insert
		stale []uint64
		rows  int
	}{
		{"the playlist left the clock", pl(jingles, clocks.RuleMiddle), []Insert{left}, []uint64{1}, 1},
		{"the playlist was deleted", pl(jingles, clocks.RuleMiddle), []Insert{deleted}, []uint64{1}, 1},
		{"the playlist is a fill playlist now", pl(jingles, clocks.RuleFill), []Insert{rulePin(1, jingleA, 0, 20*mn)}, []uint64{1}, 0},
		{
			"the middle slot is taken by the older pin", pl(jingles, clocks.RuleMiddle),
			[]Insert{rulePin(1, jingleA, 0, 20*mn), rulePin(2, jingleB, 0, 40*mn)},
			[]uint64{2},
			0,
		},
		{
			"more claims than spread slots", pl(jingles, clocks.RuleSpread, 1),
			[]Insert{rulePin(1, jingleA, 0, 20*mn), rulePin(2, jingleB, 0, 40*mn)},
			[]uint64{2},
			0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := ruleInput(3, tt.p)
			in.HourInserts = tt.pins
			got := Build(in)
			require.Empty(t, got.Refused)
			var stale []uint64
			for _, w := range got.Warnings {
				if w.Code == WarnPinClaimStale {
					stale = append(stale, *w.HourInsertID)
					require.NotEmpty(t, w.Message)
				}
			}
			require.Equal(t, tt.stale, stale)
			require.Len(t, ruleItems(got), tt.rows)
		})
	}
	require.Contains(t, WarningCodes(), WarnPinClaimStale)
}

// The text of an insert in the hash gains nothing without a claim, so the
// hashes stored for the inserts of before the claims stay valid.
func TestInputsHashOfAnInsertWithoutAClaim(t *testing.T) {
	t.Parallel()
	lib := library(trackSpec{id: 1, duration: 3 * mn, playlist: music})
	c := clocks.Clock{ID: 7, UpdatedAt: hour10, Playlists: []clocks.ClockPlaylist{pl(music, clocks.RuleFill)}}
	set := Settings{MinGapMS: 10 * sec}
	ins := Insert{ID: 4, HourStart: hour10, TrackID: 1, Placement: PlacementFixed, OffsetMS: new(20 * mn), ToleranceMS: mn}

	h := sha256.New()
	fmt.Fprintf(h, "clock %d %d\n", c.ID, c.UpdatedAt.UnixNano())
	fmt.Fprintf(h, "library %s\n", lib.clockDigest(c))
	fmt.Fprintf(h, "insert %d track %d missed %s placement %s offset %s tolerance %d\n", 4, 1, "-", PlacementFixed, "1200000", mn)
	writeTrack(h, lib.Tracks[0])
	fmt.Fprintf(h, "settings min_gap %d timezone %s\n", set.MinGapMS, "UTC")
	want := hex.EncodeToString(h.Sum(nil))
	require.Equal(t, want, inputsHash(c, []Insert{ins}, set, "UTC", lib))

	claimed := ins
	claimed.RulePlaylistID, claimed.RuleOriginMS = new(jingles), new(5*mn)
	other := claimed
	other.RuleOriginMS = new(6 * mn)
	withClaim := inputsHash(c, []Insert{claimed}, set, "UTC", lib)
	require.NotEqual(t, want, withClaim, "a claim changes the fingerprint")
	require.NotEqual(t, withClaim, inputsHash(c, []Insert{other}, set, "UTC", lib), "so does its origin")
}

// The same claims give the same plan, whatever order the inserts come in.
func TestBuildWithClaimsIsDeterministic(t *testing.T) {
	t.Parallel()
	for seed := uint64(1); seed <= 20; seed++ {
		in := ruleInput(seed, pl(jingles, clocks.RuleSpread, 4))
		in.HourInserts = []Insert{rulePin(1, jingleA, 8*mn, 20*mn), rulePin(2, jingleB, 8*mn, 40*mn)}
		first := Build(in)
		require.Equal(t, first, Build(in), "seed %d", seed)
		slices.Reverse(in.HourInserts)
		require.Equal(t, first, Build(in), "seed %d: claims go in ID order", seed)
	}
}

// newRulePinTest is a pin test whose clock also plays the jingles by rule.
func newRulePinTest(t *testing.T, p clocks.ClockPlaylist) testService {
	t.Helper()
	ts := newPinTest(t, fullBase)
	ts.store.lib = *library(append(songs(1, 500, 4*mn, music), ruleTracks()...)...)
	c := testClock()
	c.Playlists = append(c.Playlists, p)
	ts.svc.clocks = fakeClocks{c.ID: c}
	return ts
}

func ruleRow(t *testing.T, ts testService, hour time.Time) (uint64, Item) {
	t.Helper()
	ids := ts.store.itemIDs[hour.Unix()]
	for i, it := range ts.store.items[hour.Unix()] {
		if it.Source == SourceRule {
			return ids[i], it
		}
	}
	require.FailNow(t, "the hour has no rule track")
	return 0, Item{}
}

func ruleRowsOf(ts testService, hour time.Time) int {
	n := 0
	for _, it := range ts.store.items[hour.Unix()] {
		if it.Source == SourceRule {
			n++
		}
	}
	return n
}

// The whole life of a rule pin in the service: the pin stores its claim and
// takes one slot, a move keeps the claim, and the unpin brings the slot back.
func TestRulePinClaimMoveAndUnpin(t *testing.T) {
	t.Parallel()
	ts := newRulePinTest(t, pl(jingles, clocks.RuleSpread, 3))
	buildPlan(t, ts)
	itemID, row := ruleRow(t, ts, pinHour)
	before := ruleRowsOf(ts, pinHour)
	require.Equal(t, 3, before)

	_, err := ts.svc.Pin(t.Context(), PinRequest{PlanItemID: itemID, OffsetMS: 20 * mn})
	require.NoError(t, err)
	drainPlanner(t, ts.svc)
	require.Empty(t, ts.ops.failed)
	require.Len(t, ts.store.inserts, 1)
	ins := ts.store.inserts[0]
	require.Equal(t, jingles, *ins.RulePlaylistID)
	require.Equal(t, row.OffsetMS, *ins.RuleOriginMS)
	require.Equal(t, before-1, ruleRowsOf(ts, pinHour), "the pin took over one slot")
	require.Equal(t, row.TrackID, hourInsertItem(t, ts, pinHour, ins.ID).TrackID)

	_, err = ts.svc.MoveInsert(t.Context(), MoveInsertRequest{ID: ins.ID, OffsetMS: 22 * mn})
	require.NoError(t, err)
	drainPlanner(t, ts.svc)
	require.Empty(t, ts.ops.failed)
	moved := ts.store.inserts[0]
	require.Equal(t, 22*mn, *moved.OffsetMS)
	require.Equal(t, ins.RulePlaylistID, moved.RulePlaylistID, "a move keeps the claim")
	require.Equal(t, ins.RuleOriginMS, moved.RuleOriginMS, "and its origin")
	require.Equal(t, before-1, ruleRowsOf(ts, pinHour))

	_, err = ts.svc.RemoveInsert(t.Context(), ins.ID)
	require.NoError(t, err)
	drainPlanner(t, ts.svc)
	require.Empty(t, ts.ops.failed)
	require.Empty(t, ts.store.inserts)
	require.Equal(t, before, ruleRowsOf(ts, pinHour), "the unpin brings the slot back")
}

// A fill pin stores no claim.
func TestFillPinStoresNoClaim(t *testing.T) {
	t.Parallel()
	ts := newRulePinTest(t, pl(jingles, clocks.RuleMiddle))
	buildPlan(t, ts)
	itemID, _ := fillItem(t, ts, pinHour, 0)
	_, err := ts.svc.Pin(t.Context(), PinRequest{PlanItemID: itemID, OffsetMS: 10 * mn})
	require.NoError(t, err)
	drainPlanner(t, ts.svc)
	require.Empty(t, ts.ops.failed)
	require.Nil(t, ts.store.inserts[0].RulePlaylistID)
	require.Nil(t, ts.store.inserts[0].RuleOriginMS)
}

// A template insert stays refused, and so does a rule row with no playlist.
func TestPinRefusesTemplateInsertsAndRuleRowsWithoutPlaylist(t *testing.T) {
	t.Parallel()
	ts := newPinTest(t, fullBase)
	ts.store.planItems[77] = PlanItemRef{ID: 77, HourStart: pinHour, TrackID: 999, Source: SourceRule, Status: planItemPlanned}
	ts.store.planItems[78] = PlanItemRef{
		ID: 78, HourStart: pinHour, TrackID: 999, Source: SourceInsert, Status: planItemPlanned, PlaylistID: new(jingles),
	}
	ts.store.planItems[79] = PlanItemRef{
		ID: 79, HourStart: pinHour, TrackID: 999, Source: SourceRule, Status: "queued", PlaylistID: new(jingles),
	}
	for _, id := range []uint64{77, 78, 79} {
		_, err := ts.svc.Pin(t.Context(), PinRequest{PlanItemID: id, OffsetMS: 30 * mn})
		require.ErrorIs(t, err, ErrNotMovable, "item %d", id)
	}
	require.Empty(t, ts.store.inserts)
}
