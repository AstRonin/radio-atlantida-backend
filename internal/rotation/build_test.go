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
	news    uint64 = 4
	mn      int    = 60_000
	sec     int    = 1000
	// rotationRule is "Music rotation": a track once per 72 h, an artist
	// once per 2 h.
	rotationRule uint64 = 1
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

// trackSpec is a track of a test library.
type trackSpec struct {
	id       uint64
	duration int
	playlist uint64
	artist   uint64
	rules    []uint64
}

func library(specs ...trackSpec) *Library {
	l := &Library{Rules: map[uint64]playrules.RuleInput{
		rotationRule: {Name: "Music rotation", MinIntervalMinutes: new(72 * 60), ArtistMinIntervalMinutes: new(120)},
	}}
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
// own artist and the rotation rule; their lengths step by 30 s from
// duration, five steps.
func songs(first uint64, n, duration int, playlist uint64) []trackSpec {
	var out []trackSpec
	for i := range n {
		id := first + uint64(i)
		out = append(out, trackSpec{
			id: id, duration: duration + (i%5)*30*sec, playlist: playlist, artist: id, rules: []uint64{rotationRule},
		})
	}
	return out
}

func rule(r clocks.Rule) *clocks.Rule { return &r }

// pl is a playlist of a test clock with its own rule.
func pl(id uint64, r clocks.Rule, count ...int) clocks.ClockPlaylist {
	p := clocks.ClockPlaylist{PlaylistID: id, Own: clocks.PlaylistRule{Rule: r}}
	if len(count) > 0 {
		p.Own.Count = new(count[0])
	}
	return p
}

func trackInsert(id uint64, offset, tolerance int, trackID uint64) clocks.Insert {
	return clocks.Insert{ID: id, OffsetMS: offset, ToleranceMS: tolerance, TrackID: new(trackID)}
}

func input(l *Library, playlists []clocks.ClockPlaylist, inserts ...clocks.Insert) HourInput {
	return HourInput{
		Start: hour10, Location: yerevan,
		Clock:    clocks.Clock{ID: 7, Name: "Day", Playlists: playlists, Inserts: inserts},
		Library:  l,
		Settings: Settings{MinGapMS: 10 * sec},
		Seed:     42,
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

// requireTimeline checks that the items are in time order, never overlap,
// and stay inside the hour.
func requireTimeline(t *testing.T, p HourPlan) {
	t.Helper()
	at := 0
	for i, it := range p.Items {
		require.GreaterOrEqual(t, it.OffsetMS, at, "item %d overlaps the one before", i)
		require.Greater(t, it.OutMS, it.InMS)
		at = end(it)
		require.LessOrEqual(t, at, clocks.HourMS)
	}
}

func TestBuildFillsTheHourWithLongTracks(t *testing.T) {
	t.Parallel()
	l := library(songs(1, 40, 6*mn, music)...) // 6:00 to 8:00
	p := Build(input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill)}))

	requireTimeline(t, p)
	require.Empty(t, p.Warnings)
	require.Equal(t, clocks.HourMS, end(p.Items[len(p.Items)-1]), "the hour ends at 60:00")
	for i, it := range p.Items[:len(p.Items)-1] {
		require.Equal(t, it.OffsetMS+it.OutMS-it.InMS, p.Items[i+1].OffsetMS, "back to back")
		require.False(t, it.Truncated)
		require.Equal(t, SourceFill, it.Source)
		require.Equal(t, music, *it.PlaylistID)
	}
	seen := map[uint64]bool{}
	for _, it := range p.Items {
		require.False(t, seen[it.TrackID], "the rotation rule forbids a repeat")
		seen[it.TrackID] = true
	}
}

func TestBuildCutsTheLastTrackWhenNoneFits(t *testing.T) {
	t.Parallel()
	// Every track is 7 minutes: 8 fill 56:00, the 9th is cut at 60:00.
	var specs []trackSpec
	for i := range 20 {
		specs = append(specs, trackSpec{id: uint64(i + 1), duration: 7 * mn, playlist: music, artist: uint64(i + 1)})
	}
	p := Build(input(library(specs...), []clocks.ClockPlaylist{pl(music, clocks.RuleFill)}))

	requireTimeline(t, p)
	require.Len(t, p.Items, 9)
	last := p.Items[8]
	require.True(t, last.Truncated)
	require.Equal(t, 56*mn, last.OffsetMS)
	require.Equal(t, 4*mn, last.OutMS-last.InMS)
}

func TestBuildSpreadsRuleTracks(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 40, 3*mn, music), songs(100, 10, 20*sec, jingles)...)...)
	for seed := range uint64(30) {
		in := input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill), pl(jingles, clocks.RuleSpread, 2)})
		in.Seed = seed
		p := Build(in)
		requireTimeline(t, p)
		require.Empty(t, p.Warnings, "seed %d", seed)
		var rules []int
		for i, it := range p.Items {
			if it.Source != SourceRule {
				continue
			}
			rules = append(rules, it.OffsetMS)
			require.Equal(t, SourceFill, p.Items[i-1].Source, "seed %d: a fill track before it", seed)
			require.Equal(t, SourceFill, p.Items[i+1].Source, "seed %d: a fill track after it", seed)
		}
		require.Len(t, rules, 2, "seed %d", seed)
		require.Less(t, rules[0], 30*mn+3*mn, "seed %d: first half (a fill track late at most)", seed)
		require.Greater(t, rules[1], 30*mn, "seed %d: second half", seed)
	}
}

func TestBuildStartEndAndMiddle(t *testing.T) {
	t.Parallel()
	l := library(append(append(songs(1, 40, 3*mn, music), songs(100, 5, 20*sec, jingles)...),
		songs(200, 5, 2*mn, news)...)...)

	t.Run("start and end", func(t *testing.T) {
		t.Parallel()
		p := Build(input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill), pl(jingles, clocks.RuleStartEnd)}))
		requireTimeline(t, p)
		require.Empty(t, p.Warnings)
		first, last := p.Items[0], p.Items[len(p.Items)-1]
		require.Equal(t, SourceRule, first.Source)
		require.Zero(t, first.OffsetMS)
		require.Equal(t, SourceRule, last.Source)
		require.Equal(t, clocks.HourMS, end(last), "the end track ends at 60:00")
		require.False(t, last.Truncated)
	})
	t.Run("end after start of another playlist", func(t *testing.T) {
		t.Parallel()
		p := Build(input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill), pl(news, clocks.RuleStart), pl(jingles, clocks.RuleEnd)}))
		requireTimeline(t, p)
		require.Equal(t, news, *p.Items[0].PlaylistID)
		require.Equal(t, jingles, *p.Items[len(p.Items)-1].PlaylistID)
	})
	t.Run("middle", func(t *testing.T) {
		t.Parallel()
		for seed := range uint64(20) {
			in := input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill), pl(news, clocks.RuleMiddle)})
			in.Seed = seed
			p := Build(in)
			requireTimeline(t, p)
			require.Empty(t, p.Warnings, "seed %d", seed)
			n := 0
			for _, it := range p.Items {
				if it.Source == SourceRule {
					n++
					require.GreaterOrEqual(t, it.OffsetMS, 25*mn, "seed %d", seed)
					require.LessOrEqual(t, it.OffsetMS, 35*mn, "seed %d", seed)
				}
			}
			require.Equal(t, 1, n, "seed %d", seed)
		}
	})
	t.Run("middle skipped when an insert covers its window", func(t *testing.T) {
		t.Parallel()
		long := library(append(append(songs(1, 40, 3*mn, music), songs(200, 5, 2*mn, news)...),
			trackSpec{id: 300, duration: 12 * mn, playlist: extra, artist: 300})...)
		p := Build(input(long, []clocks.ClockPlaylist{pl(music, clocks.RuleFill), pl(news, clocks.RuleMiddle)},
			trackInsert(1, 24*mn, 0, 300)))
		requireTimeline(t, p)
		require.Equal(t, []WarningCode{WarnRuleSlotSkipped}, codes(p))
		require.Equal(t, 1, *p.Warnings[0].PlaylistIndex)
	})
}

func TestBuildFillWeights(t *testing.T) {
	t.Parallel()
	// 4-minute tracks: 15 per hour, every one drawn by the weights.
	share := func(a, b []trackSpec, playlists []clocks.ClockPlaylist, hours int) float64 {
		l := library(append(a, b...)...)
		first, total := 0, 0
		for h := range hours {
			in := input(l, playlists)
			in.Seed = uint64(h)
			for _, it := range Build(in).Items {
				total++
				if *it.PlaylistID == music {
					first++
				}
			}
		}
		return float64(first) / float64(total)
	}
	plain := func(first uint64, n int, playlist uint64) []trackSpec {
		var out []trackSpec
		for i := range n {
			out = append(out, trackSpec{id: first + uint64(i), duration: 4 * mn, playlist: playlist})
		}
		return out
	}

	weighted := []clocks.ClockPlaylist{pl(music, clocks.RuleFill), pl(extra, clocks.RuleFill)}
	weighted[0].FillWeight, weighted[1].FillWeight = new(30), new(70)
	got := share(plain(1, 50, music), plain(100, 50, extra), weighted, 1000)
	require.InDelta(t, 0.30, got, 0.03)

	// Without weights the playlist is drawn first: 5 tracks play as often
	// as 500.
	got = share(plain(1, 5, music), plain(100, 500, extra),
		[]clocks.ClockPlaylist{pl(music, clocks.RuleFill), pl(extra, clocks.RuleFill)}, 200)
	require.InDelta(t, 0.5, got, 0.05)
}

func TestBuildChainsInserts(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 40, 3*mn, music),
		trackSpec{id: 100, duration: 20 * sec, playlist: jingles, artist: 100},
		trackSpec{id: 101, duration: 20 * sec, playlist: jingles, artist: 101},
		trackSpec{id: 200, duration: 2 * mn, playlist: news, artist: 200},
		trackSpec{id: 201, duration: 2 * mn, playlist: news, artist: 201})...)
	for seed := range uint64(30) {
		in := input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill)},
			trackInsert(11, 29*mn+40*sec, 20*sec, 100),
			clocks.Insert{ID: 12, OffsetMS: 30 * mn, ToleranceMS: mn, PlaylistID: new(news)},
			trackInsert(13, 32*mn, mn, 101),
		)
		in.Seed = seed
		p := Build(in)
		requireTimeline(t, p)
		require.Empty(t, p.Warnings, "seed %d", seed)

		var ins []int
		for i, it := range p.Items {
			if it.Source == SourceInsert {
				ins = append(ins, i)
			}
		}
		require.Len(t, ins, 3, "seed %d", seed)
		require.Equal(t, []int{ins[0], ins[0] + 1, ins[0] + 2}, ins, "seed %d: no music between them", seed)
		a, b, c := p.Items[ins[0]], p.Items[ins[1]], p.Items[ins[2]]
		require.Equal(t, end(a), b.OffsetMS, "seed %d: back to back", seed)
		require.Equal(t, end(b), c.OffsetMS, "seed %d: back to back", seed)
		require.Equal(t, 0, *a.InsertIndex)
		require.Equal(t, uint64(11), *a.ClockInsertID)
		require.GreaterOrEqual(t, a.OffsetMS, 29*mn+20*sec, "seed %d: inside its window", seed)
		require.LessOrEqual(t, a.OffsetMS, 30*mn, "seed %d: inside its window", seed)
		before := p.Items[ins[0]-1]
		if before.Truncated {
			require.Equal(t, 29*mn+40*sec, end(before), "seed %d: cut at the target", seed)
		}
	}
}

func TestBuildOneTimeInserts(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 40, 3*mn, music),
		trackSpec{id: 100, duration: 20 * sec, playlist: jingles, artist: 100},
		trackSpec{id: 300, duration: 15 * mn, playlist: extra, artist: 300},
		trackSpec{id: 301, duration: 2 * mn, playlist: news, artist: 301})...)

	t.Run("replaces the template insert at its offset", func(t *testing.T) {
		t.Parallel()
		in := input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill)}, trackInsert(11, 30*mn, mn, 100))
		in.HourInserts = []Insert{{ID: 5, TrackID: 301, Placement: PlacementFixed, OffsetMS: new(30 * mn), ToleranceMS: mn}}
		p := Build(in)
		requireTimeline(t, p)
		require.Equal(t, []WarningCode{WarnInsertReplaced}, codes(p))
		require.Empty(t, p.Refused)
		var got []Source
		for _, it := range p.Items {
			if it.Source != SourceFill {
				got = append(got, it.Source)
				require.Equal(t, uint64(301), it.TrackID)
				require.Equal(t, uint64(5), *it.HourInsertID)
			}
		}
		require.Equal(t, []Source{SourceHourInsert}, got)
	})
	t.Run("a chain past the window skips a template insert and refuses a one-time one", func(t *testing.T) {
		t.Parallel()
		in := input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill)},
			trackInsert(11, 10*mn, 0, 300), trackInsert(12, 20*mn, mn, 100))
		in.HourInserts = []Insert{{ID: 5, TrackID: 301, Placement: PlacementFixed, OffsetMS: new(22 * mn), ToleranceMS: 0}}
		p := Build(in)
		requireTimeline(t, p)
		require.Equal(t, []WarningCode{WarnInsertSkipped}, codes(p))
		require.Equal(t, 1, *p.Warnings[0].InsertIndex)
		require.Equal(t, ReasonDoesNotFit, p.Refused[5].Reason)
	})
	t.Run("start, end, and a refused blocked track", func(t *testing.T) {
		t.Parallel()
		bl := library(append(songs(1, 40, 3*mn, music),
			trackSpec{id: 301, duration: 2 * mn, playlist: news, artist: 301},
			trackSpec{id: 302, duration: 2 * mn, playlist: news, artist: 302},
			trackSpec{id: 303, duration: 2 * mn, playlist: news, artist: 303, rules: []uint64{rotationRule}})...)
		missed := uint64(900)
		in := input(bl, []clocks.ClockPlaylist{pl(music, clocks.RuleFill)})
		in.HourInserts = []Insert{
			{ID: 5, TrackID: 301, MissedID: &missed, Placement: PlacementStart},
			{ID: 6, TrackID: 302, Placement: PlacementEnd},
			{ID: 7, TrackID: 303, Placement: PlacementFixed, OffsetMS: new(30 * mn), ToleranceMS: mn},
		}
		in.History = []Play{{TrackID: 303, ArtistID: new(uint64(303)), Start: hour10.Add(-time.Hour)}}
		p := Build(in)
		requireTimeline(t, p)
		first, last := p.Items[0], p.Items[len(p.Items)-1]
		require.Equal(t, SourceReschedule, first.Source)
		require.Equal(t, &missed, first.RescheduleOfID)
		require.Zero(t, first.OffsetMS)
		require.Equal(t, SourceHourInsert, last.Source)
		require.Equal(t, clocks.HourMS, end(last))
		require.Equal(t, map[uint64]Refusal{7: p.Refused[7]}, p.Refused)
		require.Equal(t, ReasonBlocked, p.Refused[7].Reason)
	})
}

func TestBuildLeavesAShortGap(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 40, 3*mn, music), trackSpec{id: 300, duration: 59*mn + 55*sec, playlist: extra})...)
	p := Build(input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill)}, trackInsert(1, 0, 0, 300)))
	require.Len(t, p.Items, 1, "the 5 s gap is under rotation.min_gap_ms")
	require.Empty(t, p.Warnings)
}

func TestBuildArtistInterval(t *testing.T) {
	t.Parallel()
	// Artist 9 has tracks in music and in jingles; the rotation rule keeps
	// them 2 h apart, whichever playlist they come from.
	specs := songs(1, 30, 3*mn, music)
	specs = append(specs,
		trackSpec{id: 90, duration: 3 * mn, playlist: music, artist: 9, rules: []uint64{rotationRule}},
		trackSpec{id: 91, duration: 3 * mn, playlist: music, artist: 9, rules: []uint64{rotationRule}},
		trackSpec{id: 92, duration: 20 * sec, playlist: jingles, artist: 9, rules: []uint64{rotationRule}})
	l := library(specs...)
	for seed := range uint64(50) {
		in := input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill), pl(jingles, clocks.RuleSpread, 1)})
		in.Seed = seed
		n := 0
		for _, it := range Build(in).Items {
			if it.TrackID >= 90 {
				n++
			}
		}
		require.LessOrEqual(t, n, 1, "seed %d", seed)
	}
}

func TestCheckerTrackInterval(t *testing.T) {
	t.Parallel()
	l := &Library{Rules: map[uint64]playrules.RuleInput{5: {Name: "Jingles", MinIntervalMinutes: new(20)}}}
	jingle := Track{ID: 1, DurationMS: 20 * sec, RuleIDs: []uint64{5}}
	c := newChecker(HourInput{Location: yerevan, Library: l, History: []Play{{TrackID: 1, Start: hour10.Add(-15 * time.Minute)}}})
	require.False(t, c.allowed(jingle, hour10.Add(4*time.Minute)))
	require.True(t, c.allowed(jingle, hour10.Add(5*time.Minute)), "20 minutes after the last start")
}

func TestBuildEmergency(t *testing.T) {
	t.Parallel()
	// Every music track may play only from 06:00 to 08:00.
	morning := uint64(9)
	specs := songs(1, 20, 3*mn, music)
	for i := range specs {
		specs[i].rules = []uint64{morning}
	}
	l := library(append(specs, songs(100, 30, 3*mn, extra)...)...)
	l.Rules[morning] = playrules.RuleInput{Name: "Morning", WindowFrom: new(playrules.TimeOfDay(6 * 60)), WindowTo: new(playrules.TimeOfDay(8 * 60))}

	p := Build(input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill)}))
	requireTimeline(t, p)
	require.Equal(t, []WarningCode{WarnGeneralPool}, codes(p))
	require.Equal(t, clocks.HourMS, end(p.Items[len(p.Items)-1]))
	for _, it := range p.Items {
		require.Equal(t, SourceGeneral, it.Source)
		require.Nil(t, it.PlaylistID)
	}

	only := library(specs...)
	only.Rules[morning] = l.Rules[morning]
	p = Build(input(only, []clocks.ClockPlaylist{pl(music, clocks.RuleFill)}))
	require.Empty(t, p.Items)
	require.Equal(t, []WarningCode{WarnEmergency}, codes(p))
}

func TestBuildRuleBlocked(t *testing.T) {
	t.Parallel()
	morning := uint64(9)
	specs := append(songs(1, 30, 3*mn, music), trackSpec{id: 100, duration: 20 * sec, playlist: jingles, rules: []uint64{morning}})
	l := library(specs...)
	l.Rules[morning] = playrules.RuleInput{Name: "Morning", WindowFrom: new(playrules.TimeOfDay(6 * 60)), WindowTo: new(playrules.TimeOfDay(8 * 60))}
	p := Build(input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill), pl(jingles, clocks.RuleStart)}))
	require.Equal(t, []WarningCode{WarnRuleBlocked}, codes(p))
	require.Equal(t, SourceFill, p.Items[0].Source, "the fill covers its time")
}

func TestBuildIsDeterministic(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 40, 3*mn, music), songs(100, 10, 20*sec, jingles)...)...)
	in := input(l, []clocks.ClockPlaylist{pl(music, clocks.RuleFill), pl(jingles, clocks.RuleSpread, 3)},
		clocks.Insert{ID: 1, OffsetMS: 30 * mn, ToleranceMS: mn, PlaylistID: new(jingles)})
	require.Equal(t, Build(in), Build(in))
	other := in
	other.Seed = 43
	require.NotEqual(t, Build(in).Items, Build(other).Items)
}

func TestBuildOverride(t *testing.T) {
	t.Parallel()
	// The jingles playlist is spread by itself; the clock makes it fill.
	l := library(songs(100, 40, 3*mn, jingles)...)
	p := pl(jingles, clocks.RuleSpread, 2)
	p.Rule = rule(clocks.RuleFill)
	plan := Build(input(l, []clocks.ClockPlaylist{p}))
	requireTimeline(t, plan)
	for _, it := range plan.Items {
		require.Equal(t, SourceFill, it.Source)
	}
}
