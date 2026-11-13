package library

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

type trackUserRow struct {
	TrackID uint64 `db:"track_id"`
	Kind    string `db:"kind"`
	ID      uint64 `db:"id"`
	Name    string `db:"name"`
}

// users reads usersOfTracks for ids, at most limit per track.
func (s *TrackQueries) users(ctx context.Context, ids []uint64, limit int) ([]trackUserRow, error) {
	rows, err := s.q.Query(ctx, usersOfTracks, ids, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[trackUserRow])
}

// Users returns the clocks with an insert of the track and its one-time
// inserts, at most limit.
func (s *TrackQueries) Users(ctx context.Context, id uint64, limit int) ([]library.Ref, error) {
	users, err := s.users(ctx, []uint64{id}, limit)
	if err != nil {
		return nil, fmt.Errorf("select users of track %d: %w", id, err)
	}
	rows := make([]refRow, len(users))
	for i, u := range users {
		rows[i] = refRow{Kind: u.Kind, ID: u.ID, Name: u.Name}
	}
	return refs(rows), nil
}

// DeleteUnreferenced deletes the tracks of ids that nothing refers to, in
// one statement; their memberships and play rule links go with them
// (CASCADE). Each NOT EXISTS is an index lookup: ix_play_log_track_id_started_at,
// ix_scheduled_items_track_id_planned_start_utc, ix_hour_inserts_track_id,
// ix_clock_inserts_track_id. A reference committed after the
// statement's snapshot still fails it on the foreign key (RESTRICT).
//
// EXPLAIN ANALYZE, PostgreSQL 17, 30 000 tracks, 10 000 play log rows,
// 1 000 IDs: Index Only Scan on pk_tracks, anti joins to the reference
// tables, 1.7 ms; the delete of 666 tracks with their links 15 ms.
// Blockers on the 334 kept: 8 ms.
//
// A foreign key violation can surface when the rows are read (pgx reports
// it at rows.Err), so the collect error is mapped as the query error is.
func (s *TrackQueries) DeleteUnreferenced(ctx context.Context, ids []uint64) ([]uint64, error) {
	rows, err := s.q.Query(ctx, `
DELETE FROM tracks t
WHERE t.id = ANY($1)
  AND NOT EXISTS (SELECT 1 FROM play_log p WHERE p.track_id = t.id)
  AND NOT EXISTS (SELECT 1 FROM scheduled_items i WHERE i.track_id = t.id)
  AND NOT EXISTS (SELECT 1 FROM hour_inserts h WHERE h.track_id = t.id)
  AND NOT EXISTS (SELECT 1 FROM clock_inserts c WHERE c.track_id = t.id)
RETURNING t.id`, ids)
	if err != nil {
		return nil, deleteError("delete tracks", err)
	}
	deleted, err := pgx.CollectRows(rows, pgx.RowTo[uint64])
	if err != nil {
		return nil, deleteError("delete tracks", err)
	}
	if deleted == nil {
		deleted = []uint64{}
	}
	return deleted, nil
}

// blockerRow is a track of Blockers and whether its history refers to it.
type blockerRow struct {
	ID      uint64 `db:"id"`
	Title   string `db:"title"`
	History bool   `db:"history"`
}

// Blockers returns the title of each existing track of ids, whether its
// history refers to it, and the clocks and one-time inserts that use it (at
// most limit per track).
func (s *TrackQueries) Blockers(ctx context.Context, ids []uint64, limit int) (map[uint64]library.TrackBlockers, error) {
	rows, err := s.q.Query(ctx, `
SELECT t.id, t.title,
       EXISTS (SELECT 1 FROM play_log p WHERE p.track_id = t.id)
       OR EXISTS (SELECT 1 FROM scheduled_items i WHERE i.track_id = t.id) AS history
FROM tracks t
WHERE t.id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("select tracks kept: %w", err)
	}
	tracks, err := pgx.CollectRows(rows, pgx.RowToStructByName[blockerRow])
	if err != nil {
		return nil, fmt.Errorf("scan tracks kept: %w", err)
	}
	users, err := s.users(ctx, ids, limit)
	if err != nil {
		return nil, fmt.Errorf("select users of tracks kept: %w", err)
	}
	out := make(map[uint64]library.TrackBlockers, len(tracks))
	for _, t := range tracks {
		out[t.ID] = library.TrackBlockers{Title: t.Title, History: t.History}
	}
	for _, u := range users {
		if b, ok := out[u.TrackID]; ok {
			b.UsedBy = append(b.UsedBy, library.Ref{Kind: u.Kind, ID: u.ID, Name: u.Name})
			out[u.TrackID] = b
		}
	}
	return out, nil
}
