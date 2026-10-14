package playrules

import "slices"

// AllowsAt reports whether the time window and the weekdays of the rule let
// a track start at minute t of weekday day (ISO, 1 = Monday ... 7 = Sunday),
// station local time. A window includes its start and excludes its end. A
// window that crosses midnight belongs to the weekday on which it starts, so
// its part after midnight is allowed on the day after a listed weekday.
//
// The minimum interval and the per-hour limit depend on what is planned and
// aired, so they are not checked here.
func (r RuleInput) AllowsAt(day int, t TimeOfDay) bool {
	if r.WindowFrom == nil || r.WindowTo == nil {
		return r.allowsDay(day)
	}
	from, to := *r.WindowFrom, *r.WindowTo
	if from < to {
		return t >= from && t < to && r.allowsDay(day)
	}
	// Crosses midnight: [from, 24:00) today, [00:00, to) the next day.
	if t >= from {
		return r.allowsDay(day)
	}
	return t < to && r.allowsDay(previousDay(day))
}

func (r RuleInput) allowsDay(day int) bool {
	return r.Weekdays == nil || slices.Contains(r.Weekdays, day)
}

func previousDay(day int) int {
	if day == 1 {
		return 7
	}
	return day - 1
}
