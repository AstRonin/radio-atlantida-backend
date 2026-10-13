package retention

import (
	"context"
	"time"
)

// DeleteOperations deletes finished operations that finished before cutoff.
// Running operations are kept.
func (s *Store) DeleteOperations(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.deleteInBatches(ctx, "old operations", `
		DELETE FROM operations
		WHERE id IN (
			SELECT id FROM operations
			WHERE status <> 'running' AND finished_at < $1
			LIMIT $2
		)`, cutoff)
}
