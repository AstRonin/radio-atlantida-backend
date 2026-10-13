// Package retention holds the retention rules: how long aired history and
// finished operations are kept (§4.1 and §6.5 of the design). The scheduler
// in internal/jobs runs it; the deletes are in internal/repository/retention.
package retention

import (
	"context"
	"fmt"
	"time"
)

// Retention periods. They become admin settings in Phase 5 (§6.3); the play
// log period also bounds separation checks (§4.1).
const (
	PlayLogPeriod    = 10 * 24 * time.Hour
	OperationsPeriod = 10 * 24 * time.Hour
)

// Store deletes expired rows (internal/repository/retention). Each method
// deletes in short batches, so a large backlog never holds long locks.
type Store interface {
	// DeletePlayLog deletes aired and truncated rows that started before
	// cutoff, and resolved missed rows resolved before cutoff. Open missed
	// rows are never deleted: they stay until the manager resolves them.
	DeletePlayLog(ctx context.Context, cutoff time.Time) (int64, error)
	// DeleteOperations deletes finished operations that finished before
	// cutoff. Running operations are kept.
	DeleteOperations(ctx context.Context, cutoff time.Time) (int64, error)
}

// Service applies the retention rules.
type Service struct {
	store Store
	now   func() time.Time
}

// New returns the service. now is the clock.
func New(store Store, now func() time.Time) *Service {
	return &Service{store: store, now: now}
}

// Run performs one retention pass and returns a summary. Log files are
// cleaned by the log rotator, not here.
func (s *Service) Run(ctx context.Context) (string, error) {
	now := s.now().UTC()
	plays, err := s.store.DeletePlayLog(ctx, now.Add(-PlayLogPeriod))
	if err != nil {
		return "", fmt.Errorf("play log retention: %w", err)
	}
	ops, err := s.store.DeleteOperations(ctx, now.Add(-OperationsPeriod))
	if err != nil {
		return "", fmt.Errorf("operations retention: %w", err)
	}
	return fmt.Sprintf("deleted %d play log rows and %d operations", plays, ops), nil
}
