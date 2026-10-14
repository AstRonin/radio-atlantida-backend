// Package playout stores what playout does with the plan: items pushed to
// Liquidsoap, items moved to the play log when they start, their ends, and
// emergency tracks (§4.1, §8). It implements playout.Store of the domain
// package with the same name; import it as playoutrepo next to the domain.
// pgx: play log writes are a hot path (§5).
package playout

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
)

// Store is the playout repository.
type Store struct {
	q db.Querier
}

// New returns the repository on q (the pool).
func New(q db.Querier) *Store { return &Store{q: q} }

// WithTx returns the repository bound to tx, as the domain's interface type.
func (s *Store) WithTx(tx db.Tx) playout.Store { return &Store{q: tx.Pgx()} }

// planLockKey is the advisory lock of plan writes. It must equal planLockKey
// of internal/repository/rotation: the planner holds it while it builds an
// hour, and playout takes it to mark an item queued.
const planLockKey int64 = 0x726f74706c616e // "rotplan"

// LockPlan takes the transaction-level advisory lock of plan writes.
func (s *Store) LockPlan(ctx context.Context) error {
	if _, err := s.q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, planLockKey); err != nil {
		return fmt.Errorf("lock plan: %w", err)
	}
	return nil
}

type itemRow struct {
	ID        uint64    `db:"id"`
	TrackID   uint64    `db:"track_id"`
	FilePath  string    `db:"file_path"`
	Start     time.Time `db:"planned_start_utc"`
	InMS      int       `db:"planned_in_ms"`
	OutMS     int       `db:"planned_out_ms"`
	Truncated bool      `db:"truncated"`
	Queued    bool      `db:"queued"`
}

func (r itemRow) domain() playout.Item {
	return playout.Item{
		ID: r.ID, TrackID: r.TrackID, FilePath: r.FilePath, Start: r.Start.UTC(), InMS: r.InMS, OutMS: r.OutMS,
		Truncated: r.Truncated, Queued: r.Queued,
	}
}

// Items returns the planned and queued items that start in [from, to), in
// planned order (ix_scheduled_items_planned_start_utc).
func (s *Store) Items(ctx context.Context, from, to time.Time) ([]playout.Item, error) {
	rows, err := s.q.Query(ctx, `SELECT si.id, si.track_id, t.file_path, si.planned_start_utc, si.planned_in_ms,
       si.planned_out_ms, si.truncated, si.status = 'queued' AS queued
FROM scheduled_items si
JOIN tracks t ON t.id = si.track_id
WHERE si.planned_start_utc >= $1 AND si.planned_start_utc < $2 AND si.status IN ('planned', 'queued')
ORDER BY si.planned_start_utc, si.id`, from, to)
	if err != nil {
		return nil, fmt.Errorf("select plan items: %w", err)
	}
	out, err := db.CollectAll(rows, itemRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan plan items: %w", err)
	}
	return out, nil
}

// MarkQueued sets a planned item queued; false when it is not planned.
func (s *Store) MarkQueued(ctx context.Context, id uint64) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE scheduled_items SET status = 'queued' WHERE id = $1 AND status = 'planned'`, id)
	if err != nil {
		return false, fmt.Errorf("mark item %d queued: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkPlanned sets a queued item planned again.
func (s *Store) MarkPlanned(ctx context.Context, id uint64) error {
	if _, err := s.q.Exec(ctx, `UPDATE scheduled_items SET status = 'planned' WHERE id = $1 AND status = 'queued'`, id); err != nil {
		return fmt.Errorf("mark item %d planned: %w", id, err)
	}
	return nil
}

// Unqueue sets the queued items that start in [from, to) planned again.
func (s *Store) Unqueue(ctx context.Context, from, to time.Time) (int64, error) {
	tag, err := s.q.Exec(ctx, `UPDATE scheduled_items SET status = 'planned'
WHERE planned_start_utc >= $1 AND planned_start_utc < $2 AND status = 'queued'`, from, to)
	if err != nil {
		return 0, fmt.Errorf("unqueue items: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Start moves a planned or queued item to the play log (§4.1): one
// statement deletes the plan row and inserts the log row, so it is atomic
// without an explicit transaction. Until its end is recorded, the row
// assumes the item plays its planned length. The artist is copied for
// artist separation. A repeated start finds no plan row and changes
// nothing; uq_play_log_scheduled_item_id guards the rest.
func (s *Store) Start(ctx context.Context, itemID uint64, at time.Time) (bool, error) {
	tag, err := s.q.Exec(ctx, `WITH moved AS (
    DELETE FROM scheduled_items
    WHERE id = $1 AND status IN ('planned', 'queued')
    RETURNING id, track_id, clock_id, grid_slot_id, planned_start_utc, planned_in_ms, planned_out_ms, truncated
)
INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, started_at, planned_out_ms,
                      actual_duration_ms, truncated, source, clock_id, grid_slot_id, scheduled_item_id)
SELECT m.track_id, t.artist_id, CASE WHEN m.truncated THEN 'truncated' ELSE 'aired' END, m.planned_start_utc, $2,
       m.planned_out_ms, m.planned_out_ms - m.planned_in_ms, m.truncated, 'auto', m.clock_id, m.grid_slot_id, m.id
FROM moved m
JOIN tracks t ON t.id = m.track_id
ON CONFLICT (scheduled_item_id) WHERE scheduled_item_id IS NOT NULL DO NOTHING`, itemID, at)
	if err != nil {
		return false, fmt.Errorf("move item %d to the play log: %w", itemID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// End records the end of a logged plan item whose end is not recorded yet.
// The planner never sets an in point, so planned_out_ms is the planned
// length. In SET, truncated is the value before the update.
func (s *Store) End(ctx context.Context, itemID uint64, elapsedMS, cutMarginMS int) (bool, error) {
	tag, err := s.q.Exec(ctx, `UPDATE play_log SET
    actual_duration_ms = $2::integer,
    ended_at = started_at + make_interval(secs => $2::integer / 1000.0),
    status = CASE WHEN truncated OR $2::integer < planned_out_ms - $3::integer THEN 'truncated' ELSE 'aired' END,
    truncated = truncated OR $2::integer < planned_out_ms - $3::integer
WHERE scheduled_item_id = $1 AND ended_at IS NULL AND status <> 'missed'`, itemID, elapsedMS, cutMarginMS)
	if err != nil {
		return false, fmt.Errorf("record end of item %d: %w", itemID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// LogEmergency logs an emergency track that played (§8.6): no plan fields.
// The start is computed from the journal line, so a replayed line hits
// uq_play_log_emergency and changes nothing.
func (s *Store) LogEmergency(ctx context.Context, trackID uint64, startedAt time.Time, elapsedMS int) error {
	var found bool
	err := s.q.QueryRow(ctx, `WITH track AS (
    SELECT id, artist_id FROM tracks WHERE id = $1
), logged AS (
    INSERT INTO play_log (track_id, artist_id, status, started_at, ended_at, actual_duration_ms, source)
    SELECT t.id, t.artist_id, 'aired', $2::timestamptz, $2::timestamptz + make_interval(secs => $3::integer / 1000.0),
           $3::integer, 'emergency'
    FROM track t
    ON CONFLICT (track_id, started_at) WHERE source = 'emergency' DO NOTHING
)
SELECT EXISTS (SELECT 1 FROM track)`, trackID, startedAt, elapsedMS).Scan(&found)
	if err != nil {
		return fmt.Errorf("log emergency track %d: %w", trackID, err)
	}
	if !found {
		return fmt.Errorf("log emergency track %d: %w", trackID, errNoTrack)
	}
	return nil
}

var errNoTrack = errors.New("no such track")

type loggedRow struct {
	ItemID     uint64    `db:"scheduled_item_id"`
	TrackID    uint64    `db:"track_id"`
	Title      string    `db:"title"`
	ArtistName *string   `db:"artist_name"`
	StartedAt  time.Time `db:"started_at"`
	OutMS      *int      `db:"planned_out_ms"`
	Truncated  bool      `db:"truncated"`
}

func (r loggedRow) domain() playout.NowPlaying {
	return playout.NowPlaying{
		ItemID: new(r.ItemID), TrackID: r.TrackID, Title: r.Title, ArtistName: r.ArtistName,
		Source: playout.SourcePlan, StartedAt: r.StartedAt.UTC(), OutMS: r.OutMS, Truncated: r.Truncated,
	}
}

var errNotLogged = errors.New("item not logged")

// Logged returns the logged plan item as now playing
// (uq_play_log_scheduled_item_id).
func (s *Store) Logged(ctx context.Context, itemID uint64) (playout.NowPlaying, bool, error) {
	rows, err := s.q.Query(ctx, `SELECT pl.scheduled_item_id, pl.track_id, t.title, a.name AS artist_name,
       pl.started_at, pl.planned_out_ms, pl.truncated
FROM play_log pl
JOIN tracks t ON t.id = pl.track_id
LEFT JOIN artists a ON a.id = t.artist_id
WHERE pl.scheduled_item_id = $1`, itemID)
	if err != nil {
		return playout.NowPlaying{}, false, fmt.Errorf("select logged item %d: %w", itemID, err)
	}
	np, err := db.CollectOne(rows, loggedRow.domain, errNotLogged)
	switch {
	case errors.Is(err, errNotLogged):
		return playout.NowPlaying{}, false, nil
	case err != nil:
		return playout.NowPlaying{}, false, fmt.Errorf("scan logged item %d: %w", itemID, err)
	}
	return np, true, nil
}

type trackRow struct {
	ID         uint64  `db:"id"`
	Title      string  `db:"title"`
	ArtistName *string `db:"artist_name"`
	FilePath   string  `db:"file_path"`
	DurationMS *int    `db:"duration_ms"`
}

func (r trackRow) domain() playout.Track {
	return playout.Track{ID: r.ID, Title: r.Title, ArtistName: r.ArtistName, FilePath: r.FilePath, DurationMS: r.DurationMS}
}

const trackColumns = `t.id, t.title, a.name AS artist_name, t.file_path, t.duration_ms`

// Track returns a track; false if there is none.
func (s *Store) Track(ctx context.Context, id uint64) (playout.Track, bool, error) {
	rows, err := s.q.Query(ctx, `SELECT `+trackColumns+`
FROM tracks t LEFT JOIN artists a ON a.id = t.artist_id WHERE t.id = $1`, id)
	if err != nil {
		return playout.Track{}, false, fmt.Errorf("select track %d: %w", id, err)
	}
	t, err := db.CollectOne(rows, trackRow.domain, errNoTrack)
	switch {
	case errors.Is(err, errNoTrack):
		return playout.Track{}, false, nil
	case err != nil:
		return playout.Track{}, false, fmt.Errorf("scan track %d: %w", id, err)
	}
	return t, true, nil
}

// PlaylistTracks returns the tracks with a duration of a playlist
// (ix_track_playlists_playlist_id).
func (s *Store) PlaylistTracks(ctx context.Context, playlistID uint64) ([]playout.Track, error) {
	rows, err := s.q.Query(ctx, `SELECT `+trackColumns+`
FROM track_playlists tp
JOIN tracks t ON t.id = tp.track_id
LEFT JOIN artists a ON a.id = t.artist_id
WHERE tp.playlist_id = $1 AND t.duration_ms IS NOT NULL
ORDER BY t.id`, playlistID)
	if err != nil {
		return nil, fmt.Errorf("select tracks of playlist %d: %w", playlistID, err)
	}
	out, err := db.CollectAll(rows, trackRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan tracks of playlist %d: %w", playlistID, err)
	}
	return out, nil
}

type queuedRow struct {
	ID         uint64    `db:"id"`
	TrackID    uint64    `db:"track_id"`
	Title      string    `db:"title"`
	ArtistName *string   `db:"artist_name"`
	Start      time.Time `db:"planned_start_utc"`
	OutMS      int       `db:"planned_out_ms"`
	Truncated  bool      `db:"truncated"`
}

func (r queuedRow) domain() playout.QueuedItem {
	return playout.QueuedItem{
		ID: r.ID, TrackID: r.TrackID, Title: r.Title, ArtistName: r.ArtistName, Start: r.Start.UTC(),
		OutMS: r.OutMS, Truncated: r.Truncated,
	}
}

// Queued returns the queued items in planned order
// (ix_scheduled_items_status_outage_id leads with the status).
func (s *Store) Queued(ctx context.Context, limit int) ([]playout.QueuedItem, error) {
	rows, err := s.q.Query(ctx, `SELECT si.id, si.track_id, t.title, a.name AS artist_name, si.planned_start_utc,
       si.planned_out_ms, si.truncated
FROM scheduled_items si
JOIN tracks t ON t.id = si.track_id
LEFT JOIN artists a ON a.id = t.artist_id
WHERE si.status = 'queued'
ORDER BY si.planned_start_utc, si.id
LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("select queued items: %w", err)
	}
	out, err := db.CollectAll(rows, queuedRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan queued items: %w", err)
	}
	return out, nil
}
