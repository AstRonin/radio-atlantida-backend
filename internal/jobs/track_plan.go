package jobs

import (
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

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
