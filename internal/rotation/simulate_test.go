package rotation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// The plays of an earlier simulated hour count for the play rules of the
// later hours of the run: a track may not start again inside its interval
// even when the later hour is built after it in the simulation.
func TestWindowCarriesPlaysAcrossHours(t *testing.T) {
	t.Parallel()
	// Eighty songs with the rotation rule: a track plays once in 72 h. Each
	// hour takes about twenty of them, so a second hour that ignored the
	// first would repeat some.
	l := library(songs(1, 80, 3*mn, music)...)
	clk := clocks.Clock{ID: 7, Name: "Day", Playlists: []clocks.ClockPlaylist{pl(music, clocks.RuleFill)}}
	settings := Settings{MinGapMS: 10 * sec}

	win := newWindow(nil)
	var starts []Play
	var firstHour map[uint64]bool
	for h := range 3 {
		start := hour10.Add(time.Duration(h) * time.Hour)
		seed := uint64(start.Unix()) //nolint:gosec // G115: hour starts are after 1970
		plan := Build(HourInput{
			Start: start, Location: yerevan, Clock: clk, Library: l, Settings: settings, Seed: seed,
			History: win.forHour(start, l.maxRuleInterval()),
		})
		win.setHour(start, playsOf(start, plan.Items, l))

		require.NotEmpty(t, plan.Items, "hour %d", h)
		if h == 0 {
			firstHour = map[uint64]bool{}
		}
		for _, it := range plan.Items {
			at := start.Add(time.Duration(it.OffsetMS) * time.Millisecond)
			for _, p := range starts {
				if p.TrackID == it.TrackID {
					require.GreaterOrEqual(t, at.Sub(p.Start), 72*time.Hour,
						"track %d plays again at %s, inside its interval of %s", it.TrackID, at, p.Start)
				}
			}
			starts = append(starts, Play{TrackID: it.TrackID, Start: at})
			if h == 0 {
				firstHour[it.TrackID] = true
			}
		}
	}
	require.NotEmpty(t, win.plays, "the window keeps the built plays")

	// The check above means something only if the second hour would repeat
	// a track of the first without the plays of the first.
	second := hour10.Add(time.Hour)
	seed := uint64(second.Unix()) //nolint:gosec // G115: hour starts are after 1970
	naive := Build(HourInput{Start: second, Location: yerevan, Clock: clk, Library: l, Settings: settings, Seed: seed})

	repeats := 0
	for _, it := range naive.Items {
		if firstHour[it.TrackID] {
			repeats++
		}
	}
	require.Positive(t, repeats, "without the history of the first hour the second repeats no track")
}

func TestCheckSimulateDays(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name     string
		from, to time.Time
		ok       bool
	}{
		{"one day", day(1), day(1), true},
		{"31 days", day(1), day(31), true},
		{"32 days", day(1), day(1).AddDate(0, 0, 31), false},
		{"to before from", day(2), day(1), false},
	} {
		err := checkSimulateDays(tc.from, tc.to)
		if tc.ok {
			require.NoError(t, err, tc.name)
		} else {
			require.Error(t, err, tc.name)
		}
	}
}
