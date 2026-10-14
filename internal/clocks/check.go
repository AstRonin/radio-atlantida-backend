package clocks

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
)

// WarningCode is the stable code of a dry check warning; the panel
// translates by code.
type WarningCode string

// Warning codes (§6.0, §7.3). They do not block the save.
const (
	// WarnNoTracks: no track of the element's playlists has a duration.
	WarnNoTracks WarningCode = "element_no_tracks"
	// WarnNoFittingTrack: no track lies within nominal ± tolerance.
	WarnNoFittingTrack WarningCode = "element_no_fitting_track"
	// WarnFewTracks: fewer fitting tracks than the element draws within the
	// track separation window, counted from the grid hours of the clock.
	WarnFewTracks WarningCode = "element_few_tracks"
	// WarnBlockedByPlayRules: in some grid hours of the clock, the play
	// rule windows block every fitting track (§7.3).
	WarnBlockedByPlayRules WarningCode = "element_blocked_by_play_rules"
	// WarnSegmentOverflow: the last segment runs past the end of the hour;
	// the crossing track is cut there (§7).
	WarnSegmentOverflow WarningCode = "segment_overflow"
)

// WarningCodes lists every warning code.
func WarningCodes() []WarningCode {
	return []WarningCode{WarnNoTracks, WarnNoFittingTrack, WarnFewTracks, WarnBlockedByPlayRules, WarnSegmentOverflow}
}

// WeekdayHour is an hour of the week, station local.
type WeekdayHour struct {
	Weekday int
	Hour    int
}

// Warning is a problem the dry check found. Element and Segment are indexes
// into the saved clock.
type Warning struct {
	Code    WarningCode
	Message string
	Element *int
	Segment *int
	// Hours are the blocked hours of WarnBlockedByPlayRules.
	Hours []WeekdayHour
}

// warnings runs the dry check of a saved clock (§6.0): available tracks per
// element, their fit to the tolerance, enough tracks for the separation
// window, play rule windows in the clock's grid hours, and overflow.
func (s *Service) warnings(ctx context.Context, c Clock, l Layout) ([]Warning, error) {
	var out []Warning
	last := l.Segments[len(l.Segments)-1]
	if last.OverflowMS > 0 {
		out = append(out, Warning{
			Code: WarnSegmentOverflow, Segment: new(last.Index),
			Message: fmt.Sprintf("the elements run %d s past the end of the hour; the crossing track is cut and faded out", (last.OverflowMS+999)/1000),
		})
	}

	var playlists []int64
	for _, e := range c.Elements {
		playlists = append(playlists, e.PlaylistIDs...)
	}
	cands, err := s.check.Candidates(ctx, uniq(playlists))
	if err != nil {
		return nil, fmt.Errorf("read candidates: %w", err)
	}
	hours, sepEnd, err := s.gridHours(ctx, c.ID)
	if err != nil {
		return nil, err
	}

	for i, e := range c.Elements {
		var withDuration, fitting []Candidate
		for _, t := range cands.Tracks {
			if !intersects(t.PlaylistIDs, e.PlaylistIDs) || t.DurationMS == nil {
				continue
			}
			withDuration = append(withDuration, t)
			if abs(*t.DurationMS-e.NominalDurationMS) <= e.DurationToleranceMS {
				fitting = append(fitting, t)
			}
		}
		switch {
		case len(withDuration) == 0:
			out = append(out, Warning{
				Code: WarnNoTracks, Element: new(i),
				Message: "the element's playlists have no track with a duration",
			})
			continue
		case len(fitting) == 0:
			out = append(out, Warning{
				Code: WarnNoFittingTrack, Element: new(i),
				Message: fmt.Sprintf("no track lasts %s ± %d s", clockTime(e.NominalDurationMS), e.DurationToleranceMS/1000),
			})
			continue
		}
		if w, ok := fewTracks(c.Elements, i, len(fitting), hours, sepEnd); ok {
			out = append(out, w)
		}
		if w, ok := blockedHours(e, i, fitting, cands.Rules, hours); ok {
			out = append(out, w)
		}
	}
	return out, nil
}

// gridHours returns the grid hours of the clock within the grid horizon,
// and the end of the track separation window.
func (s *Service) gridHours(ctx context.Context, clockID int64) ([]Hour, time.Time, error) {
	set, err := s.settings(ctx)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read clock settings: %w", err)
	}
	sepHours, err := s.check.TrackSeparationHours(ctx)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read track separation: %w", err)
	}
	now := s.now().UTC()
	hours, err := s.grid.ClockHours(ctx, clockID, now, now.AddDate(0, 0, set.GridHorizonDays))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read grid hours of clock %d: %w", clockID, err)
	}
	return hours, now.Add(time.Duration(sepHours) * time.Hour), nil
}

// fewTracks warns when an element has fewer fitting tracks than it draws
// within the track separation window: each track may air once in that
// window. Elements of the clock with the same playlists draw from the same
// tracks. Other clocks that share the playlists are not counted.
func fewTracks(elements []Element, i, fitting int, hours []Hour, sepEnd time.Time) (Warning, bool) {
	inWindow := 0
	for _, h := range hours {
		if h.StartUTC.Before(sepEnd) {
			inWindow++
		}
	}
	perHour := 0
	for _, e := range elements {
		if slices.Equal(uniq(e.PlaylistIDs), uniq(elements[i].PlaylistIDs)) {
			perHour++
		}
	}
	draws := perHour * inWindow
	if fitting >= draws {
		return Warning{}, false
	}
	return Warning{
		Code: WarnFewTracks, Element: new(i),
		Message: fmt.Sprintf("%d fitting tracks for %d plays within the track separation window; some slots will stay empty",
			fitting, draws),
	}, true
}

// blockedHours warns about the grid hours in which the time windows of the
// play rules block every fitting track of the element. Only the window and
// weekdays are checked: the interval and per-hour limits depend on the plan.
func blockedHours(e Element, i int, fitting []Candidate, rules map[int64]playrules.RuleInput, hours []Hour) (Warning, bool) {
	// Tracks with the same rules behave the same; one check per rule set.
	groups := map[string][]playrules.RuleInput{}
	for _, t := range fitting {
		var rs []playrules.RuleInput
		for _, id := range t.RuleIDs {
			if r, ok := rules[id]; ok && (r.WindowFrom != nil || r.Weekdays != nil) {
				rs = append(rs, r)
			}
		}
		if len(rs) == 0 {
			return Warning{}, false // a track without time limits fits every hour
		}
		groups[ruleKey(t.RuleIDs)] = rs
	}

	first, lastMin := 0, 59
	if e.Placement == PlacementFixed {
		first = max(0, e.earliest()/minuteMS)
		lastMin = min(59, (e.target()+e.FixedToleranceMS)/minuteMS)
	}
	seen := map[WeekdayHour]bool{}
	var blocked []WeekdayHour
	for _, h := range hours {
		wh := WeekdayHour{Weekday: h.Weekday, Hour: h.Hour}
		if seen[wh] {
			continue
		}
		seen[wh] = true
		if !anyAllowed(groups, wh, first, lastMin) {
			blocked = append(blocked, wh)
		}
	}
	if len(blocked) == 0 {
		return Warning{}, false
	}
	slices.SortFunc(blocked, func(a, b WeekdayHour) int {
		if a.Weekday != b.Weekday {
			return a.Weekday - b.Weekday
		}
		return a.Hour - b.Hour
	})
	return Warning{
		Code: WarnBlockedByPlayRules, Element: new(i), Hours: blocked,
		Message: fmt.Sprintf("play rules block every fitting track in %d hours of the grid; the element is skipped there", len(blocked)),
	}, true
}

// anyAllowed reports whether some rule group lets a track start at some
// minute first ... last of the hour.
func anyAllowed(groups map[string][]playrules.RuleInput, wh WeekdayHour, first, last int) bool {
	for _, rs := range groups {
		for m := first; m <= last; m++ {
			t := playrules.TimeOfDay(wh.Hour*60 + m)
			if !slices.ContainsFunc(rs, func(r playrules.RuleInput) bool { return !r.AllowsAt(wh.Weekday, t) }) {
				return true
			}
		}
	}
	return false
}

func ruleKey(ids []int64) string {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	parts := make([]string, len(sorted))
	for i, id := range sorted {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

func intersects(a, b []int64) bool {
	return slices.ContainsFunc(a, func(x int64) bool { return slices.Contains(b, x) })
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
