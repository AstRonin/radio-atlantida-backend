package rotation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
)

const (
	music   uint64 = 1
	jingles uint64 = 2
	extra   uint64 = 3
	mn      int    = 60_000
)

var yerevan = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Yerevan")
	if err != nil {
		panic(err)
	}
	return loc
}()

// hour10 is 10:00 station time (06:00 UTC).
var hour10 = time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)

// lib builds a library; each track is (id, duration, playlist, artist).
type trackSpec struct {
	id       uint64
	duration int
	playlist uint64
	artist   uint64
	rules    []uint64
}

func library(specs ...trackSpec) *Library {
	l := &Library{Rules: map[uint64]playrules.RuleInput{}}
	for _, s := range specs {
		t := Track{ID: s.id, DurationMS: s.duration, PlaylistIDs: []uint64{s.playlist}, RuleIDs: s.rules}
		if s.artist != 0 {
			t.ArtistID = new(s.artist)
		}
		l.Tracks = append(l.Tracks, t)
	}
	return l
}

// songs returns n tracks of the playlist with ids from first, each with its
// own artist.
func songs(first uint64, n, duration int, playlist uint64) []trackSpec {
	var out []trackSpec
	for i := range n {
		id := first + uint64(i)
		out = append(out, trackSpec{id: id, duration: duration, playlist: playlist, artist: id})
	}
	return out
}

func flow(nominal int, playlists ...uint64) clocks.Element {
	return clocks.Element{
		Placement: clocks.PlacementFlow, NominalDurationMS: nominal, DurationToleranceMS: 30_000,
		Filler: clocks.Filler{Mode: clocks.FillerNone}, PlaylistIDs: playlists,
	}
}

// withFiller sets the element's filler mode.
func withFiller(e clocks.Element, mode clocks.FillerMode) clocks.Element {
	e.Filler = clocks.Filler{Mode: mode}
	return e
}

// noFiller is a minimum filler gap larger than any gap.
const noFiller = clocks.HourMS + 1

func fixed(minute, nominal int, playlists ...uint64) clocks.Element {
	e := flow(nominal, playlists...)
	e.Placement, e.FixedMinute, e.FixedToleranceMS = clocks.PlacementFixed, new(minute), 60_000
	return e
}

func input(t *testing.T, l *Library, elements ...clocks.Element) HourInput {
	t.Helper()
	for i := range elements {
		elements[i].ID = uint64(i + 1)
	}
	layout, err := clocks.LayoutOf(elements)
	require.NoError(t, err)
	return HourInput{
		Start: hour10, Location: yerevan,
		Clock:         clocks.Clock{ID: 7, Name: "Day", Elements: elements},
		Layout:        layout,
		DefaultFiller: clocks.Filler{Mode: clocks.FillerRandom},
		Library:       l,
		Separation:    Separation{Track: 72 * time.Hour, Artist: 2 * time.Hour},
		Settings:      Settings{MinFillerGapMS: 10_000, MaxRelaxedToleranceMS: 120_000},
		Seed:          42,
	}
}

func codes(p HourPlan) []WarningCode {
	var out []WarningCode
	for _, w := range p.Warnings {
		out = append(out, w.Code)
	}
	return out
}

func end(it Item) int { return it.OffsetMS + it.OutMS - it.InMS }

func TestBuildFillsTheHour(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 10, 19*mn, music), songs(100, 5, 3*mn+5000, extra)...)...)
	in := input(t, l, flow(19*mn, music), flow(19*mn, music), withFiller(flow(19*mn, music), clocks.FillerRandom))
	p := Build(in)

	require.Empty(t, p.Warnings)
	require.Len(t, p.Items, 4)
	for i, it := range p.Items[:3] {
		require.Equal(t, SourceClock, it.Source)
		require.Equal(t, uint64(i+1), *it.ElementID)
		require.Equal(t, i*19*mn, it.OffsetMS)
	}
	// 3 minutes left: the best fit from the whole library, cut at the hour.
	f := p.Items[3]
	require.Equal(t, SourceFillerRandom, f.Source)
	require.Nil(t, f.ElementID)
	require.GreaterOrEqual(t, f.TrackID, uint64(100))
	require.True(t, f.Truncated)
	require.Equal(t, clocks.HourMS, end(f))
	// Every track at most once, every artist separated.
	seen := map[uint64]bool{}
	for _, it := range p.Items {
		require.False(t, seen[it.TrackID])
		seen[it.TrackID] = true
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	t.Parallel()
	l := library(songs(1, 50, 4*mn, music)...)
	elements := func() []clocks.Element {
		var es []clocks.Element
		for range 15 {
			es = append(es, flow(4*mn, music))
		}
		return es
	}
	a := Build(input(t, l, elements()...))
	b := Build(input(t, l, elements()...))
	require.Equal(t, a, b)

	other := input(t, l, elements()...)
	other.Seed = 43
	require.NotEqual(t, a.Items, Build(other).Items, "another seed picks other tracks")
}

func TestBuildHonoursSeparation(t *testing.T) {
	t.Parallel()
	l := library(songs(1, 3, 20*mn, music)...)
	in := input(t, l, flow(20*mn, music), withFiller(flow(20*mn, music), clocks.FillerRandom))
	// Track 1 aired 71 hours ago; the artist of track 2 aired an hour ago.
	in.History = []Play{
		{TrackID: 1, ArtistID: new(uint64(1)), Start: hour10.Add(-71 * time.Hour)},
		{TrackID: 9, ArtistID: new(uint64(2)), Start: hour10.Add(-time.Hour)},
	}
	p := Build(in)
	require.Len(t, p.Items, 1)
	require.Equal(t, uint64(3), p.Items[0].TrackID)
	require.Equal(t, []WarningCode{WarnEmergency, WarnGapUnfilled}, codes(p))
}

func TestBuildPlayRules(t *testing.T) {
	t.Parallel()
	morning, oncePerHour := uint64(1), uint64(2)
	l := library(
		trackSpec{id: 1, duration: 30_000, playlist: jingles, rules: []uint64{morning}},
		trackSpec{id: 2, duration: 30_000, playlist: jingles, rules: []uint64{oncePerHour}},
	)
	l.Rules[morning] = playrules.RuleInput{WindowFrom: new(playrules.TimeOfDay(6 * 60)), WindowTo: new(playrules.TimeOfDay(8 * 60))}
	l.Rules[oncePerHour] = playrules.RuleInput{MaxPerHour: new(1)}

	in := input(t, l, flow(30_000, jingles), flow(30_000, jingles))
	in.Separation = Separation{}
	in.Settings.MinFillerGapMS = noFiller
	p := Build(in)
	// At 10:00 the morning jingle is out of its window; the other plays
	// once, then the per-hour limit blocks every jingle.
	require.Len(t, p.Items, 1)
	require.Equal(t, uint64(2), p.Items[0].TrackID)
	require.Equal(t, []WarningCode{WarnBlockedByPlayRules}, codes(p))
	require.Equal(t, 1, *p.Warnings[0].Element)

	in.Start = time.Date(2026, 10, 15, 3, 0, 0, 0, time.UTC) // 07:00 station time
	p = Build(in)
	require.Len(t, p.Items, 2, "both jingles are allowed at 07:00")
}

func TestBuildMinInterval(t *testing.T) {
	t.Parallel()
	every10 := uint64(1)
	l := library(trackSpec{id: 1, duration: 4 * mn, playlist: jingles, rules: []uint64{every10}})
	l.Rules[every10] = playrules.RuleInput{MinIntervalMinutes: new(10)}
	in := input(t, l, flow(4*mn, jingles), flow(4*mn, jingles), flow(4*mn, jingles), flow(4*mn, jingles))
	in.Separation = Separation{}
	in.Settings.MinFillerGapMS = noFiller
	p := Build(in)
	// After the start at 0, every slot is at 4:00, because a skipped slot
	// does not advance the hour, and 4 minutes is within the interval.
	require.Len(t, p.Items, 1)
	require.Equal(t, []WarningCode{WarnBlockedByPlayRules, WarnBlockedByPlayRules, WarnBlockedByPlayRules}, codes(p))
}

func TestBuildCrossingTrackAtTheHour(t *testing.T) {
	t.Parallel()
	l := library(songs(1, 5, 21*mn, music)...)
	in := input(t, l, flow(21*mn, music), flow(21*mn, music), flow(21*mn, music), flow(21*mn, music))
	p := Build(in)
	// 0, 21, 42: the third crosses 60:00 and is cut there; the fourth is
	// removed, and overflow never asks for filler.
	require.Len(t, p.Items, 3)
	last := p.Items[2]
	require.True(t, last.Truncated)
	require.Equal(t, 18*mn, last.OutMS)
	require.Empty(t, p.Warnings)
}

func TestBuildFixedElement(t *testing.T) {
	t.Parallel()
	news := uint64(9)
	specs := append(songs(1, 10, 14*mn, music), trackSpec{id: 50, duration: 5 * mn, playlist: news})
	specs = append(specs, songs(100, 3, 80_000, extra)...)
	l := library(specs...)

	t.Run("short segment gets filler before the window", func(t *testing.T) {
		t.Parallel()
		in := input(t, l, flow(14*mn, music), withFiller(flow(14*mn, music), clocks.FillerRandom), fixed(30, 5*mn, news),
			flow(14*mn, music), flow(14*mn, music))
		p := Build(in)
		// 28 minutes of music, 1 minute short of the window (29:00 to
		// 31:00): an 80 s filler ends at 29:20, inside the window, and the
		// news starts there.
		require.Len(t, p.Items, 6)
		require.Equal(t, SourceFillerRandom, p.Items[2].Source)
		require.Equal(t, uint64(50), p.Items[3].TrackID)
		require.Equal(t, 29*mn+20_000, p.Items[3].OffsetMS)
		require.Equal(t, 34*mn+20_000, p.Items[4].OffsetMS, "the next segment starts at the end of the news")
	})

	t.Run("crossing track is cut at the minute", func(t *testing.T) {
		t.Parallel()
		// Nominal 3 × 10 minutes fits before 31:00; the 14-minute tracks
		// lie within the 5-minute tolerance but overrun.
		loose := flow(10*mn, music)
		loose.DurationToleranceMS = 5 * mn
		in := input(t, l, loose, loose, loose, fixed(30, 5*mn, news))
		in.Settings.MinFillerGapMS = noFiller
		p := Build(in)
		// 0, 14, 28: the third would end at 42:00, past 31:00; it is cut at
		// 30:00 and the news starts there.
		require.Len(t, p.Items, 4)
		require.True(t, p.Items[2].Truncated)
		require.Equal(t, 30*mn, end(p.Items[2]))
		require.Equal(t, 30*mn, p.Items[3].OffsetMS)
	})
}

func TestBuildEmergencySteps(t *testing.T) {
	t.Parallel()
	tight := flow(3*mn, music)
	tight.DurationToleranceMS = 10_000

	t.Run("relaxed tolerance", func(t *testing.T) {
		t.Parallel()
		in := input(t, library(trackSpec{id: 1, duration: 3*mn + 35_000, playlist: music}), tight)
		in.Settings.MinFillerGapMS = noFiller
		p := Build(in)
		require.Len(t, p.Items, 1)
		require.Equal(t, []WarningCode{WarnToleranceRelaxed}, codes(p))
	})
	t.Run("general pool", func(t *testing.T) {
		t.Parallel()
		l := library(trackSpec{id: 1, duration: 10 * mn, playlist: music}, trackSpec{id: 2, duration: 3 * mn, playlist: extra})
		in := input(t, l, tight)
		in.Settings.MinFillerGapMS = noFiller
		p := Build(in)
		require.Equal(t, uint64(2), p.Items[0].TrackID)
		require.Equal(t, []WarningCode{WarnGeneralPool}, codes(p))
	})
	t.Run("nothing at all", func(t *testing.T) {
		t.Parallel()
		in := input(t, library(trackSpec{id: 1, duration: 3 * mn, playlist: music}), tight)
		in.History = []Play{{TrackID: 1, Start: hour10.Add(-time.Hour)}}
		in.Settings.MinFillerGapMS = noFiller
		p := Build(in)
		require.Empty(t, p.Items)
		require.Equal(t, []WarningCode{WarnEmergency}, codes(p))
	})
}

func TestBuildWaitingSegments(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 5, 19*mn, music), songs(100, 3, 3*mn, extra)...)...)
	short := func() HourInput {
		// 3 × 19 minutes with holder none: the incomplete clock waits.
		return input(t, l, flow(19*mn, music), flow(19*mn, music), flow(19*mn, music))
	}
	// The default holder filler none fills at random when the segment is
	// short on real tracks only; on nominal durations it waits.
	in := short()
	p := Build(in)
	require.Equal(t, []int{0}, p.Asked)
	require.Equal(t, clocks.FillerNone, p.AskMode)
	require.Len(t, p.Items, 3)
	require.Equal(t, []WarningCode{WarnFillerWaiting}, codes(p))

	in = short()
	in.Waiting = WaitDefault
	in.DefaultFiller = clocks.Filler{Mode: clocks.FillerPlaylist, SourceID: new(extra)}
	p = Build(in)
	require.Len(t, p.Items, 4)
	require.Equal(t, SourceFillerPlaylist, p.Items[3].Source)

	in = short()
	in.Waiting, in.AnswerTrackID = WaitAnswer, 101
	p = Build(in)
	require.Len(t, p.Items, 4)
	require.Equal(t, SourceFillerAnswer, p.Items[3].Source)
	require.Equal(t, uint64(101), p.Items[3].TrackID)
	require.False(t, p.AnswerRefused)

	in = short()
	in.Waiting, in.AnswerTrackID = WaitAnswer, 101
	in.History = []Play{{TrackID: 101, Start: hour10.Add(-time.Hour)}}
	p = Build(in)
	require.True(t, p.AnswerRefused)
}

func TestBuildPool(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 5, 19*mn, music), songs(100, 3, 3*mn, extra)...)...)
	pooled := func() HourInput {
		last := flow(19*mn, music)
		last.Filler = clocks.Filler{Mode: clocks.FillerPool}
		return input(t, l, flow(19*mn, music), flow(19*mn, music), last)
	}

	in := pooled()
	in.Pool = []PoolRow{{ID: 11, TrackID: 101}, {ID: 12, TrackID: 102}}
	in.History = []Play{{TrackID: 101, Start: hour10.Add(-time.Hour)}}
	p := Build(in)
	// 101 breaks separation: skipped, the next row is used.
	require.Len(t, p.Items, 4)
	require.Equal(t, SourceFillerPool, p.Items[3].Source)
	require.Equal(t, uint64(102), p.Items[3].TrackID)
	require.Equal(t, uint64(12), *p.Items[3].PoolRowID)
	require.Equal(t, []WarningCode{WarnPoolTrackRefused}, codes(p))
	require.False(t, p.PoolEmpty)

	in = pooled()
	p = Build(in)
	require.True(t, p.PoolEmpty)
	require.Equal(t, SourceFillerRandom, p.Items[3].Source, "an empty pool falls back to the default mode")
	require.Equal(t, []WarningCode{WarnPoolEmpty}, codes(p))
}

func TestBuildSmallGapIsLeft(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 3, 19*mn+56_000, music), songs(100, 3, 3*mn, extra)...)...)
	in := input(t, l, flow(20*mn, music), flow(20*mn, music), flow(20*mn, music))
	p := Build(in)
	// 3 × 19:56 leaves 12 s: above the 10 s minimum, so filler, cut at the
	// hour.
	require.Len(t, p.Items, 4)
	in.Settings.MinFillerGapMS = 15_000
	p = Build(in)
	require.Len(t, p.Items, 3, "a gap below the minimum gets no filler")
	require.Empty(t, p.Warnings)
}

func TestCheckerWindows(t *testing.T) {
	t.Parallel()
	c := &checker{loc: yerevan, sep: Separation{Track: time.Hour}, byTrack: map[uint64][]time.Time{}, byArtist: map[uint64][]time.Time{}}
	tr := Track{ID: 1}
	c.add(1, nil, hour10)
	require.False(t, c.separated(tr, hour10.Add(59*time.Minute)))
	require.True(t, c.separated(tr, hour10.Add(time.Hour)), "an interval of exactly the separation passes")
	require.False(t, c.separated(tr, hour10.Add(-59*time.Minute)), "a later play blocks too")
}

// A track with cue points is fitted by its length on air, and its item
// carries the in and out points in the file (docs/phase-22-plan.md §3.2).
func TestBuildUsesTheCut(t *testing.T) {
	t.Parallel()
	// Track 1: a 70 s file cut to 10 s ... 40 s, 30 s on air. Track 2: a
	// 30 s file without a cut.
	l := library(trackSpec{id: 1, duration: 30_000, playlist: music, artist: 1}, trackSpec{id: 2, duration: 50*mn + 30_000, playlist: extra, artist: 2})
	l.Tracks[0].CueInMS = 10_000
	el := flow(30_000, music)
	el.DurationToleranceMS = 0
	in := input(t, l, el, flow(59*mn+30_000, extra))
	in.Settings.MinFillerGapMS = noFiller
	p := Build(in)

	require.Len(t, p.Items, 2)
	require.Equal(t, Item{TrackID: 1, ElementID: new(uint64(1)), OffsetMS: 0, InMS: 10_000, OutMS: 40_000, Source: SourceClock}, p.Items[0])
	require.Equal(t, 30_000, p.Items[1].OffsetMS, "the next item starts after the 30 s on air")
	require.Zero(t, p.Items[1].InMS)
}

// A crossing track with a cue in point is cut at the segment end, counted
// from its in point.
func TestBuildCutsACrossingTrackFromItsCueIn(t *testing.T) {
	t.Parallel()
	l := library(trackSpec{id: 1, duration: 61 * mn, playlist: music, artist: 1})
	l.Tracks[0].CueInMS = 5_000
	in := input(t, l, flow(61*mn, music))
	in.Settings.MinFillerGapMS = noFiller
	p := Build(in)

	require.Len(t, p.Items, 1)
	it := p.Items[0]
	require.True(t, it.Truncated)
	require.Equal(t, 5_000, it.InMS)
	require.Equal(t, 5_000+clocks.HourMS, it.OutMS)
	require.Equal(t, clocks.HourMS, end(it))
}
