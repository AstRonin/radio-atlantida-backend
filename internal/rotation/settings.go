package rotation

import (
	"context"
	"time"
)

// PlanSettings are the settings the service reads before each sync.
type PlanSettings struct {
	Location *time.Location
	// PlanHorizonDays is how far ahead the 5-minute pass keeps the plan
	// (rotation.plan_horizon_days).
	PlanHorizonDays int
	// GridHorizonDays is how far ahead the grid is planned; the full pass
	// rebuilds up to it (rotation.grid_horizon_days).
	GridHorizonDays int
	// FullPassEnabled, FullPassTime (HH:MM station local), FullBatchHours,
	// FullSlowMS and FullHeapWarnMB configure the full pass and its logs
	// (rotation.full_pass_*).
	FullPassEnabled bool
	FullPassTime    string
	FullBatchHours  int
	FullSlowMS      int
	FullHeapWarnMB  int
	// Freeze is how long before its start an hour is frozen: playout may
	// have pushed its first items to Liquidsoap (playout.prepush_minutes),
	// so a built hour is no longer rebuilt or cleared, and its one-time
	// inserts are not changed.
	Freeze time.Duration
	// DefaultFixedToleranceMS is the start tolerance of a one-time insert
	// when the request gives none (rotation.default_fixed_tolerance_ms).
	DefaultFixedToleranceMS int
	Build                   Settings
}

// frozen reports whether the hour that starts at start is on air, over, or
// so close to its start that playout may have pushed its items.
func (p PlanSettings) frozen(start, now time.Time) bool {
	return !start.After(now.Add(p.Freeze))
}

// SettingsFunc reads the settings.
type SettingsFunc func(ctx context.Context) (PlanSettings, error)

// planHorizon returns the end of the 5-minute pass's horizon: the plan
// covers the hours that start up to rotation.plan_horizon_days after now.
func planHorizon(now time.Time, set PlanSettings) time.Time {
	return now.Add(time.Duration(set.PlanHorizonDays) * 24 * time.Hour)
}
