package rotation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

type planHourRow struct {
	Start           time.Time `db:"hour_start_utc"`
	ClockID         uint64    `db:"clock_id"`
	ClockUpdatedAt  time.Time `db:"clock_updated_at"`
	GridSlotID      *uint64   `db:"grid_slot_id"`
	Seed            int64     `db:"seed"`
	BuiltAt         time.Time `db:"built_at"`
	FillerRequestID *uint64   `db:"filler_request_id"`
}

func (r planHourRow) domain() rotation.PlanHour {
	return rotation.PlanHour{
		Start: r.Start.UTC(), ClockID: r.ClockID, ClockUpdatedAt: r.ClockUpdatedAt.UTC(), GridSlotID: r.GridSlotID,
		Seed: r.Seed, BuiltAt: r.BuiltAt.UTC(), FillerRequestID: r.FillerRequestID,
	}
}

const planHourColumns = `hour_start_utc, clock_id, clock_updated_at, grid_slot_id, seed, built_at, filler_request_id`

// PlanHours returns the built hours that start in [from, to) (pk_plan_hours).
func (s *Store) PlanHours(ctx context.Context, from, to time.Time) ([]rotation.PlanHour, error) {
	rows, err := s.q.Query(ctx, `SELECT `+planHourColumns+` FROM plan_hours
WHERE hour_start_utc >= $1 AND hour_start_utc < $2 ORDER BY hour_start_utc`, from, to)
	if err != nil {
		return nil, fmt.Errorf("select plan hours: %w", err)
	}
	out, err := db.CollectAll(rows, planHourRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan plan hours: %w", err)
	}
	return out, nil
}

var errNoHour = errors.New("no plan hour")

// PlanHour returns one built hour; false if there is none.
func (s *Store) PlanHour(ctx context.Context, start time.Time) (rotation.PlanHour, bool, error) {
	rows, err := s.q.Query(ctx, `SELECT `+planHourColumns+` FROM plan_hours WHERE hour_start_utc = $1`, start)
	if err != nil {
		return rotation.PlanHour{}, false, fmt.Errorf("select plan hour: %w", err)
	}
	h, err := db.CollectOne(rows, planHourRow.domain, errNoHour)
	switch {
	case errors.Is(err, errNoHour):
		return rotation.PlanHour{}, false, nil
	case err != nil:
		return rotation.PlanHour{}, false, fmt.Errorf("scan plan hour: %w", err)
	}
	return h, true, nil
}

// releasePool and deletePlanned work on the planned items of one hour
// [$1, $2); queued, aired, and missed items are never touched (§7).
const (
	releasePool = `
UPDATE clock_filler_pool SET used_at = NULL
WHERE id IN (SELECT filler_pool_id FROM scheduled_items
             WHERE status = 'planned' AND planned_start_utc >= $1 AND planned_start_utc < $2
               AND filler_pool_id IS NOT NULL)`
	deletePlanned = `
DELETE FROM scheduled_items
WHERE status = 'planned' AND planned_start_utc >= $1 AND planned_start_utc < $2`
)

// ReplaceHour replaces the planned items of the hour with items and stores
// the hour. Pool rows taken by the old items are released first, then the
// new items' rows are marked used, so a row taken again stays used.
func (s *Store) ReplaceHour(ctx context.Context, h rotation.PlanHour, items []rotation.Item) error {
	end := h.Start.Add(time.Hour)
	if _, err := s.q.Exec(ctx, releasePool, h.Start, end); err != nil {
		return fmt.Errorf("release pool rows: %w", err)
	}
	if _, err := s.q.Exec(ctx, deletePlanned, h.Start, end); err != nil {
		return fmt.Errorf("delete planned items: %w", err)
	}

	rows := make([][]any, len(items))
	var used []uint64
	for i, it := range items {
		rows[i] = []any{
			h.GridSlotID, h.ClockID, it.TrackID, h.Start.Add(time.Duration(it.OffsetMS) * time.Millisecond),
			it.InMS, it.OutMS, it.Truncated, string(it.Source), it.ElementID, it.PoolRowID, it.RescheduleOfID,
		}
		if it.PoolRowID != nil {
			used = append(used, *it.PoolRowID)
		}
	}
	if len(rows) > 0 {
		_, err := s.q.CopyFrom(ctx, pgx.Identifier{"scheduled_items"}, []string{
			"grid_slot_id", "clock_id", "track_id", "planned_start_utc",
			"planned_in_ms", "planned_out_ms", "truncated", "source", "clock_element_id", "filler_pool_id", "reschedule_of_id",
		}, pgx.CopyFromRows(rows))
		if err != nil {
			return fmt.Errorf("insert planned items: %w", err)
		}
	}
	if len(used) > 0 {
		if _, err := s.q.Exec(ctx, `UPDATE clock_filler_pool SET used_at = $1 WHERE id = ANY($2)`, h.BuiltAt, used); err != nil {
			return fmt.Errorf("mark pool rows used: %w", err)
		}
	}
	_, err := s.q.Exec(ctx, `
INSERT INTO plan_hours (`+planHourColumns+`)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (hour_start_utc) DO UPDATE SET
    clock_id = EXCLUDED.clock_id, clock_updated_at = EXCLUDED.clock_updated_at,
    grid_slot_id = EXCLUDED.grid_slot_id, seed = EXCLUDED.seed, built_at = EXCLUDED.built_at,
    filler_request_id = EXCLUDED.filler_request_id`,
		h.Start, h.ClockID, h.ClockUpdatedAt, h.GridSlotID, h.Seed, h.BuiltAt, h.FillerRequestID)
	if err != nil {
		return fmt.Errorf("store plan hour: %w", err)
	}
	return nil
}

// ClearHour deletes the planned items of the hour, releasing their pool
// rows, and its record.
func (s *Store) ClearHour(ctx context.Context, start time.Time) error {
	end := start.Add(time.Hour)
	if _, err := s.q.Exec(ctx, releasePool, start, end); err != nil {
		return fmt.Errorf("release pool rows: %w", err)
	}
	if _, err := s.q.Exec(ctx, deletePlanned, start, end); err != nil {
		return fmt.Errorf("delete planned items: %w", err)
	}
	if _, err := s.q.Exec(ctx, `DELETE FROM plan_hours WHERE hour_start_utc = $1`, start); err != nil {
		return fmt.Errorf("delete plan hour: %w", err)
	}
	return nil
}

type plannedHourRow struct {
	planHourRow
	ClockName string `db:"clock_name"`
}

type plannedItemRow struct {
	ID         uint64    `db:"id"`
	TrackID    uint64    `db:"track_id"`
	Title      string    `db:"title"`
	ArtistName *string   `db:"artist_name"`
	DurationMS *int32    `db:"duration_ms"`
	ElementID  *uint64   `db:"clock_element_id"`
	Start      time.Time `db:"planned_start_utc"`
	InMS       int32     `db:"planned_in_ms"`
	OutMS      int32     `db:"planned_out_ms"`
	Truncated  bool      `db:"truncated"`
	Source     string    `db:"source"`
	Status     string    `db:"status"`
}

func (r plannedItemRow) domain() rotation.PlannedItem {
	out := rotation.PlannedItem{
		ID: r.ID, TrackID: r.TrackID, Title: r.Title, ArtistName: r.ArtistName, ElementID: r.ElementID,
		Start: r.Start.UTC(), InMS: int(r.InMS), OutMS: int(r.OutMS), Truncated: r.Truncated, Source: r.Source, Status: r.Status,
	}
	if r.DurationMS != nil {
		out.DurationMS = int(*r.DurationMS)
	}
	return out
}

// Plan returns the built hours that start in [from, to) with their items in
// time order. An item belongs to the latest hour that starts at or before
// it.
func (s *Store) Plan(ctx context.Context, from, to time.Time) ([]rotation.PlannedHour, error) {
	rows, err := s.q.Query(ctx, `
SELECT h.hour_start_utc, h.clock_id, h.clock_updated_at, h.grid_slot_id, h.seed, h.built_at,
       h.filler_request_id, c.name AS clock_name
FROM plan_hours h
JOIN clocks c ON c.id = h.clock_id
WHERE h.hour_start_utc >= $1 AND h.hour_start_utc < $2
ORDER BY h.hour_start_utc`, from, to)
	if err != nil {
		return nil, fmt.Errorf("select plan hours: %w", err)
	}
	hours, err := db.CollectAll(rows, func(r plannedHourRow) rotation.PlannedHour {
		return rotation.PlannedHour{PlanHour: r.domain(), ClockName: r.ClockName}
	})
	if err != nil {
		return nil, fmt.Errorf("scan plan hours: %w", err)
	}
	if len(hours) == 0 {
		return hours, nil
	}

	rows, err = s.q.Query(ctx, `
SELECT si.id, si.track_id, t.title, a.name AS artist_name, t.duration_ms, si.clock_element_id,
       si.planned_start_utc, si.planned_in_ms, si.planned_out_ms, si.truncated, si.source, si.status
FROM scheduled_items si
JOIN tracks t ON t.id = si.track_id
LEFT JOIN artists a ON a.id = t.artist_id
WHERE si.planned_start_utc >= $1 AND si.planned_start_utc < $2
ORDER BY si.planned_start_utc, si.id`, hours[0].Start, hours[len(hours)-1].Start.Add(time.Hour))
	if err != nil {
		return nil, fmt.Errorf("select planned items: %w", err)
	}
	items, err := db.CollectAll(rows, plannedItemRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan planned items: %w", err)
	}
	h := 0
	for _, it := range items {
		for h+1 < len(hours) && !it.Start.Before(hours[h+1].Start) {
			h++
		}
		if it.Start.Before(hours[h].Start.Add(time.Hour)) {
			hours[h].Items = append(hours[h].Items, it)
		}
	}
	return hours, nil
}
