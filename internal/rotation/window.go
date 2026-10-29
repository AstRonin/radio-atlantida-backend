package rotation

import (
	"slices"
	"time"
)

// window is the history a run of builds reads, kept in memory for the run:
// the aired, queued and planned plays around its hours. The plays of the
// hours it builds replace their planned plays, so each hour is built with
// the history the planner would read in its own transaction: the plays
// before it, the stored plan of the hours after it, and the items of the
// hours built earlier in the run.
type window struct {
	plays []Play
}

// newWindow returns a window over plays (any order).
func newWindow(plays []Play) *window {
	return &window{plays: plays}
}

// forHour returns the history of the hour that starts at start: the plays
// in [start-span, start+1h+span), without the planned plays of the hour
// itself, which the build replaces (Store.History does the same).
func (w *window) forHour(start time.Time, span time.Duration) []Play {
	from, end := start.Add(-span), start.Add(time.Hour+span)
	hourEnd := start.Add(time.Hour)
	var out []Play
	for _, p := range w.plays {
		if p.Start.Before(from) || !p.Start.Before(end) {
			continue
		}
		if p.Planned && !p.Start.Before(start) && p.Start.Before(hourEnd) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// setHour replaces the planned plays of the hour that starts at start with
// plays: the items the hour was built with, or none once it is cleared. The
// plays it adds must be marked Planned.
func (w *window) setHour(start time.Time, plays []Play) {
	hourEnd := start.Add(time.Hour)
	w.plays = slices.DeleteFunc(w.plays, func(p Play) bool {
		return p.Planned && !p.Start.Before(start) && p.Start.Before(hourEnd)
	})
	w.plays = append(w.plays, plays...)
}

// playsOf returns the plays of the items of the hour that starts at start,
// marked planned.
func playsOf(start time.Time, items []Item, lib *Library) []Play {
	out := make([]Play, len(items))
	for i, it := range items {
		t, _ := lib.track(it.TrackID)
		out[i] = Play{
			TrackID: it.TrackID, ArtistID: t.ArtistID, Planned: true,
			Start: start.Add(time.Duration(it.OffsetMS) * time.Millisecond),
		}
	}
	return out
}
