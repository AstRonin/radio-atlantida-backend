package rotation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// news is a playlist of rescheduled tracks.
const news uint64 = 4

func pinnedInput(t *testing.T, l *Library, ins Insert, elements ...clocks.Element) (HourInput, insertedHour) {
	t.Helper()
	in := input(t, l, elements...)
	ih := applyInserts(in.Clock, in.Layout, []Insert{ins}, l)
	in.Clock, in.Layout, in.Pins = ih.clock, ih.layout, ih.pins
	return in, ih
}

func TestBuildPlacesAPinnedItem(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 10, 10*mn, music), trackSpec{id: 50, duration: 5 * mn, playlist: news, artist: 50})...)
	missed := uint64(900)
	in, ih := pinnedInput(t, l, Insert{ID: 1, TrackID: 50, MissedID: &missed, Placement: clocks.PlacementStart},
		flow(10*mn, music), flow(10*mn, music), flow(10*mn, music), flow(10*mn, music), flow(10*mn, music),
		withFiller(flow(10*mn, music), clocks.FillerRandom))
	require.Empty(t, ih.dropped)

	p := Build(in)

	require.Empty(t, p.PinsRefused)
	first := p.Items[0]
	require.Equal(t, SourceReschedule, first.Source)
	require.Equal(t, uint64(50), first.TrackID)
	require.Equal(t, &missed, first.RescheduleOfID)
	require.Nil(t, first.ElementID)
	require.Zero(t, first.OffsetMS)
	require.False(t, first.Truncated)
	// The clock's elements follow it.
	require.Equal(t, 5*mn, p.Items[1].OffsetMS)
	require.Equal(t, SourceClock, p.Items[1].Source)
}

func TestBuildRefusesAPinnedItemThatBreaksSeparation(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 10, 10*mn, music), trackSpec{id: 50, duration: 5 * mn, playlist: news, artist: 50})...)
	in, ih := pinnedInput(t, l, Insert{ID: 1, TrackID: 50, Placement: clocks.PlacementEnd},
		flow(10*mn, music), flow(10*mn, music))
	in.History = []Play{{TrackID: 50, ArtistID: new(uint64(50)), Start: hour10.Add(-time.Hour)}}

	p := Build(in)

	require.Equal(t, []uint64{1}, p.PinsRefused)
	require.Contains(t, codes(p), WarnRescheduleDropped)
	for _, it := range p.Items {
		require.NotEqual(t, SourceReschedule, it.Source)
	}
	remapWarnings(p.Warnings, ih.origin)
	for _, w := range p.Warnings {
		if w.Code == WarnRescheduleDropped {
			require.Nil(t, w.Element, "an insert is not an element of the clock")
		}
	}
}

func TestBuildRefusesAPinnedItemThatDoesNotFitWhole(t *testing.T) {
	t.Parallel()
	// The hour is full with music; an end item of 5 minutes would be cut.
	l := library(append(songs(1, 10, 20*mn, music), trackSpec{id: 50, duration: 5 * mn, playlist: news, artist: 50})...)
	in, _ := pinnedInput(t, l, Insert{ID: 1, TrackID: 50, Placement: clocks.PlacementEnd},
		flow(20*mn, music), flow(20*mn, music), flow(19*mn, music))

	p := Build(in)

	require.Equal(t, []uint64{1}, p.PinsRefused)
}

func TestApplyInserts(t *testing.T) {
	t.Parallel()
	l := library(append(songs(1, 5, 10*mn, music),
		trackSpec{id: 50, duration: 2 * mn, playlist: news, artist: 50},
		trackSpec{id: 51, duration: 2 * mn, playlist: news, artist: 51},
	)...)
	in := input(t, l, flow(10*mn, music), fixed(30, 2*mn, music), flow(10*mn, music))

	t.Run("replaces the clock's fixed element", func(t *testing.T) {
		t.Parallel()
		ih := applyInserts(in.Clock, in.Layout, []Insert{
			{ID: 1, TrackID: 50, Placement: clocks.PlacementFixed, FixedMinute: new(30), FixedToleranceMS: 60_000},
		}, l)
		require.Empty(t, ih.dropped)
		require.Len(t, ih.replaced, 1)
		require.Equal(t, uint64(2), ih.replaced[0].ID)
		require.Equal(t, []int{0, -1, 2}, ih.origin)
		require.Equal(t, uint64(1), ih.pins[1].InsertID)
	})
	t.Run("a later fixed insert replaces an earlier one", func(t *testing.T) {
		t.Parallel()
		ih := applyInserts(in.Clock, in.Layout, []Insert{
			{ID: 1, TrackID: 50, Placement: clocks.PlacementFixed, FixedMinute: new(30), FixedToleranceMS: 60_000},
			{ID: 2, TrackID: 51, Placement: clocks.PlacementFixed, FixedMinute: new(31), FixedToleranceMS: 60_000},
		}, l)
		require.Contains(t, ih.dropped, uint64(1))
		require.Len(t, ih.pins, 1)
		require.Equal(t, uint64(2), ih.pins[1].InsertID)
	})
	t.Run("an insert that no longer fits is dropped", func(t *testing.T) {
		t.Parallel()
		ih := applyInserts(in.Clock, in.Layout, []Insert{
			{ID: 3, TrackID: 50, Placement: clocks.PlacementFixed, FixedMinute: new(0), FixedToleranceMS: 60_000},
			{ID: 4, TrackID: 999, Placement: clocks.PlacementStart},
		}, l)
		require.Contains(t, ih.dropped, uint64(3))
		require.Equal(t, "the track has no duration", ih.dropped[4])
		require.Empty(t, ih.pins)
		require.Equal(t, []int{0, 1, 2}, ih.origin)
	})
}
