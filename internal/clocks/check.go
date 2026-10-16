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

// Warning codes of the dry check (§6.0, §7.3). They do not block the save.
// The sample build adds the warning codes of rotation.
const (
	// WarnNoTracks: a playlist of the clock or an insert has no track that
	// can be planned.
	WarnNoTracks WarningCode = "playlist_no_tracks"
	// WarnFewTracks: fewer fill tracks than the clock plays within the
	// minimum interval of their play rules, counted from its grid hours.
	WarnFewTracks WarningCode = "fill_few_tracks"
	// WarnNoIntervalRule: no play rule of a fill playlist sets a minimum
	// interval, so its tracks may repeat within the hour.
	WarnNoIntervalRule WarningCode = "fill_no_interval_rule"
	// WarnBlockedByPlayRules: in some grid hours of the clock, the play
	// rule windows block every track of a rule or insert playlist (§7.3).
	WarnBlockedByPlayRules WarningCode = "rule_blocked_by_play_rules"
)

// WarningCodes lists every warning code of the dry check.
func WarningCodes() []WarningCode {
	return []WarningCode{WarnNoTracks, WarnFewTracks, WarnNoIntervalRule, WarnBlockedByPlayRules}
}

// WeekdayHour is an hour of the week, station local.
type WeekdayHour struct {
	Weekday int
	Hour    int
}

// Warning is a problem the dry check found. PlaylistIndex and InsertIndex
// are indexes into the saved clock.
type Warning struct {
	Code          WarningCode
	Message       string
	PlaylistIndex *int
	InsertIndex   *int
	// Hours are the blocked hours of WarnBlockedByPlayRules.
	Hours []WeekdayHour
}

// warnings runs the dry check of a saved clock (§6.0): playable tracks per
// playlist and insert, enough fill tracks for the minimum interval of their
// rules, a fill playlist without an interval rule, and play rule windows in
// the clock's grid hours.
func (s *Service) warnings(ctx context.Context, c Clock) ([]Warning, error) {
	var playlists []uint64
	for _, p := range c.Playlists {
		playlists = append(playlists, p.PlaylistID)
	}
	for _, ins := range c.Inserts {
		if ins.PlaylistID != nil {
			playlists = append(playlists, *ins.PlaylistID)
		}
	}
	cands, err := s.check.Candidates(ctx, uniq(playlists))
	if err != nil {
		return nil, fmt.Errorf("read candidates: %w", err)
	}
	hours, err := s.gridHours(ctx, c.ID)
	if err != nil {
		return nil, err
	}

	var out []Warning
	var fillTracks []Candidate
	fillIDs := map[uint64]bool{}
	for i, p := range c.Playlists {
		playable := playableOf(cands.Tracks, p.PlaylistID)
		rule := p.EffectiveRule()
		if len(playable) == 0 {
			out = append(out, Warning{
				Code: WarnNoTracks, PlaylistIndex: new(i),
				Message: "the playlist has no track that can be planned (with a duration)",
			})
			continue
		}
		if rule == RuleFill {
			for _, t := range playable {
				if !fillIDs[t.TrackID] {
					fillIDs[t.TrackID] = true
					fillTracks = append(fillTracks, t)
				}
			}
			if !hasInterval(cands.PlaylistRules[p.PlaylistID], cands.Rules) {
				out = append(out, Warning{
					Code: WarnNoIntervalRule, PlaylistIndex: new(i),
					Message: "no play rule of the playlist sets a minimum interval; its tracks may repeat within the hour",
				})
			}
			continue
		}
		first, last := 0, 59
		if rule == RuleMiddle {
			first, last = 25, 35
		}
		if w, ok := blockedHours(playable, cands.Rules, hours, first, last); ok {
			w.PlaylistIndex = new(i)
			w.Message = fmt.Sprintf("play rules block every track of the playlist in %d hours of the grid; its slot is skipped there", len(w.Hours))
			out = append(out, w)
		}
	}
	for i, ins := range c.Inserts {
		if ins.PlaylistID == nil {
			continue
		}
		playable := playableOf(cands.Tracks, *ins.PlaylistID)
		if len(playable) == 0 {
			out = append(out, Warning{
				Code: WarnNoTracks, InsertIndex: new(i),
				Message: "the insert's playlist has no track that can be planned (with a duration)",
			})
			continue
		}
		first := max(0, ins.OffsetMS-ins.ToleranceMS) / 60_000
		last := min(HourMS-1, ins.OffsetMS+ins.ToleranceMS) / 60_000
		if w, ok := blockedHours(playable, cands.Rules, hours, first, last); ok {
			w.InsertIndex = new(i)
			w.Message = fmt.Sprintf("play rules block every track of the insert's playlist in %d hours of the grid; the insert is skipped there", len(w.Hours))
			out = append(out, w)
		}
	}
	if w, ok := fewTracks(fillTracks, cands.Rules, hours, s.now().UTC()); ok {
		out = append(out, w)
	}
	return out, nil
}

// gridHours returns the grid hours of the clock within the grid horizon.
func (s *Service) gridHours(ctx context.Context, clockID uint64) ([]Hour, error) {
	set, err := s.settings(ctx)
	if err != nil {
		return nil, fmt.Errorf("read clock settings: %w", err)
	}
	now := s.now().UTC()
	hours, err := s.grid.ClockHours(ctx, clockID, now, now.AddDate(0, 0, set.GridHorizonDays))
	if err != nil {
		return nil, fmt.Errorf("read grid hours of clock %d: %w", clockID, err)
	}
	return hours, nil
}

func playableOf(tracks []Candidate, playlistID uint64) []Candidate {
	var out []Candidate
	for _, t := range tracks {
		if t.AirMS != nil && slices.Contains(t.PlaylistIDs, playlistID) {
			out = append(out, t)
		}
	}
	return out
}

func hasInterval(ruleIDs []uint64, rules map[uint64]playrules.RuleInput) bool {
	return slices.ContainsFunc(ruleIDs, func(id uint64) bool {
		r, ok := rules[id]
		return ok && r.MinIntervalMinutes != nil
	})
}

// fewTracks warns when the fill playlists have fewer tracks than the clock
// plays within the minimum interval of their rules: each track may air once
// in that interval. The interval is the shortest one that covers every fill
// track; tracks without one are counted as free. The plays per hour come
// from the average length of the fill tracks. Other clocks that share the
// playlists are not counted.
func fewTracks(fill []Candidate, rules map[uint64]playrules.RuleInput, hours []Hour, now time.Time) (Warning, bool) {
	if len(fill) == 0 {
		return Warning{}, false
	}
	interval, total := 0, 0
	for _, t := range fill {
		longest := 0
		for _, id := range t.RuleIDs {
			if r, ok := rules[id]; ok && r.MinIntervalMinutes != nil {
				longest = max(longest, *r.MinIntervalMinutes)
			}
		}
		if longest == 0 {
			return Warning{}, false // a track that may repeat at once never runs out
		}
		if interval == 0 || longest < interval {
			interval = longest
		}
		total += *t.AirMS
	}
	end := now.Add(time.Duration(interval) * time.Minute)
	inWindow := 0
	for _, h := range hours {
		if h.StartUTC.Before(end) {
			inWindow++
		}
	}
	perHour := (HourMS*len(fill) + total - 1) / total // ceil(hour / average length)
	plays := perHour * inWindow
	if len(fill) >= plays {
		return Warning{}, false
	}
	return Warning{
		Code: WarnFewTracks,
		Message: fmt.Sprintf("%d fill tracks for about %d plays within the minimum interval of their play rules; the hours run short of allowed tracks",
			len(fill), plays),
	}, true
}

// blockedHours finds the grid hours in which the time windows of the play
// rules block every track at every minute first ... last of the hour. Only
// the window and weekdays are checked: the interval and per-hour limits
// depend on the plan.
func blockedHours(tracks []Candidate, rules map[uint64]playrules.RuleInput, hours []Hour, first, last int) (Warning, bool) {
	// Tracks with the same rules behave the same; one check per rule set.
	groups := map[string][]playrules.RuleInput{}
	for _, t := range tracks {
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
	seen := map[WeekdayHour]bool{}
	var blocked []WeekdayHour
	for _, h := range hours {
		wh := WeekdayHour{Weekday: h.Weekday, Hour: h.Hour}
		if seen[wh] {
			continue
		}
		seen[wh] = true
		if !anyAllowed(groups, wh, first, last) {
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
	return Warning{Code: WarnBlockedByPlayRules, Hours: blocked}, true
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

func ruleKey(ids []uint64) string {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	parts := make([]string, len(sorted))
	for i, id := range sorted {
		parts[i] = strconv.FormatUint(id, 10)
	}
	return strings.Join(parts, ",")
}
