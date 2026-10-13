package retention

import (
	"context"
	"time"
)

// DeletePlayLog deletes aired and truncated rows started before cutoff, and
// rescheduled or dismissed missed rows resolved before cutoff. Open missed
// rows are never deleted.
//
// The scan has no supporting index: the table holds about 10 days of rows
// and this runs once a day, so a sequential scan per batch is cheap (no
// speculative index, §1.2 of the init plan).
func (s *Store) DeletePlayLog(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.deleteInBatches(ctx, "old play log rows", `
		DELETE FROM play_log
		WHERE id IN (
			SELECT id FROM play_log
			WHERE (status IN ('aired', 'truncated') AND started_at < $1)
			   OR (status = 'missed' AND resolution IN ('rescheduled', 'dismissed') AND resolved_at < $1)
			LIMIT $2
		)`, cutoff)
}
