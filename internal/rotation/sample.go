package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// SampleItem is a track of a sample hour, with what the panel shows.
type SampleItem struct {
	Item
	Title      string
	ArtistName *string
	// LengthMS is how long it plays: OutMS - InMS.
	LengthMS int
}

// Sample is an hour of a clock built with the real library and history but
// not stored (§6.0): it shows how the hour is laid out. The planned hours
// may differ by seed and history.
type Sample struct {
	HourStart time.Time
	Items     []SampleItem
	Warnings  []Warning
}

// Sample builds the sample hour of a saved clock: the next hour the grid
// assigns to it inside the plan horizon (with that hour's one-time
// inserts), else the next station hour.
func (s *Service) Sample(ctx context.Context, c clocks.Clock) (Sample, error) {
	ctx, cancel := context.WithTimeout(ctx, hourTimeout)
	defer cancel()
	set, err := s.settings(ctx)
	if err != nil {
		return Sample{}, fmt.Errorf("read rotation settings: %w", err)
	}
	now := s.now().UTC()
	hours, err := s.grid.Hours(ctx, now.Add(time.Nanosecond), now.Add(time.Duration(set.PlanHorizonHours)*time.Hour))
	if err != nil {
		return Sample{}, fmt.Errorf("read grid hours: %w", err)
	}
	start, planned := nextStationHour(now, set.Location), false
	for _, h := range hours {
		if h.Clock.ID == c.ID {
			start, planned = h.StartUTC, true
			break
		}
	}
	lib, err := s.store.Library(ctx)
	if err != nil {
		return Sample{}, fmt.Errorf("read library: %w", err)
	}
	span := lib.maxRuleInterval()
	history, err := s.store.History(ctx, start.Add(-span), start.Add(time.Hour+span), start)
	if err != nil {
		return Sample{}, fmt.Errorf("read history: %w", err)
	}
	var inserts []Insert
	if planned {
		if inserts, err = s.store.Inserts(ctx, start); err != nil {
			return Sample{}, fmt.Errorf("read hour inserts: %w", err)
		}
	}
	plan := Build(HourInput{
		Start: start, Location: set.Location, Clock: c, HourInserts: inserts, Library: &lib, History: history,
		Settings: set.Build, Seed: uint64(start.Unix()), //nolint:gosec // G115: hour starts are after 1970
	})
	out := Sample{HourStart: start, Warnings: plan.Warnings, Items: make([]SampleItem, len(plan.Items))}
	for i, it := range plan.Items {
		t, _ := lib.track(it.TrackID)
		out.Items[i] = SampleItem{Item: it, Title: t.Title, ArtistName: t.ArtistName, LengthMS: it.OutMS - it.InMS}
	}
	return out, nil
}

// nextStationHour returns the start of the station hour after now.
func nextStationHour(now time.Time, loc *time.Location) time.Time {
	l := now.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc).Add(time.Hour).UTC()
}
