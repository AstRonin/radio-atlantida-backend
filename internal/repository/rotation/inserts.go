package rotation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

type missedRow struct {
	ID         uint64 `db:"id"`
	TrackID    uint64 `db:"track_id"`
	Resolution string `db:"resolution"`
}

func (r missedRow) domain() rotation.MissedItem {
	return rotation.MissedItem{ID: r.ID, TrackID: r.TrackID, Resolution: outage.Resolution(r.Resolution)}
}

const missedQuery = `SELECT id, track_id, resolution FROM play_log WHERE id = $1 AND status = 'missed'`

// Missed returns a missed item.
func (s *Store) Missed(ctx context.Context, id uint64) (rotation.MissedItem, error) {
	return s.missed(ctx, id, missedQuery)
}

// LockMissed returns a missed item locked for update, so a dismiss cannot
// race the reschedule.
func (s *Store) LockMissed(ctx context.Context, id uint64) (rotation.MissedItem, error) {
	return s.missed(ctx, id, missedQuery+` FOR UPDATE`)
}

func (s *Store) missed(ctx context.Context, id uint64, query string) (rotation.MissedItem, error) {
	rows, err := s.q.Query(ctx, query, id)
	if err != nil {
		return rotation.MissedItem{}, fmt.Errorf("select missed item %d: %w", id, err)
	}
	m, err := db.CollectOne(rows, missedRow.domain, outage.ErrMissedNotFound)
	if err != nil && !errors.Is(err, outage.ErrMissedNotFound) {
		return rotation.MissedItem{}, fmt.Errorf("scan missed item %d: %w", id, err)
	}
	return m, err
}

type insertRow struct {
	ID               uint64    `db:"id"`
	HourStart        time.Time `db:"hour_start_utc"`
	TrackID          uint64    `db:"track_id"`
	MissedID         *uint64   `db:"missed_id"`
	Placement        string    `db:"placement"`
	FixedMinute      *int      `db:"fixed_minute"`
	FixedToleranceMS int       `db:"fixed_tolerance_ms"`
}

func (r insertRow) domain() rotation.Insert {
	return rotation.Insert{
		ID: r.ID, HourStart: r.HourStart.UTC(), TrackID: r.TrackID, MissedID: r.MissedID,
		Placement: clocks.Placement(r.Placement), FixedMinute: r.FixedMinute, FixedToleranceMS: r.FixedToleranceMS,
	}
}

// Inserts returns the inserts of the hour, in ID order
// (ix_hour_inserts_hour_start_utc).
func (s *Store) Inserts(ctx context.Context, hourStart time.Time) ([]rotation.Insert, error) {
	rows, err := s.q.Query(ctx, `SELECT id, hour_start_utc, track_id, missed_id, placement, fixed_minute, fixed_tolerance_ms
FROM hour_inserts WHERE hour_start_utc = $1 ORDER BY id`, hourStart)
	if err != nil {
		return nil, fmt.Errorf("select hour inserts: %w", err)
	}
	out, err := db.CollectAll(rows, insertRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan hour inserts: %w", err)
	}
	return out, nil
}

// CreateInsert stores an insert.
func (s *Store) CreateInsert(ctx context.Context, ins rotation.Insert) (uint64, error) {
	var id uint64
	err := s.q.QueryRow(ctx, `INSERT INTO hour_inserts (hour_start_utc, track_id, missed_id, placement, fixed_minute, fixed_tolerance_ms)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		ins.HourStart, ins.TrackID, ins.MissedID, string(ins.Placement), ins.FixedMinute, ins.FixedToleranceMS).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert hour insert: %w", err)
	}
	return id, nil
}

// DeleteInsert deletes an insert.
func (s *Store) DeleteInsert(ctx context.Context, id uint64) error {
	if _, err := s.q.Exec(ctx, `DELETE FROM hour_inserts WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete hour insert %d: %w", id, err)
	}
	return nil
}

// ReopenMissed sets a rescheduled missed item open again.
func (s *Store) ReopenMissed(ctx context.Context, missedID uint64) error {
	_, err := s.q.Exec(ctx, `UPDATE play_log SET resolution = 'open', resolved_at = NULL, resolved_item_id = NULL
WHERE id = $1 AND status = 'missed' AND resolution = 'rescheduled'`, missedID)
	if err != nil {
		return fmt.Errorf("reopen missed item %d: %w", missedID, err)
	}
	return nil
}

// LinkRescheduled resolves a missed item as rescheduled to the plan row
// that airs it again. A rebuild of the hour makes a new row, so the link
// is set on every build; resolved_at keeps the first resolution.
func (s *Store) LinkRescheduled(ctx context.Context, missedID uint64, at time.Time) error {
	_, err := s.q.Exec(ctx, `UPDATE play_log pl SET resolution = 'rescheduled',
    resolved_at = CASE WHEN pl.resolution = 'rescheduled' THEN pl.resolved_at ELSE $2 END,
    resolved_item_id = si.id
FROM scheduled_items si
WHERE pl.id = $1 AND pl.status = 'missed' AND si.reschedule_of_id = pl.id`, missedID, at)
	if err != nil {
		return fmt.Errorf("link rescheduled item of missed item %d: %w", missedID, err)
	}
	return nil
}
