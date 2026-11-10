package rotation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
)

// startedSpecs is the library of the partial builds: 4 to 6 minute music, 30 s
// jingles, 45 s extras, 90 s news.
func startedSpecs() []trackSpec {
	specs := songs(1, 40, 4*mn, music)
	specs = append(specs, songs(101, 8, 30*sec, jingles)...)
	specs = append(specs, songs(201, 6, 45*sec, extra)...)
	return append(specs, songs(301, 6, 90*sec, news)...)
}

func itemsOf(p HourPlan, playlist uint64) []Item {
	var out []Item
	for _, it := range p.Items {
		if it.PlaylistID != nil && *it.PlaylistID == playlist {
			out = append(out, it)
		}
	}
	return out
}

func hasTrack(items []Item, id uint64) bool {
	for _, it := range items {
		if it.TrackID == id {
			return true
		}
	}
	return false
}

func TestBuildFromMS(t *testing.T) {
	t.Parallel()
	fill := pl(music, clocks.RuleFill)
	fixedInsert := func(offset int) Insert {
		return Insert{ID: 1, HourStart: hour10, TrackID: 302, Placement: PlacementFixed, OffsetMS: new(offset), ToleranceMS: 60 * sec}
	}
	for _, tc := range []struct {
		name      string
		fromMS    int
		library   *Library
		playlists []clocks.ClockPlaylist
		inserts   []clocks.Insert
		hour      []Insert
		warnings  []WarningCode
		check     func(t *testing.T, p HourPlan)
	}{
		{
			name: "the start rule and the start of start_end are skipped, the end is placed", fromMS: 20 * mn,
			playlists: []clocks.ClockPlaylist{fill, pl(jingles, clocks.RuleStart), pl(extra, clocks.RuleStartEnd)},
			check: func(t *testing.T, p HourPlan) {
				require.Empty(t, itemsOf(p, jingles))
				require.Len(t, itemsOf(p, extra), 1)
				last := p.Items[len(p.Items)-1]
				require.Equal(t, extra, *last.PlaylistID)
				require.Equal(t, clocks.HourMS, end(last))
			},
		},
		{
			name: "a one-time start insert is skipped", fromMS: 20 * mn,
			playlists: []clocks.ClockPlaylist{fill},
			hour:      []Insert{{ID: 1, HourStart: hour10, TrackID: 302, Placement: PlacementStart}},
			check:     func(t *testing.T, p HourPlan) { require.False(t, hasTrack(p.Items, 302)) },
		},
		{
			name: "a middle slot whose window has passed is skipped", fromMS: 40 * mn,
			playlists: []clocks.ClockPlaylist{fill, pl(news, clocks.RuleMiddle)},
			check:     func(t *testing.T, p HourPlan) { require.Empty(t, itemsOf(p, news)) },
		},
		{
			name: "a middle slot whose window holds the start is placed after a fill track", fromMS: 28 * mn,
			playlists: []clocks.ClockPlaylist{fill, pl(news, clocks.RuleMiddle)},
			check: func(t *testing.T, p HourPlan) {
				got := itemsOf(p, news)
				require.Len(t, got, 1)
				require.Greater(t, got[0].OffsetMS, 28*mn)
				require.LessOrEqual(t, got[0].OffsetMS, 35*mn)
			},
		},
		{
			name: "a slot the first fill track runs past gives no warning", fromMS: 34*mn + 30*sec,
			playlists: []clocks.ClockPlaylist{fill, pl(news, clocks.RuleMiddle)},
			check:     func(t *testing.T, p HourPlan) { require.Empty(t, itemsOf(p, news)) },
		},
		{
			name: "a template insert whose window has passed is skipped", fromMS: 20 * mn,
			playlists: []clocks.ClockPlaylist{fill},
			inserts:   []clocks.Insert{trackInsert(1, 5*mn, 60*sec, 301)},
			check:     func(t *testing.T, p HourPlan) { require.False(t, hasTrack(p.Items, 301)) },
		},
		{
			name: "a one-time insert whose window has passed is skipped, not refused", fromMS: 20 * mn,
			playlists: []clocks.ClockPlaylist{fill},
			hour:      []Insert{fixedInsert(5 * mn)},
			check:     func(t *testing.T, p HourPlan) { require.False(t, hasTrack(p.Items, 302)) },
		},
		{
			name: "an insert whose window holds the start is placed there", fromMS: 30*mn + 30*sec,
			playlists: []clocks.ClockPlaylist{fill},
			inserts:   []clocks.Insert{trackInsert(1, 30*mn, 60*sec, 301)},
			check: func(t *testing.T, p HourPlan) {
				require.Equal(t, uint64(301), p.Items[0].TrackID)
				require.Equal(t, 30*mn+30*sec, p.Items[0].OffsetMS)
				require.Equal(t, SourceInsert, p.Items[0].Source)
			},
		},
		{
			name: "an insert at 0:00 whose window holds the start is placed there", fromMS: 30 * sec,
			playlists: []clocks.ClockPlaylist{fill},
			inserts:   []clocks.Insert{trackInsert(1, 0, 60*sec, 301)},
			check: func(t *testing.T, p HourPlan) {
				require.Equal(t, uint64(301), p.Items[0].TrackID)
				require.Equal(t, 30*sec, p.Items[0].OffsetMS)
			},
		},
		{
			name: "a later insert keeps its place", fromMS: 10 * mn,
			playlists: []clocks.ClockPlaylist{fill},
			inserts:   []clocks.Insert{trackInsert(1, 45*mn, 60*sec, 301)},
			check: func(t *testing.T, p HourPlan) {
				require.True(t, hasTrack(p.Items, 301))
				for _, it := range p.Items {
					if it.TrackID == 301 {
						require.InDelta(t, 45*mn, it.OffsetMS, float64(60*sec))
					}
				}
			},
		},
		{
			name: "spread slots before the start are dropped and the count is not raised", fromMS: 40 * mn,
			playlists: []clocks.ClockPlaylist{fill, pl(jingles, clocks.RuleSpread, 4)},
			check: func(t *testing.T, p HourPlan) {
				// Of four parts of 15 minutes, the first two have passed.
				got := itemsOf(p, jingles)
				require.NotEmpty(t, got)
				require.LessOrEqual(t, len(got), 2)
			},
		},
		{
			name: "the last track is cut at the end of the hour", fromMS: 45 * mn,
			playlists: []clocks.ClockPlaylist{fill},
			check: func(t *testing.T, p HourPlan) {
				require.Equal(t, clocks.HourMS, end(p.Items[len(p.Items)-1]))
			},
		},
		{
			name: "no track left is still a warning", fromMS: 20 * mn,
			library:   library(),
			playlists: []clocks.ClockPlaylist{fill},
			warnings:  []WarningCode{WarnEmergency},
			check:     func(t *testing.T, p HourPlan) { require.Empty(t, p.Items) },
		},
		{
			name: "an end track longer than the remainder is still a warning", fromMS: 57 * mn,
			library:   library(append(songs(1, 40, 4*mn, music), trackSpec{id: 500, duration: 5 * mn, playlist: jingles})...),
			playlists: []clocks.ClockPlaylist{fill, pl(jingles, clocks.RuleEnd)},
			warnings:  []WarningCode{WarnRuleSlotSkipped},
			check:     func(t *testing.T, p HourPlan) { require.Empty(t, itemsOf(p, jingles)) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := tc.library
			if l == nil {
				l = library(startedSpecs()...)
			}
			in := input(l, tc.playlists, tc.inserts...)
			in.HourInserts = tc.hour
			in.FromMS = tc.fromMS
			p := Build(in)
			requireTimeline(t, p)
			for _, it := range p.Items {
				require.GreaterOrEqual(t, it.OffsetMS, tc.fromMS, "nothing starts before the build begins")
			}
			require.ElementsMatch(t, tc.warnings, codes(p))
			require.Empty(t, p.Refused)
			tc.check(t, p)
		})
	}
}

// A build with FromMS 0 is the build of a whole hour: the plan of this seed
// was taken from the code before FromMS was added.
func TestBuildFromZeroKeepsThePlanOfASeed(t *testing.T) {
	t.Parallel()
	in := input(library(startedSpecs()...), []clocks.ClockPlaylist{
		pl(music, clocks.RuleFill), pl(jingles, clocks.RuleSpread, 3), pl(extra, clocks.RuleStartEnd), pl(news, clocks.RuleMiddle),
	}, trackInsert(1, 15*mn, 60*sec, 301))

	type placed struct {
		track  uint64
		offset int
	}
	want := []placed{
		{206, 0},
		{11, 45000},
		{35, 285000},
		{101, 645000},
		{26, 675000},
		{301, 915000},
		{23, 1005000},
		{25, 1305000},
		{107, 1665000},
		{2, 1725000},
		{303, 1995000},
		{38, 2145000},
		{32, 2445000},
		{20, 2715000},
		{103, 3075000},
		{1, 3165000},
		{40, 3405000},
		{203, 3495000},
	}
	var got []placed
	for _, it := range Build(in).Items {
		got = append(got, placed{it.TrackID, it.OffsetMS})
	}
	require.Equal(t, want, got)
}

func TestStartedOffset(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		want    int
		ok      bool
	}{
		{name: "the first instant of the hour", elapsed: 0, want: 10 * sec, ok: true},
		{name: "the middle of the hour", elapsed: 20 * time.Minute, want: 20*mn + 10*sec, ok: true},
		{name: "rounded up to a millisecond", elapsed: time.Minute + time.Nanosecond, want: mn + 10*sec + 1, ok: true},
		{name: "exactly the minimum remainder", elapsed: 57*time.Minute + 50*time.Second, want: 58 * mn, ok: true},
		{name: "just under the minimum remainder", elapsed: 57*time.Minute + 50*time.Second + time.Millisecond},
		{name: "the last seconds of the hour", elapsed: 59 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := startedOffset(fullBase, fullBase.Add(tc.elapsed))
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

// oneHour is the range that holds the hour on air of the sync tests.
func oneHour(start time.Time) []Range { return []Range{{From: start, To: start.Add(time.Hour)}} }

func TestSyncStartedHour(t *testing.T) {
	t.Parallel()
	start := fullBase
	key := start.Unix()
	next := start.Add(time.Hour).Unix()
	stored := []Item{{TrackID: 5, OffsetMS: 100, OutMS: 1000, Source: SourceFill}}
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		row     bool
		items   []Item
		force   bool
		offGrid bool
		built   bool
	}{
		{name: "no row: built for the remainder", elapsed: 20 * time.Minute, built: true},
		{name: "no row at the first instant of the hour", elapsed: 0, built: true},
		{name: "no row, with force: built for the remainder", elapsed: 20 * time.Minute, force: true, built: true},
		{name: "exactly the minimum remainder: built", elapsed: 57*time.Minute + 50*time.Second, built: true},
		{name: "a row with items is not touched", elapsed: 20 * time.Minute, row: true, items: stored},
		{name: "a row without items is not touched", elapsed: 20 * time.Minute, row: true},
		{name: "a row with items is not touched by force", elapsed: 20 * time.Minute, row: true, items: stored, force: true},
		{name: "a row without items is not touched by force", elapsed: 20 * time.Minute, row: true, force: true},
		{name: "under the minimum remainder: no row", elapsed: 58 * time.Minute},
		{name: "not on the grid: nothing", elapsed: 20 * time.Minute, offGrid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := start.Add(tc.elapsed)
			ts := newTestService(t, fullSettings(), fixedNow(now), nil)
			var row PlanHour
			if tc.row {
				row = PlanHour{Start: start, ClockID: 7, ClockUpdatedAt: testClock().UpdatedAt.Add(-time.Hour), Seed: 1}
				ts.store.hours[key], ts.store.items[key] = row, tc.items
			}
			if tc.offGrid {
				ts.grid.without = map[int64]bool{key: true}
			}
			var force []Range
			if tc.force {
				force = oneHour(start)
			}

			res, err := ts.svc.sync(t.Context(), TriggerJob, force)
			require.NoError(t, err)
			require.Contains(t, ts.store.hours, next, "the later hours of the sync are built")
			require.Zero(t, res.Skipped, "an hour with a row does not join the work list")

			got, found := ts.store.hours[key]
			switch {
			case tc.built:
				require.True(t, found)
				require.Equal(t, key, got.Seed, "the seed stays the hour start")
				items := ts.store.items[key]
				require.NotEmpty(t, items)
				from := int(tc.elapsed.Milliseconds()) + 10*sec
				requireTimeline(t, HourPlan{Items: items})
				require.GreaterOrEqual(t, items[0].OffsetMS, from)
			case tc.row:
				require.Equal(t, row, got)
				require.Equal(t, tc.items, ts.store.items[key])
			default:
				require.False(t, found, "no row is written")
				require.Empty(t, ts.store.items[key])
			}
		})
	}
}

func TestStartedHourIsBuiltOnce(t *testing.T) {
	t.Parallel()
	start := fullBase
	key := start.Unix()
	var now time.Time
	at := func(elapsed time.Duration) { now = start.Add(elapsed) }
	at(20 * time.Minute)
	ts := newTestService(t, fullSettings(), func() time.Time { return now }, nil)

	_, err := ts.svc.RunJob(t.Context())
	require.NoError(t, err)
	row, items := ts.store.hours[key], ts.store.items[key]
	require.NotEmpty(t, items)

	at(25 * time.Minute)
	_, err = ts.svc.RunJob(t.Context())
	require.NoError(t, err)
	require.Equal(t, row, ts.store.hours[key], "a second sync does not build it again")
	require.Equal(t, items, ts.store.items[key])

	edited := testClock()
	edited.UpdatedAt = edited.UpdatedAt.Add(time.Hour)
	ts.svc.clocks = fakeClocks{7: edited}
	at(30 * time.Minute)
	_, err = ts.svc.sync(t.Context(), TriggerClockBuild, oneHour(start))
	require.NoError(t, err)
	require.Equal(t, row, ts.store.hours[key], "a clock edit and a rebuild keep the partial hour")
	require.Equal(t, items, ts.store.items[key])
}

// The offset and the remainder come from the time under the lock, not from
// the read of the sync.
func TestBuildOfAStartedHourUsesTheTimeUnderTheLock(t *testing.T) {
	t.Parallel()
	start := fullBase
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		built   bool
	}{
		{name: "the build begins after the lead from now", elapsed: 30 * time.Minute, built: true},
		{name: "too little is left by now: no row", elapsed: 58*time.Minute + 30*time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			set := fullSettings()
			ts := newTestService(t, set, fixedNow(start.Add(tc.elapsed)), nil)
			lib := ts.store.lib
			out, err := ts.svc.buildTx(t.Context(), ts.store,
				hourJob{start: start, slotID: new(uint64(9)), clock: testClock(), started: true}, &lib, set)
			require.NoError(t, err)
			require.Equal(t, !tc.built, out.skipped)
			require.Equal(t, tc.built, len(ts.store.hours) == 1)
			if tc.built {
				require.GreaterOrEqual(t, ts.store.items[start.Unix()][0].OffsetMS, int(tc.elapsed.Milliseconds())+10*sec)
			}
		})
	}
}

// The freeze before an hour starts behaves as before: a built hour is kept,
// a missing one is built whole.
func TestFreezeBeforeTheStartIsUnchanged(t *testing.T) {
	t.Parallel()
	now := fullBase.Add(58*time.Minute + 30*time.Second)
	frozen := fullBase.Add(time.Hour)
	key := frozen.Unix()
	stored := []Item{{TrackID: 5, OffsetMS: 100, OutMS: 1000, Source: SourceFill}}
	for _, tc := range []struct {
		name string
		row  bool
	}{
		{name: "a built hour is kept", row: true},
		{name: "a missing hour is built from its start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestService(t, fullSettings(), fixedNow(now), nil)
			if tc.row {
				ts.store.hours[key], ts.store.items[key] = PlanHour{Start: frozen, ClockID: 7, Seed: 1}, stored
			}
			_, err := ts.svc.RunJob(t.Context())
			require.NoError(t, err)
			if tc.row {
				require.Equal(t, stored, ts.store.items[key])
				return
			}
			require.Zero(t, ts.store.items[key][0].OffsetMS)
		})
	}
}

// What aired in the hour before now counts for the play rules of the rest.
func TestPlaysOfTheStartedHourCount(t *testing.T) {
	t.Parallel()
	const maxOnce uint64 = 2
	start := fullBase
	aired := start.Add(5 * time.Minute)
	for _, tc := range []struct {
		name    string
		rules   []uint64
		artist  uint64
		play    *Play
		blocked bool
	}{
		{name: "nothing aired: the track plays", rules: []uint64{rotationRule}},
		{name: "the track interval", rules: []uint64{rotationRule}, play: &Play{TrackID: 1, Start: aired}, blocked: true},
		{
			name: "the artist interval", rules: []uint64{rotationRule}, artist: 77,
			play: &Play{TrackID: 99, ArtistID: new(uint64(77)), Start: aired}, blocked: true,
		},
		{name: "plays per hour", rules: []uint64{maxOnce}, play: &Play{TrackID: 1, Start: aired}, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestService(t, fullSettings(), fixedNow(start.Add(20*time.Minute)), nil)
			l := library(
				trackSpec{id: 1, duration: 15 * mn, playlist: music, artist: tc.artist, rules: tc.rules},
				trackSpec{id: 2, duration: 15 * mn, playlist: music, rules: tc.rules},
			)
			l.Rules[maxOnce] = playrules.RuleInput{Name: "Once", MaxPerHour: new(1)}
			ts.store.lib = *l
			if tc.play != nil {
				ts.store.base = []Play{*tc.play}
			}
			_, err := ts.svc.sync(t.Context(), TriggerJob, oneHour(start))
			require.NoError(t, err)
			require.Equal(t, !tc.blocked, hasTrack(ts.store.items[start.Unix()], 1))
		})
	}
}
