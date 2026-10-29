package jobs

import (
	"context"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

// TrackPlanFull schedules the nightly full pass of the track plan
// (internal/rotation): it is due once a day at rotation.full_pass_time
// (station local), when due reports so; the check runs every 5 minutes, and a
// run that is not due creates no operation. timeout bounds one pass.
func TrackPlanFull(run operations.Func, due func(ctx context.Context) (bool, error), timeout time.Duration) Job {
	return Job{
		Kind:      operations.KindTrackPlanFull,
		Component: logging.ComponentRotation,
		FirstRun:  time.Minute,
		Interval:  5 * time.Minute,
		Timeout:   timeout,
		Due:       due,
		Run:       run,
	}
}

// TrackPlan schedules the plan job (internal/rotation): reminders whose
// deadline passed, then the plan extended to its horizon (§7). The design
// asks for an hourly extension; every 5 minutes also keeps reminder
// deadlines (5 minutes at the shortest) on time and catches up after a
// rebuild that could not be started. A run with nothing to do reads the
// grid and the plan hours only.
func TrackPlan(run operations.Func) Job {
	return Job{
		Kind:      operations.KindTrackPlan,
		Component: logging.ComponentRotation,
		FirstRun:  30 * time.Second,
		Interval:  5 * time.Minute,
		Timeout:   10 * time.Minute,
		Run:       run,
	}
}
