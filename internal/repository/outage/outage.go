// Package outage is the pgx store of internal/outage: broadcast outages,
// the backend heartbeat, and the plan items an outage affects (§5.1).
package outage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
)

// Store implements outage.Store.
type Store struct {
	q db.Querier
}

// New returns the store.
func New(q db.Querier) *Store { return &Store{q: q} }

// WithTx returns the store bound to tx.
func (s *Store) WithTx(tx db.Tx) outage.Store { return &Store{q: tx.Pgx()} }

// planLockKey is the advisory lock of plan writes. It must equal planLockKey
// of internal/repository/rotation: the planner holds it while it builds an
// hour, playout while it marks an item queued.
const planLockKey int64 = 0x726f74706c616e // "rotplan"

// LockPlan takes the transaction-level advisory lock of plan writes.
func (s *Store) LockPlan(ctx context.Context) error {
	if _, err := s.q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, planLockKey); err != nil {
		return fmt.Errorf("lock plan: %w", err)
	}
	return nil
}

// itemEnd is the planned end of a scheduled_items row si.
const itemEnd = `(si.planned_start_utc + make_interval(secs => (si.planned_out_ms - si.planned_in_ms) / 1000.0))`

const outageColumns = `id, started_at, ended_at, detected_by, component, reason, reconciled_at`

type outageRow struct {
	ID           uint64     `db:"id"`
	StartedAt    time.Time  `db:"started_at"`
	EndedAt      *time.Time `db:"ended_at"`
	DetectedBy   string     `db:"detected_by"`
	Component    string     `db:"component"`
	Reason       *string    `db:"reason"`
	ReconciledAt *time.Time `db:"reconciled_at"`
}

func (r outageRow) domain() outage.Outage {
	return outage.Outage{
		ID: r.ID, StartedAt: r.StartedAt.UTC(), EndedAt: utcPtr(r.EndedAt), DetectedBy: outage.DetectedBy(r.DetectedBy),
		Component: outage.Component(r.Component), Reason: r.Reason, ReconciledAt: utcPtr(r.ReconciledAt),
	}
}

// OpenOutage returns the open outage.
func (s *Store) OpenOutage(ctx context.Context) (outage.Outage, bool, error) {
	rows, err := s.q.Query(ctx, `SELECT `+outageColumns+` FROM broadcast_outages WHERE ended_at IS NULL`)
	if err != nil {
		return outage.Outage{}, false, fmt.Errorf("select open outage: %w", err)
	}
	o, err := db.CollectOne(rows, outageRow.domain, errNone)
	switch {
	case errors.Is(err, errNone):
		return outage.Outage{}, false, nil
	case err != nil:
		return outage.Outage{}, false, fmt.Errorf("scan open outage: %w", err)
	}
	return o, true, nil
}

var errNone = errors.New("no row")

// CreateOutage records an outage; uq_broadcast_outages_open refuses a
// second open one.
func (s *Store) CreateOutage(ctx context.Context, o outage.NewOutage, endedAt *time.Time) (outage.Outage, error) {
	var reason *string
	if o.Reason != "" {
		reason = &o.Reason
	}
	rows, err := s.q.Query(ctx, `INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component, reason)
VALUES ($1, $2, $3, $4, $5) RETURNING `+outageColumns,
		o.StartedAt, endedAt, string(o.DetectedBy), string(o.Component), reason)
	if err != nil {
		return outage.Outage{}, fmt.Errorf("insert outage: %w", err)
	}
	out, err := db.CollectOne(rows, outageRow.domain, errNone)
	if ce, ok := db.AsConstraintError(err); ok && ce.Kind == db.Unique && ce.Constraint == "uq_broadcast_outages_open" {
		return outage.Outage{}, outage.ErrOutageOpen
	}
	if err != nil {
		return outage.Outage{}, fmt.Errorf("insert outage: %w", err)
	}
	return out, nil
}

// CloseOutage sets the end of an open outage.
func (s *Store) CloseOutage(ctx context.Context, id uint64, endedAt time.Time) error {
	tag, err := s.q.Exec(ctx, `UPDATE broadcast_outages SET ended_at = GREATEST($2, started_at)
WHERE id = $1 AND ended_at IS NULL`, id, endedAt)
	if err != nil {
		return fmt.Errorf("close outage %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("close outage %d: not open", id)
	}
	return nil
}

// Unmarked returns the closed outages not marked yet, oldest first.
func (s *Store) Unmarked(ctx context.Context) ([]outage.Outage, error) {
	rows, err := s.q.Query(ctx, `SELECT `+outageColumns+` FROM broadcast_outages
WHERE ended_at IS NOT NULL AND reconciled_at IS NULL ORDER BY started_at`)
	if err != nil {
		return nil, fmt.Errorf("select unmarked outages: %w", err)
	}
	out, err := db.CollectAll(rows, outageRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan unmarked outages: %w", err)
	}
	return out, nil
}

// MarkMissed moves the items of the outage window that never started to
// the play log as missed (§4.1, §7.1). One statement deletes the plan rows
// and inserts the log rows. A missed row keeps the plan's times, clock, and
// slot; the reason is read from its outage. A filler pool row stays used:
// the track was planned.
//
// The outage window is short and ix_scheduled_items_planned_start_utc
// bounds the scan to the plan rows that start before before.
func (s *Store) MarkMissed(ctx context.Context, o outage.Outage, before time.Time) (int, error) {
	if o.EndedAt == nil {
		return 0, fmt.Errorf("mark missed items of outage %d: the outage is open", o.ID)
	}
	tag, err := s.q.Exec(ctx, `WITH moved AS (
    DELETE FROM scheduled_items si
    WHERE si.status IN ('planned', 'queued')
      AND si.planned_start_utc < LEAST($3::timestamptz, $4::timestamptz)
      AND `+itemEnd+` > $2
    RETURNING si.id, si.track_id, si.clock_id, si.grid_slot_id, si.planned_start_utc, si.planned_in_ms, si.planned_out_ms
)
INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, planned_in_ms, planned_out_ms, truncated, source,
                      clock_id, grid_slot_id, scheduled_item_id, outage_id, resolution)
SELECT m.track_id, t.artist_id, 'missed', m.planned_start_utc, m.planned_in_ms, m.planned_out_ms, false, 'auto',
       m.clock_id, m.grid_slot_id, m.id, $1, 'open'
FROM moved m
JOIN tracks t ON t.id = m.track_id
ON CONFLICT (scheduled_item_id) WHERE scheduled_item_id IS NOT NULL DO NOTHING`,
		o.ID, o.StartedAt, *o.EndedAt, before)
	if err != nil {
		return 0, fmt.Errorf("mark missed items of outage %d: %w", o.ID, err)
	}
	return int(tag.RowsAffected()), nil
}

// SetReconciled records that the outage's missed items are marked.
func (s *Store) SetReconciled(ctx context.Context, id uint64, at time.Time) error {
	if _, err := s.q.Exec(ctx, `UPDATE broadcast_outages SET reconciled_at = $2 WHERE id = $1`, id, at); err != nil {
		return fmt.Errorf("set outage %d reconciled: %w", id, err)
	}
	return nil
}

// OldestUnaired returns the start of the oldest planned or queued item that
// starts before before (ix_scheduled_items_planned_start_utc).
func (s *Store) OldestUnaired(ctx context.Context, before time.Time) (time.Time, bool, error) {
	var t *time.Time
	err := s.q.QueryRow(ctx, `SELECT min(planned_start_utc) FROM scheduled_items
WHERE status IN ('planned', 'queued') AND planned_start_utc < $1`, before).Scan(&t)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("select oldest unaired item: %w", err)
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return t.UTC(), true, nil
}

// CountUnaired counts the planned and queued items that start in [from, to).
func (s *Store) CountUnaired(ctx context.Context, from, to time.Time) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `SELECT count(*) FROM scheduled_items
WHERE status IN ('planned', 'queued') AND planned_start_utc >= $1 AND planned_start_utc < $2`, from, to).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count unaired items: %w", err)
	}
	return n, nil
}

// DropStale deletes the items that did not air while no outage was open
// (docs/phase-10-plan.md §2, decision 5): planned or queued, starting
// before before, overlapping no open outage and no closed one that is not
// marked yet. Their pool rows are released, as for a rebuild, and a
// rescheduled item that did not air opens its missed item again.
func (s *Store) DropStale(ctx context.Context, before time.Time) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `WITH dropped AS (
    DELETE FROM scheduled_items si
    WHERE si.status IN ('planned', 'queued') AND si.planned_start_utc < $1
      AND NOT EXISTS (
          SELECT 1 FROM broadcast_outages o
          WHERE (o.ended_at IS NULL OR o.reconciled_at IS NULL)
            AND `+itemEnd+` > o.started_at
            AND (o.ended_at IS NULL OR si.planned_start_utc < o.ended_at))
    RETURNING si.filler_pool_id, si.reschedule_of_id
), released AS (
    UPDATE clock_filler_pool SET used_at = NULL
    WHERE id IN (SELECT filler_pool_id FROM dropped WHERE filler_pool_id IS NOT NULL)
), reopened AS (
    UPDATE play_log SET resolution = 'open', resolved_at = NULL, resolved_item_id = NULL
    WHERE id IN (SELECT reschedule_of_id FROM dropped WHERE reschedule_of_id IS NOT NULL)
      AND status = 'missed' AND resolution = 'rescheduled'
)
SELECT count(*) FROM dropped`, before).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("drop unaired items: %w", err)
	}
	return n, nil
}

// ItemsDue reports whether a planned or queued item plays within
// [from, to). An item is shorter than an hour, so the scan is bounded by
// ix_scheduled_items_planned_start_utc.
func (s *Store) ItemsDue(ctx context.Context, from, to time.Time) (bool, error) {
	var due bool
	err := s.q.QueryRow(ctx, `SELECT EXISTS (
    SELECT 1 FROM scheduled_items si
    WHERE si.status IN ('planned', 'queued')
      AND si.planned_start_utc < $2::timestamptz AND si.planned_start_utc > $1::timestamptz - interval '1 hour'
      AND `+itemEnd+` > $1::timestamptz)`, from, to).Scan(&due)
	if err != nil {
		return false, fmt.Errorf("select due items: %w", err)
	}
	return due, nil
}

// Heartbeat returns when the backend was last alive.
func (s *Store) Heartbeat(ctx context.Context) (time.Time, bool, error) {
	var t time.Time
	err := s.q.QueryRow(ctx, `SELECT alive_at FROM backend_heartbeat`).Scan(&t)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("select backend heartbeat: %w", err)
	}
	return t.UTC(), true, nil
}

// SetHeartbeat records that the backend is alive.
func (s *Store) SetHeartbeat(ctx context.Context, at time.Time) error {
	_, err := s.q.Exec(ctx, `INSERT INTO backend_heartbeat (alive_at) VALUES ($1)
ON CONFLICT (id) DO UPDATE SET alive_at = EXCLUDED.alive_at`, at)
	if err != nil {
		return fmt.Errorf("upsert backend heartbeat: %w", err)
	}
	return nil
}

// Outages returns the outages that overlap [from, to), newest first, with
// the number of items marked missed (ix_play_log_outage_id).
func (s *Store) Outages(ctx context.Context, from, to time.Time) ([]outage.Outage, error) {
	rows, err := s.q.Query(ctx, `SELECT o.id, o.started_at, o.ended_at, o.detected_by, o.component, o.reason,
       o.reconciled_at,
       (SELECT count(*) FROM play_log pl WHERE pl.outage_id = o.id AND pl.status = 'missed') AS missed_count
FROM broadcast_outages o
WHERE o.started_at < $2 AND (o.ended_at IS NULL OR o.ended_at > $1)
ORDER BY o.started_at DESC, o.id DESC`, from, to)
	if err != nil {
		return nil, fmt.Errorf("select outages: %w", err)
	}
	out, err := db.CollectAll(rows, func(r countedOutageRow) outage.Outage {
		o := r.domain()
		o.MissedCount = r.MissedCount
		return o
	})
	if err != nil {
		return nil, fmt.Errorf("scan outages: %w", err)
	}
	return out, nil
}

type countedOutageRow struct {
	outageRow
	MissedCount int `db:"missed_count"`
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return new(t.UTC())
}
