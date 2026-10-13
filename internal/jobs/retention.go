package jobs

import (
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

// Retention schedules the retention pass (internal/retention): once a day,
// first a few minutes after startup.
func Retention(run operations.Func) Job {
	return Job{
		Kind:      operations.KindRetention,
		Component: logging.ComponentRetention,
		FirstRun:  5 * time.Minute,
		Interval:  24 * time.Hour,
		Timeout:   30 * time.Minute,
		Run:       run,
	}
}
