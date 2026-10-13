package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

// Retention periods. They become admin settings in Phase 4 (§6.3); the play
// log period also bounds separation checks (§4.1).
const (
	playLogRetention    = 10 * 24 * time.Hour
	operationsRetention = 10 * 24 * time.Hour
)

const (
	// retentionBatch caps the rows of one DELETE, so locks stay short.
	retentionBatch = 5000
	// batchTimeout bounds one DELETE batch.
	batchTimeout = 30 * time.Second
)

// Execer runs a statement. *pgxpool.Pool implements it.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// OperationsCleaner deletes finished operations.
type OperationsCleaner interface {
	DeleteFinishedBefore(ctx context.Context, cutoff time.Time, batchSize int) (int64, error)
}

// Retention deletes aired play log rows and resolved missed items older than
// 10 days (§4.1), and finished operations older than 10 days (§6.5). Open
// missed items are never deleted. Log files are cleaned by the log rotator.
type Retention struct {
	db  Execer
	ops OperationsCleaner
	now func() time.Time
}

// NewRetention returns the retention job. now is the clock.
func NewRetention(db Execer, ops OperationsCleaner, now func() time.Time) *Retention {
	return &Retention{db: db, ops: ops, now: now}
}

// Job returns the schedule: once a day, first a few minutes after startup.
func (r *Retention) Job() Job {
	return Job{
		Kind:      operations.KindRetention,
		Component: logging.ComponentRetention,
		FirstRun:  5 * time.Minute,
		Interval:  24 * time.Hour,
		Timeout:   30 * time.Minute,
		Run:       r.Run,
	}
}

// Run performs one retention pass and returns a summary.
func (r *Retention) Run(ctx context.Context) (string, error) {
	now := r.now().UTC()
	plays, err := r.deletePlayLog(ctx, now.Add(-playLogRetention))
	if err != nil {
		return "", err
	}
	ops, err := r.ops.DeleteFinishedBefore(ctx, now.Add(-operationsRetention), retentionBatch)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("deleted %d play log rows and %d operations", plays, ops), nil
}

// deletePlayLog deletes in batches. The scan has no supporting index: the
// table holds about 10 days of rows and runs once a day, so a sequential
// scan per batch is cheap (no speculative index, §1.2 of the init plan).
func (r *Retention) deletePlayLog(ctx context.Context, cutoff time.Time) (int64, error) {
	var total int64
	for {
		bctx, cancel := context.WithTimeout(ctx, batchTimeout)
		tag, err := r.db.Exec(bctx, `
			DELETE FROM play_log
			WHERE id IN (
				SELECT id FROM play_log
				WHERE (status IN ('aired', 'truncated') AND started_at < $1)
				   OR (status = 'missed' AND resolution IN ('rescheduled', 'dismissed') AND resolved_at < $1)
				LIMIT $2
			)`,
			cutoff, retentionBatch,
		)
		cancel()
		if err != nil {
			return total, fmt.Errorf("delete old play log rows: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < retentionBatch {
			return total, nil
		}
	}
}
