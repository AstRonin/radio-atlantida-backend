package rotation

import (
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
)

// checker decides whether a track may start at a time by its play rules
// (§7.3), against the history and what the build planned so far. The rules
// are never relaxed.
type checker struct {
	loc   *time.Location
	rules map[uint64]playrules.RuleInput
	// Starts per track and per artist, sorted.
	byTrack  map[uint64][]time.Time
	byArtist map[uint64][]time.Time
}

func newChecker(in HourInput) *checker {
	c := &checker{
		loc: in.Location, rules: in.Library.Rules,
		byTrack: map[uint64][]time.Time{}, byArtist: map[uint64][]time.Time{},
	}
	for _, p := range in.History {
		c.add(p.TrackID, p.ArtistID, p.Start)
	}
	return c
}

// add records a start.
func (c *checker) add(trackID uint64, artistID *uint64, at time.Time) {
	insert(c.byTrack, trackID, at)
	if artistID != nil {
		insert(c.byArtist, *artistID, at)
	}
}

func insert(m map[uint64][]time.Time, key uint64, at time.Time) {
	ts := m[key]
	i, _ := slices.BinarySearchFunc(ts, at, time.Time.Compare)
	m[key] = slices.Insert(ts, i, at)
}

// within reports whether a start in ts lies less than d from at, before or
// after it.
func within(ts []time.Time, at time.Time, d time.Duration) bool {
	if d <= 0 || len(ts) == 0 {
		return false
	}
	i, _ := slices.BinarySearchFunc(ts, at, time.Time.Compare)
	if i < len(ts) && ts[i].Sub(at) < d {
		return true
	}
	return i > 0 && at.Sub(ts[i-1]) < d
}

// allowed reports whether every play rule of the track lets it start at at:
// the time window and weekdays (station local), the minimum interval
// between its starts, the minimum interval between starts of its artist
// (from any playlist; a track without an artist has none), and the starts
// per local clock hour. A rule deleted since the library was read does not
// limit.
func (c *checker) allowed(t Track, at time.Time) bool {
	local := at.In(c.loc)
	day := (int(local.Weekday())+6)%7 + 1
	tod := playrules.TimeOfDay(local.Hour()*60 + local.Minute())
	starts := c.byTrack[t.ID]
	for _, id := range t.RuleIDs {
		r, ok := c.rules[id]
		if !ok {
			continue
		}
		if !r.AllowsAt(day, tod) {
			return false
		}
		if r.MinIntervalMinutes != nil && within(starts, at, time.Duration(*r.MinIntervalMinutes)*time.Minute) {
			return false
		}
		if r.ArtistMinIntervalMinutes != nil && t.ArtistID != nil &&
			within(c.byArtist[*t.ArtistID], at, time.Duration(*r.ArtistMinIntervalMinutes)*time.Minute) {
			return false
		}
		if r.MaxPerHour != nil && startsInHour(starts, local) >= *r.MaxPerHour {
			return false
		}
	}
	return true
}

// startsInHour counts the starts in the station-local clock hour of local.
func startsInHour(starts []time.Time, local time.Time) int {
	from := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, local.Location())
	to := from.Add(time.Hour)
	lo, _ := slices.BinarySearchFunc(starts, from, time.Time.Compare)
	hi, _ := slices.BinarySearchFunc(starts, to, time.Time.Compare)
	return hi - lo
}
