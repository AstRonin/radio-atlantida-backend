package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// MaxSimulateDays bounds one simulation: the station local dates of the
// range, both ends included.
const MaxSimulateDays = 31

// simulateTimeout bounds one simulation. It is longer than hourTimeout
// because one run builds up to MaxSimulateDays * 24 hours. Measured on the
// dev stack with a 2,000-track library: 31 days in about 2.8 s without the
// race detector (about 19 s with it). The HTTP route times out at 30 s, so
// the route, not this constant, is the real limit for a long run. Nothing is
// written, so a long run holds no lock.
const simulateTimeout = 30 * time.Second

// SimulatedItem is a track of a simulated hour, with what the response shows.
type SimulatedItem struct {
	Start      time.Time
	TrackID    uint64
	Title      string
	ArtistName *string
	// InMS and OutMS are the in and out points from the start of the file.
	InMS, OutMS int
	Truncated   bool
	Source      Source
}

// SimulatedHour is one grid hour built by a simulation.
type SimulatedHour struct {
	Start     time.Time
	ClockID   uint64
	ClockName string
	ItemCount int
	Warnings  []Warning
	// Items are set only when the simulation asks for them.
	Items []SimulatedItem
}

// Simulation is the result of building a range of grid hours. Nothing is
// stored: the plan, the scheduled items and the one-time inserts are read,
// never written.
type Simulation struct {
	// From and To are the station local dates of the request, inclusive.
	From, To time.Time
	Hours    []SimulatedHour
	// Skipped counts the grid hours whose clock no longer exists.
	Skipped int
	// Items is the number of items of all built hours.
	Items int
	// Warnings counts the warnings of all built hours by code.
	Warnings map[WarningCode]int
	Elapsed  time.Duration
	// Queries counts the store calls of the run: the settings, the grid, the
	// library, the window (history and stored plan), the inserts of the range,
	// and one per distinct clock. A store call may run several statements.
	Queries int
	// LibraryTracks is the number of tracks of the snapshot the build read: the
	// tracks of the playlists of the clocks in the range, and their inserts.
	LibraryTracks int
	// HistoryPlays is the number of plays read before From: the history
	// the first hour is built with.
	HistoryPlays int
}

// Simulate builds every grid hour of the station local dates from ... to,
// inclusive, in time order, as the planner would build them, and reports the
// result without storing it. from and to are station local dates (midnight
// UTC, as the API parses them). withItems adds the items of each hour to the
// result.
//
// The history is the one the planner reads (window): the aired and queued
// plays, and the stored plan inside the range, so a range that already has a
// plan gives the same items. Each simulated hour replaces the planned items it
// had with its own, so the hours after it see them, as they would after a
// rebuild. Built hours that the grid no longer assigns are not cleared here:
// their items still count as history.
func (s *Service) Simulate(ctx context.Context, from, to time.Time, withItems bool) (Simulation, error) {
	if err := checkSimulateDays(from, to); err != nil {
		return Simulation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, simulateTimeout)
	defer cancel()
	began := s.now()
	out := Simulation{From: from, To: to, Warnings: map[WarningCode]int{}}
	count := &queryCount{}
	st := countingStore{Store: s.store, n: count}

	set, err := s.settings(ctx)
	if err != nil {
		return Simulation{}, fmt.Errorf("read rotation settings: %w", err)
	}
	count.add(1)
	loc := set.Location
	first := localMidnight(from, loc)
	end := localMidnight(to.AddDate(0, 0, 1), loc)
	hours, err := s.grid.Hours(ctx, first, end)
	if err != nil {
		return Simulation{}, fmt.Errorf("read grid hours: %w", err)
	}
	count.add(1)

	cache, err := s.clocksOf(ctx, hours, count)
	if err != nil {
		return Simulation{}, err
	}
	inserts, err := st.InsertsRange(ctx, first, end)
	if err != nil {
		return Simulation{}, fmt.Errorf("read hour inserts: %w", err)
	}
	// The snapshot holds the playlists of the clocks of the range; it is read
	// once for the whole simulation.
	var used []clocks.Clock
	for _, c := range cache {
		if c != nil {
			used = append(used, *c)
		}
	}
	lib, err := libraryFor(ctx, st, scopeOf(used, inserts))
	if err != nil {
		return Simulation{}, err
	}
	span := lib.maxRuleInterval()
	plays, err := st.Window(ctx, first.Add(-span), end.Add(span))
	if err != nil {
		return Simulation{}, fmt.Errorf("read history: %w", err)
	}
	for _, p := range plays {
		if p.Start.Before(first) {
			out.HistoryPlays++
		}
	}
	out.LibraryTracks = len(lib.Tracks)

	win := newWindow(plays)
	byHour := map[int64][]Insert{}
	for _, ins := range inserts {
		byHour[ins.HourStart.Unix()] = append(byHour[ins.HourStart.Unix()], ins)
	}
	for _, h := range hours {
		c := cache[h.Clock.ID]
		if c == nil {
			out.Skipped++
			continue
		}
		plan := Build(HourInput{
			Start: h.StartUTC, Location: loc, Clock: *c, HourInserts: byHour[h.StartUTC.Unix()], Library: &lib,
			History: win.forHour(h.StartUTC, span), Settings: set.Build,
			Seed: uint64(h.StartUTC.Unix()), //nolint:gosec // G115: hour starts are after 1970
		})
		win.setHour(h.StartUTC, playsOf(h.StartUTC, plan.Items, &lib))
		hour := SimulatedHour{
			Start: h.StartUTC, ClockID: c.ID, ClockName: c.Name, ItemCount: len(plan.Items), Warnings: plan.Warnings,
		}
		if withItems {
			hour.Items = simulatedItems(&lib, h.StartUTC, plan.Items)
		}
		out.Hours = append(out.Hours, hour)
		out.Items += len(plan.Items)
		for _, w := range plan.Warnings {
			out.Warnings[w.Code]++
		}
	}
	if withItems {
		var ids []uint64
		for _, h := range out.Hours {
			for _, it := range h.Items {
				ids = append(ids, it.TrackID)
			}
		}
		titles, err := titlesOf(ctx, st, ids)
		if err != nil {
			return Simulation{}, err
		}
		for i := range out.Hours {
			for j := range out.Hours[i].Items {
				t := titles[out.Hours[i].Items[j].TrackID]
				out.Hours[i].Items[j].Title = t.Title
				out.Hours[i].Items[j].ArtistName = t.ArtistName
			}
		}
	}
	out.Queries = count.n
	out.Elapsed = s.now().Sub(began)
	return out, nil
}

// checkSimulateDays refuses a range that is empty or longer than
// MaxSimulateDays.
func checkSimulateDays(from, to time.Time) error {
	switch {
	case to.Before(from):
		return validation.Field("query.to", validation.Range, "must not be before from")
	case int(to.Sub(from)/(24*time.Hour))+1 > MaxSimulateDays:
		return validation.Field("query.to", validation.Range, "at most %d days per request", MaxSimulateDays)
	}
	return nil
}

// localMidnight returns the instant of midnight of the station local date
// of d (d is a date, at midnight UTC).
func localMidnight(d time.Time, loc *time.Location) time.Time {
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc).UTC()
}

// simulatedItems returns the items of a built hour that starts at start, with
// the titles of the library.
func simulatedItems(lib *Library, start time.Time, items []Item) []SimulatedItem {
	out := make([]SimulatedItem, len(items))
	for i, it := range items {
		t, _ := lib.track(it.TrackID)
		out[i] = SimulatedItem{
			Start: start.Add(time.Duration(it.OffsetMS) * time.Millisecond), TrackID: it.TrackID,
			Title: t.Title, ArtistName: t.ArtistName, InMS: it.InMS, OutMS: it.OutMS,
			Truncated: it.Truncated, Source: it.Source,
		}
	}
	return out
}
