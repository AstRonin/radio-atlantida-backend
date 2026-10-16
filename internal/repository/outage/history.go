package outage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
)

// entrySelect reads play_log rows pl as entries of the played-tracks list.
// sort_time is the expression of ix_play_log_sort_time.
const entrySelect = `SELECT pl.id, pl.status, pl.source, pl.track_id, t.title, a.name AS artist_name,
       pl.planned_start_utc, pl.started_at, pl.planned_in_ms, pl.planned_out_ms, pl.actual_duration_ms,
       pl.clock_id, c.name AS clock_name, pl.outage_id, o.component AS outage_component, o.reason AS outage_reason,
       pl.resolution, pl.resolved_at, pl.resolved_item_id,
       COALESCE(pl.started_at, pl.planned_start_utc) AS sort_time
FROM play_log pl
JOIN tracks t ON t.id = pl.track_id
LEFT JOIN artists a ON a.id = t.artist_id
LEFT JOIN clocks c ON c.id = pl.clock_id
LEFT JOIN broadcast_outages o ON o.id = pl.outage_id`

type entryRow struct {
	ID               uint64     `db:"id"`
	Status           string     `db:"status"`
	Source           string     `db:"source"`
	TrackID          uint64     `db:"track_id"`
	Title            string     `db:"title"`
	ArtistName       *string    `db:"artist_name"`
	PlannedStart     *time.Time `db:"planned_start_utc"`
	StartedAt        *time.Time `db:"started_at"`
	PlannedInMS      int        `db:"planned_in_ms"`
	PlannedOutMS     *int       `db:"planned_out_ms"`
	ActualDurationMS *int       `db:"actual_duration_ms"`
	ClockID          *uint64    `db:"clock_id"`
	ClockName        *string    `db:"clock_name"`
	OutageID         *uint64    `db:"outage_id"`
	OutageComponent  *string    `db:"outage_component"`
	OutageReason     *string    `db:"outage_reason"`
	Resolution       *string    `db:"resolution"`
	ResolvedAt       *time.Time `db:"resolved_at"`
	ResolvedItemID   *uint64    `db:"resolved_item_id"`
	SortTime         time.Time  `db:"sort_time"`
}

func (r entryRow) domain() outage.Entry {
	e := outage.Entry{
		ID: r.ID, Status: outage.Status(r.Status), Source: r.Source, TrackID: r.TrackID, Title: r.Title,
		ArtistName: r.ArtistName, PlannedStart: utcPtr(r.PlannedStart), StartedAt: utcPtr(r.StartedAt),
		PlannedInMS: r.PlannedInMS, PlannedOutMS: r.PlannedOutMS, ActualDurationMS: r.ActualDurationMS,
		ClockID: r.ClockID, ClockName: r.ClockName,
		ResolvedAt: utcPtr(r.ResolvedAt), ResolvedItemID: r.ResolvedItemID, SortTime: r.SortTime.UTC(),
	}
	if r.OutageID != nil && r.OutageComponent != nil {
		e.Outage = &outage.EntryOutage{ID: *r.OutageID, Component: outage.Component(*r.OutageComponent), Reason: r.OutageReason}
	}
	if r.Resolution != nil {
		e.Resolution = new(outage.Resolution(*r.Resolution))
	}
	return e
}

// History returns a page of the played-tracks list, newest first, keyset
// paginated on (sort_time, id).
//
// EXPLAIN ANALYZE (PostgreSQL 17.11, TestExplainHistory: 10 days at 15
// plays per hour, the newest page of 100 of the last day):
//
//	Limit (actual rows=101 loops=1)
//	  ->  Nested Loop Left Join (actual rows=101 loops=1)  [artists, clocks, outages: pk lookups]
//	        ->  Nested Loop (actual rows=101 loops=1)
//	              ->  Index Scan using ix_play_log_sort_time on play_log pl (actual rows=101 loops=1)
//	                    Index Cond: ((COALESCE(started_at, planned_start_utc) >= ...) AND (... < ...))
//	              ->  Memoize -> Index Scan using pk_tracks on tracks t (actual rows=1 loops=1)
//	Planning Time: 0.672 ms
//	Execution Time: 0.120 ms
func (s *Store) History(ctx context.Context, f outage.HistoryFilter) ([]outage.Entry, error) {
	var status *string
	if f.Status != nil {
		status = new(string(*f.Status))
	}
	var afterTime *time.Time
	var afterID *uint64
	if f.After != nil {
		afterTime, afterID = &f.After.SortTime, &f.After.ID
	}
	rows, err := s.q.Query(ctx, entrySelect+`
WHERE COALESCE(pl.started_at, pl.planned_start_utc) >= $1
  AND COALESCE(pl.started_at, pl.planned_start_utc) < $2
  AND ($3::text IS NULL OR pl.status = $3)
  AND ($4::timestamptz IS NULL OR (COALESCE(pl.started_at, pl.planned_start_utc), pl.id) < ($4, $5::bigint))
ORDER BY COALESCE(pl.started_at, pl.planned_start_utc) DESC, pl.id DESC
LIMIT $6`, f.From, f.To, status, afterTime, afterID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("select play history: %w", err)
	}
	out, err := db.CollectAll(rows, entryRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan play history: %w", err)
	}
	return out, nil
}

// Missed returns the missed items with the resolution, newest planned
// first (ix_play_log_status_planned_start_utc).
func (s *Store) Missed(ctx context.Context, r outage.Resolution, limit int) ([]outage.Entry, error) {
	rows, err := s.q.Query(ctx, entrySelect+`
WHERE pl.status = 'missed' AND pl.resolution = $1
ORDER BY pl.planned_start_utc DESC, pl.id DESC
LIMIT $2`, string(r), limit)
	if err != nil {
		return nil, fmt.Errorf("select missed items: %w", err)
	}
	out, err := db.CollectAll(rows, entryRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan missed items: %w", err)
	}
	return out, nil
}

// Dismiss resolves an open missed item as dismissed and returns it.
func (s *Store) Dismiss(ctx context.Context, id uint64, at time.Time) (outage.Entry, error) {
	tag, err := s.q.Exec(ctx, `UPDATE play_log SET resolution = 'dismissed', resolved_at = $2
WHERE id = $1 AND status = 'missed' AND resolution = 'open'`, id, at)
	if err != nil {
		return outage.Entry{}, fmt.Errorf("dismiss missed item %d: %w", id, err)
	}
	e, err := s.missed(ctx, id)
	if err != nil {
		return outage.Entry{}, err
	}
	if tag.RowsAffected() == 0 {
		return outage.Entry{}, outage.ErrMissedResolved
	}
	return e, nil
}

// missed reads one missed item; outage.ErrMissedNotFound when the row is
// not a missed item.
func (s *Store) missed(ctx context.Context, id uint64) (outage.Entry, error) {
	rows, err := s.q.Query(ctx, entrySelect+` WHERE pl.id = $1 AND pl.status = 'missed'`, id)
	if err != nil {
		return outage.Entry{}, fmt.Errorf("select missed item %d: %w", id, err)
	}
	e, err := db.CollectOne(rows, entryRow.domain, outage.ErrMissedNotFound)
	if err != nil && !errors.Is(err, outage.ErrMissedNotFound) {
		return outage.Entry{}, fmt.Errorf("scan missed item %d: %w", id, err)
	}
	return e, err
}
