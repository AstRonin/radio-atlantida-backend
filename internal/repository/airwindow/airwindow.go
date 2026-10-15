// Package airwindow is the pgx store of internal/airwindow: the last play
// log rows and the next plan items (§5.1).
package airwindow

import (
	"context"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/airwindow"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

// Store implements airwindow.Store.
type Store struct {
	q db.Querier
}

// New returns the store.
func New(q db.Querier) *Store { return &Store{q: q} }

// playedSQL reads the last play log rows by the moment they were due. The
// order is the expression of ix_play_log_sort_time.
const playedSQL = `SELECT pl.id, pl.scheduled_item_id, pl.status, pl.source, pl.track_id, t.title, a.name AS artist_name,
       pl.planned_start_utc, pl.started_at, pl.planned_out_ms, pl.actual_duration_ms, pl.resolution, pl.outage_id
FROM play_log pl
JOIN tracks t ON t.id = pl.track_id
LEFT JOIN artists a ON a.id = t.artist_id
WHERE pl.status IN ('aired', 'truncated', 'missed')
ORDER BY COALESCE(pl.started_at, pl.planned_start_utc) DESC, pl.id DESC
LIMIT $1`

type playedRow struct {
	ID               uint64     `db:"id"`
	ScheduledItemID  *uint64    `db:"scheduled_item_id"`
	Status           string     `db:"status"`
	Source           string     `db:"source"`
	TrackID          uint64     `db:"track_id"`
	Title            string     `db:"title"`
	ArtistName       *string    `db:"artist_name"`
	PlannedStart     *time.Time `db:"planned_start_utc"`
	StartedAt        *time.Time `db:"started_at"`
	PlannedOutMS     *int       `db:"planned_out_ms"`
	ActualDurationMS *int       `db:"actual_duration_ms"`
	Resolution       *string    `db:"resolution"`
	OutageID         *uint64    `db:"outage_id"`
}

func (r playedRow) domain() airwindow.Played {
	return airwindow.Played{
		ID: r.ID, ScheduledItemID: r.ScheduledItemID, Status: r.Status, Source: r.Source, TrackID: r.TrackID,
		Title: r.Title, ArtistName: r.ArtistName, PlannedStart: utcPtr(r.PlannedStart), StartedAt: utcPtr(r.StartedAt),
		PlannedOutMS: r.PlannedOutMS, ActualDurationMS: r.ActualDurationMS, Resolution: r.Resolution, OutageID: r.OutageID,
	}
}

// Played returns the last limit play log rows, newest first.
//
// EXPLAIN ANALYZE (PostgreSQL 17.11, TestExplainPlayed: 10 days at 15 plays
// per hour, limit 11):
//
//	Limit (actual rows=11 loops=1)
//	  ->  Nested Loop Left Join (actual rows=11 loops=1)
//	        ->  Nested Loop (actual rows=11 loops=1)
//	              ->  Index Scan using ix_play_log_sort_time on play_log pl (actual rows=11 loops=1)
//	                    Filter: (status = ANY ('{aired,truncated,missed}'::text[]))
//	              ->  Memoize -> Index Scan using pk_tracks on tracks t (actual rows=1 loops=1)
//	        ->  Index Scan using pk_artists on artists a (actual rows=1 loops=11)
//	Planning Time: 0.261 ms
//	Execution Time: 0.036 ms
func (s *Store) Played(ctx context.Context, limit int) ([]airwindow.Played, error) {
	rows, err := s.q.Query(ctx, playedSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("select played items: %w", err)
	}
	out, err := db.CollectAll(rows, playedRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan played items: %w", err)
	}
	return out, nil
}

// queuedSQL reads the next plan items that have not started.
const queuedSQL = `SELECT si.id, si.track_id, t.title, a.name AS artist_name, si.planned_start_utc,
       si.planned_in_ms, si.planned_out_ms, si.truncated, si.status = 'queued' AS in_liquidsoap
FROM scheduled_items si
JOIN tracks t ON t.id = si.track_id
LEFT JOIN artists a ON a.id = t.artist_id
WHERE si.status IN ('planned', 'queued') AND si.planned_start_utc >= $1
ORDER BY si.planned_start_utc, si.id
LIMIT $2`

type queuedRow struct {
	ID           uint64    `db:"id"`
	TrackID      uint64    `db:"track_id"`
	Title        string    `db:"title"`
	ArtistName   *string   `db:"artist_name"`
	Start        time.Time `db:"planned_start_utc"`
	InMS         int       `db:"planned_in_ms"`
	OutMS        int       `db:"planned_out_ms"`
	Truncated    bool      `db:"truncated"`
	InLiquidsoap bool      `db:"in_liquidsoap"`
}

func (r queuedRow) domain() airwindow.Queued {
	return airwindow.Queued{
		ID: r.ID, TrackID: r.TrackID, Title: r.Title, ArtistName: r.ArtistName, Start: r.Start.UTC(),
		InMS: r.InMS, OutMS: r.OutMS, Truncated: r.Truncated, InLiquidsoap: r.InLiquidsoap,
	}
}

// Queued returns the planned and queued items that start at or after
// from, in planned order.
//
// The start index is enough: items before from are skipped by the index
// condition, and after it nearly every item is planned or queued, so a
// (status, planned_start_utc) index would not be chosen (decision 12).
//
// EXPLAIN ANALYZE (PostgreSQL 17.11, TestExplain: 48 hours of plan at 15
// items per hour, limit 9):
//
//	Limit (actual rows=9 loops=1)
//	  ->  Incremental Sort (actual rows=9 loops=1)
//	        Sort Key: si.planned_start_utc, si.id
//	        Presorted Key: si.planned_start_utc
//	        ->  Nested Loop Left Join (actual rows=10 loops=1)
//	              ->  Nested Loop (actual rows=10 loops=1)
//	                    ->  Index Scan using ix_scheduled_items_planned_start_utc on scheduled_items si (actual rows=10 loops=1)
//	                          Index Cond: (planned_start_utc >= '2026-10-15 08:01:00+00'::timestamp with time zone)
//	                          Filter: (status = ANY ('{planned,queued}'::text[]))
//	                    ->  Memoize -> Index Scan using pk_tracks on tracks t (actual rows=1 loops=1)
//	              ->  Index Scan using pk_artists on artists a (actual rows=1 loops=10)
//	Planning Time: 0.205 ms
//	Execution Time: 0.034 ms
func (s *Store) Queued(ctx context.Context, from time.Time, limit int) ([]airwindow.Queued, error) {
	rows, err := s.q.Query(ctx, queuedSQL, from.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("select queued items: %w", err)
	}
	out, err := db.CollectAll(rows, queuedRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan queued items: %w", err)
	}
	return out, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	return new(t.UTC())
}
