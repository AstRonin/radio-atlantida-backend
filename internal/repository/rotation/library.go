// Package rotation stores the track plan, the built hours, and the filler
// reminders, and reads what a build needs (§7 of the design). It implements
// rotation.Store of the domain package with the same name; import it as
// rotationrepo next to the domain. pgx: rotation is a hot path (§5).
package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

// Store is the rotation repository.
type Store struct {
	q db.Querier
}

// New returns the repository on q (the pool).
func New(q db.Querier) *Store { return &Store{q: q} }

// WithTx returns the repository bound to tx, as the domain's interface type.
func (s *Store) WithTx(tx db.Tx) rotation.Store { return &Store{q: tx.Pgx()} }

// planLockKey is the advisory lock of plan writes ("rotation/plan"); the job
// locks of internal/repository/jobs hash their names, so a fixed small key
// cannot collide with them in practice.
const planLockKey int64 = 0x726f74706c616e // "rotplan"

// LockPlan takes the transaction-level advisory lock of the plan. It is
// released when the transaction ends.
func (s *Store) LockPlan(ctx context.Context) error {
	if _, err := s.q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, planLockKey); err != nil {
		return fmt.Errorf("lock plan: %w", err)
	}
	return nil
}

type trackRow struct {
	ID          uint64   `db:"id"`
	ArtistID    *uint64  `db:"artist_id"`
	AirMS       int32    `db:"air_ms"`
	CueInMS     int32    `db:"cue_in_ms"`
	PlaylistIDs []uint64 `db:"playlist_ids"`
	RuleIDs     []uint64 `db:"rule_ids"`
}

func (r trackRow) domain() rotation.Track {
	return rotation.Track{
		ID: r.ID, ArtistID: r.ArtistID, DurationMS: int(r.AirMS), CueInMS: int(r.CueInMS),
		PlaylistIDs: r.PlaylistIDs, RuleIDs: r.RuleIDs,
	}
}

type ruleRow struct {
	ID                 uint64      `db:"id"`
	WindowFrom         pgtype.Time `db:"window_from"`
	WindowTo           pgtype.Time `db:"window_to"`
	Weekdays           []int16     `db:"weekdays"`
	MinIntervalMinutes *int32      `db:"min_interval_minutes"`
	MaxPerHour         *int32      `db:"max_per_hour"`
}

const microsPerMinute = int64(time.Minute / time.Microsecond)

func (r ruleRow) domain() playrules.RuleInput {
	var out playrules.RuleInput
	if r.WindowFrom.Valid && r.WindowTo.Valid {
		out.WindowFrom = new(playrules.TimeOfDay(r.WindowFrom.Microseconds / microsPerMinute))
		out.WindowTo = new(playrules.TimeOfDay(r.WindowTo.Microseconds / microsPerMinute))
	}
	if r.Weekdays != nil {
		out.Weekdays = make([]int, len(r.Weekdays))
		for i, d := range r.Weekdays {
			out.Weekdays[i] = int(d)
		}
	}
	if r.MinIntervalMinutes != nil {
		out.MinIntervalMinutes = new(int(*r.MinIntervalMinutes))
	}
	if r.MaxPerHour != nil {
		out.MaxPerHour = new(int(*r.MaxPerHour))
	}
	return out
}

// Library returns every track with a duration, in ID order, with its
// length on air (between its cue points, docs/phase-22-plan.md), its
// playlists, the play rules of the track and of its playlists, and every
// play rule. Tracks without a duration cannot be planned, nor can a track
// whose cue no longer fits its file (a duration entered by hand; an
// analysis resets such a cue). The playlist and
// rule lookups use pk_track_playlists, pk_track_play_rules, and
// pk_playlist_play_rules.
func (s *Store) Library(ctx context.Context) (rotation.Library, error) {
	rows, err := s.q.Query(ctx, `
SELECT t.id, t.artist_id, COALESCE(t.cue_out_ms, t.duration_ms) - t.cue_in_ms AS air_ms, t.cue_in_ms,
       ARRAY(SELECT tp.playlist_id FROM track_playlists tp WHERE tp.track_id = t.id ORDER BY tp.playlist_id) AS playlist_ids,
       ARRAY(SELECT tr.rule_id FROM track_play_rules tr WHERE tr.track_id = t.id
             UNION
             SELECT pr.rule_id FROM track_playlists tp2
             JOIN playlist_play_rules pr ON pr.playlist_id = tp2.playlist_id
             WHERE tp2.track_id = t.id) AS rule_ids
FROM tracks t
WHERE t.duration_ms IS NOT NULL AND COALESCE(t.cue_out_ms, t.duration_ms) > t.cue_in_ms
  AND (t.cue_out_ms IS NULL OR t.cue_out_ms <= t.duration_ms)
ORDER BY t.id`)
	if err != nil {
		return rotation.Library{}, fmt.Errorf("select library: %w", err)
	}
	tracks, err := db.CollectAll(rows, trackRow.domain)
	if err != nil {
		return rotation.Library{}, fmt.Errorf("scan library: %w", err)
	}

	rows, err = s.q.Query(ctx, `
SELECT id, window_from, window_to, weekdays, min_interval_minutes, max_per_hour FROM play_rules`)
	if err != nil {
		return rotation.Library{}, fmt.Errorf("select play rules: %w", err)
	}
	rules, err := db.CollectAll(rows, func(r ruleRow) ruleRow { return r })
	if err != nil {
		return rotation.Library{}, fmt.Errorf("scan play rules: %w", err)
	}
	out := rotation.Library{Tracks: tracks, Rules: make(map[uint64]playrules.RuleInput, len(rules))}
	for _, r := range rules {
		out.Rules[r.ID] = r.domain()
	}
	return out, nil
}

// Separation returns the longest interval of each scope.
func (s *Store) Separation(ctx context.Context) (rotation.Separation, error) {
	var track, artist int
	err := s.q.QueryRow(ctx, `
SELECT COALESCE(max(min_interval_hours) FILTER (WHERE scope = 'track'), 0),
       COALESCE(max(min_interval_hours) FILTER (WHERE scope = 'artist'), 0)
FROM separation_rules`).Scan(&track, &artist)
	if err != nil {
		return rotation.Separation{}, fmt.Errorf("select separation: %w", err)
	}
	return rotation.Separation{Track: time.Duration(track) * time.Hour, Artist: time.Duration(artist) * time.Hour}, nil
}

type playRow struct {
	TrackID  uint64    `db:"track_id"`
	ArtistID *uint64   `db:"artist_id"`
	Start    time.Time `db:"start"`
}

func (r playRow) domain() rotation.Play {
	return rotation.Play{TrackID: r.TrackID, ArtistID: r.ArtistID, Start: r.Start.UTC()}
}

// History returns the plays in [from, to): aired and truncated rows of the
// play log, planned and queued rows of the plan, without the planned items
// of the hour at hourStart (it is being rebuilt). Missed items do not count
// (§7). The play log part uses ix_play_log_started_at. scheduled_items
// holds only the plan horizon, so the window covers most of it and a
// sequential scan wins.
//
// EXPLAIN ANALYZE (PostgreSQL 17.11, TestExplainHistory: 10 days of play
// log at 15 plays per hour, 48 hours of plan, a 145-hour window):
//
//	Append (actual rows=1787 loops=1)
//	  ->  Index Scan using ix_play_log_started_at on play_log pl (actual rows=1081 loops=1)
//	  ->  Nested Loop (actual rows=706 loops=1)
//	        ->  Seq Scan on scheduled_items si (actual rows=706 loops=1)
//	        ->  Memoize (actual rows=1 loops=706)
//	              ->  Index Scan using pk_tracks on tracks t (actual rows=1 loops=1)
//	Planning Time: 0.289 ms
//	Execution Time: 0.308 ms
func (s *Store) History(ctx context.Context, from, to, hourStart time.Time) ([]rotation.Play, error) {
	rows, err := s.q.Query(ctx, `
SELECT pl.track_id, pl.artist_id, pl.started_at AS start
FROM play_log pl
WHERE pl.status IN ('aired', 'truncated') AND pl.started_at >= $1 AND pl.started_at < $2
UNION ALL
SELECT si.track_id, t.artist_id, si.planned_start_utc AS start
FROM scheduled_items si
JOIN tracks t ON t.id = si.track_id
WHERE si.status IN ('planned', 'queued')
  AND si.planned_start_utc >= $1 AND si.planned_start_utc < $2
  AND NOT (si.status = 'planned' AND si.planned_start_utc >= $3 AND si.planned_start_utc < $4)`,
		from, to, hourStart, hourStart.Add(time.Hour))
	if err != nil {
		return nil, fmt.Errorf("select history: %w", err)
	}
	plays, err := db.CollectAll(rows, playRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan history: %w", err)
	}
	return plays, nil
}

type poolRow struct {
	ID      uint64 `db:"id"`
	TrackID uint64 `db:"track_id"`
}

// Pool returns the unused rows of a clock's pool in position order
// (ix_clock_filler_pool_clock_id_position_unused).
func (s *Store) Pool(ctx context.Context, clockID uint64) ([]rotation.PoolRow, error) {
	rows, err := s.q.Query(ctx, `
SELECT id, track_id FROM clock_filler_pool
WHERE clock_id = $1 AND used_at IS NULL
ORDER BY position`, clockID)
	if err != nil {
		return nil, fmt.Errorf("select pool of clock %d: %w", clockID, err)
	}
	out, err := db.CollectAll(rows, func(r poolRow) rotation.PoolRow { return rotation.PoolRow{ID: r.ID, TrackID: r.TrackID} })
	if err != nil {
		return nil, fmt.Errorf("scan pool of clock %d: %w", clockID, err)
	}
	return out, nil
}
