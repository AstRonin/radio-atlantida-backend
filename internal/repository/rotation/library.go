// Package rotation stores the track plan, the built hours, and the one-time
// inserts, and reads what a build needs (§7 of the design). It implements
// rotation.Store of the domain package with the same name; import it as
// rotationrepo next to the domain. pgx: rotation is a hot path (§5).
package rotation

import (
	"cmp"
	"context"
	"fmt"
	"slices"
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

// trackRow is a track of the snapshot: the columns a build reads.
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

// trackInfoRow is a track with its title and artist, for output (TracksOf).
type trackInfoRow struct {
	trackRow
	Title      string  `db:"title"`
	ArtistName *string `db:"artist_name"`
}

func (r trackInfoRow) domain() rotation.Track {
	t := r.trackRow.domain()
	t.Title, t.ArtistName = r.Title, r.ArtistName
	return t
}

// trackSelect selects the columns of a track t, with its playlists and rules
// as arrays (TracksOf).
const trackSelect = `
SELECT t.id, t.artist_id, COALESCE(t.cue_out_ms, t.duration_ms) - t.cue_in_ms AS air_ms, t.cue_in_ms,
       ARRAY(SELECT tp.playlist_id FROM track_playlists tp WHERE tp.track_id = t.id ORDER BY tp.playlist_id) AS playlist_ids,
       ARRAY(SELECT tr.rule_id FROM track_play_rules tr WHERE tr.track_id = t.id
             UNION
             SELECT pr.rule_id FROM track_playlists tp2
             JOIN playlist_play_rules pr ON pr.playlist_id = tp2.playlist_id
             WHERE tp2.track_id = t.id) AS rule_ids`

const trackValid = `t.duration_ms IS NOT NULL AND COALESCE(t.cue_out_ms, t.duration_ms) > t.cue_in_ms
  AND (t.cue_out_ms IS NULL OR t.cue_out_ms <= t.duration_ms)`

type ruleRow struct {
	ID                 uint64      `db:"id"`
	WindowFrom         pgtype.Time `db:"window_from"`
	WindowTo           pgtype.Time `db:"window_to"`
	Weekdays           []int16     `db:"weekdays"`
	MinIntervalMinutes *int32      `db:"min_interval_minutes"`
	ArtistMinInterval  *int32      `db:"artist_min_interval_minutes"`
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
	if r.ArtistMinInterval != nil {
		out.ArtistMinIntervalMinutes = new(int(*r.ArtistMinInterval))
	}
	if r.MaxPerHour != nil {
		out.MaxPerHour = new(int(*r.MaxPerHour))
	}
	return out
}

// Library returns the tracks of the scope with a duration, in ID order: every
// track when scope.All, else the tracks of scope.Playlists and the tracks of
// scope.Tracks, with their length on air, playlists, and the play rules of the
// track and of its playlists. Titles are not read (TracksOf). Every play rule
// is read: they are few.
//
// It is three set reads, not one query with per-track subqueries: a snapshot of
// a few hundred thousand tracks takes seconds with subqueries, and the reads
// use pk_track_playlists, ix_track_playlists_playlist_id, pk_track_play_rules,
// and pk_playlist_play_rules.
func (s *Store) Library(ctx context.Context, scope rotation.LibraryScope) (rotation.Library, error) {
	base, err := s.tracksOf(ctx, scope)
	if err != nil {
		return rotation.Library{}, err
	}
	ids := make([]int64, len(base))
	for i, t := range base {
		ids[i] = int64(t.ID) //nolint:gosec // G115: IDs are bigint keys of the database
	}

	playlists, err := s.playlistsOf(ctx, ids)
	if err != nil {
		return rotation.Library{}, err
	}
	rules, err := s.rulesOf(ctx, ids)
	if err != nil {
		return rotation.Library{}, err
	}
	for i := range base {
		base[i].PlaylistIDs = playlists[base[i].ID]
		base[i].RuleIDs = rules[base[i].ID]
	}

	ruleRows, err := s.q.Query(ctx, `
SELECT id, window_from, window_to, weekdays, min_interval_minutes, artist_min_interval_minutes, max_per_hour
FROM play_rules`)
	if err != nil {
		return rotation.Library{}, fmt.Errorf("select play rules: %w", err)
	}
	allRules, err := db.CollectAll(ruleRows, func(r ruleRow) ruleRow { return r })
	if err != nil {
		return rotation.Library{}, fmt.Errorf("scan play rules: %w", err)
	}
	out := rotation.Library{Tracks: base, Rules: make(map[uint64]playrules.RuleInput, len(allRules))}
	for _, r := range allRules {
		out.Rules[r.ID] = r.domain()
	}
	return out, nil
}

// tracksOf reads the snapshot tracks of the scope, in ID order. A scope of
// playlists and a scope of named tracks are two reads, merged here: a query
// with both in one OR can plan badly on a large library.
func (s *Store) tracksOf(ctx context.Context, scope rotation.LibraryScope) ([]rotation.Track, error) {
	var (
		rows []rotation.Track
		err  error
	)
	if scope.All {
		rows, err = s.trackRows(ctx, trackBase+` WHERE `+trackValid+` ORDER BY t.id`)
		if err != nil {
			return nil, err
		}
		return rows, nil
	}
	byID := map[uint64]rotation.Track{}
	if len(scope.Playlists) > 0 {
		rows, err = s.trackRows(ctx, trackBase+`
WHERE `+trackValid+` AND t.id IN (SELECT tp.track_id FROM track_playlists tp WHERE tp.playlist_id = ANY($1))`,
			int64s(scope.Playlists))
		if err != nil {
			return nil, err
		}
		for _, t := range rows {
			byID[t.ID] = t
		}
	}
	if len(scope.Tracks) > 0 {
		rows, err = s.trackRows(ctx, trackBase+` WHERE `+trackValid+` AND t.id = ANY($1)`, int64s(scope.Tracks))
		if err != nil {
			return nil, err
		}
		for _, t := range rows {
			byID[t.ID] = t
		}
	}
	out := make([]rotation.Track, 0, len(byID))
	for _, t := range byID {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b rotation.Track) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// trackRows runs a query of trackBase and maps its rows.
func (s *Store) trackRows(ctx context.Context, query string, args ...any) ([]rotation.Track, error) {
	rows, err := s.q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("select library: %w", err)
	}
	out, err := db.CollectAll(rows, trackBaseRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan library: %w", err)
	}
	return out, nil
}

// playlistsOf returns the playlist IDs of each track, in ID order.
func (s *Store) playlistsOf(ctx context.Context, ids []int64) (map[uint64][]uint64, error) {
	rows, err := s.q.Query(ctx, `
SELECT tp.track_id, tp.playlist_id AS id FROM track_playlists tp
WHERE tp.track_id = ANY($1) ORDER BY tp.track_id, tp.playlist_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("select track playlists: %w", err)
	}
	pairs, err := db.CollectAll(rows, trackPairRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan track playlists: %w", err)
	}
	out := make(map[uint64][]uint64, len(ids))
	for _, p := range pairs {
		out[p.track] = append(out[p.track], p.id)
	}
	return out, nil
}

// rulesOf returns the play rule IDs of each track: its own and those of its
// playlists, in ID order without duplicates.
func (s *Store) rulesOf(ctx context.Context, ids []int64) (map[uint64][]uint64, error) {
	rows, err := s.q.Query(ctx, `
SELECT tr.track_id, tr.rule_id AS id FROM track_play_rules tr WHERE tr.track_id = ANY($1)
UNION
SELECT tp.track_id, pr.rule_id AS id FROM track_playlists tp
JOIN playlist_play_rules pr ON pr.playlist_id = tp.playlist_id
WHERE tp.track_id = ANY($1)
ORDER BY 1, 2`, ids)
	if err != nil {
		return nil, fmt.Errorf("select track rules: %w", err)
	}
	pairs, err := db.CollectAll(rows, trackPairRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan track rules: %w", err)
	}
	out := make(map[uint64][]uint64, len(ids))
	for _, p := range pairs {
		out[p.track] = append(out[p.track], p.id)
	}
	return out, nil
}

// trackPair is a row of a track and one of its playlists or rules.
type trackPair struct {
	track, id uint64
}

// trackPairRow maps the rows of playlistsOf and rulesOf (column id).
type trackPairRow struct {
	Track uint64 `db:"track_id"`
	ID    uint64 `db:"id"`
}

func (r trackPairRow) domain() trackPair { return trackPair{track: r.Track, id: r.ID} }

// trackBaseRow is a track of the snapshot: the columns a build reads.
type trackBaseRow struct {
	ID       uint64  `db:"id"`
	ArtistID *uint64 `db:"artist_id"`
	AirMS    int32   `db:"air_ms"`
	CueInMS  int32   `db:"cue_in_ms"`
}

func (r trackBaseRow) domain() rotation.Track {
	return rotation.Track{ID: r.ID, ArtistID: r.ArtistID, DurationMS: int(r.AirMS), CueInMS: int(r.CueInMS)}
}

const trackBase = `
SELECT t.id, t.artist_id, COALESCE(t.cue_out_ms, t.duration_ms) - t.cue_in_ms AS air_ms, t.cue_in_ms
FROM tracks t`

// TracksOf returns the tracks with a duration among ids, in ID order, with
// their titles and artists. Tracks that cannot be planned are left out, as in
// the library snapshot.
func (s *Store) TracksOf(ctx context.Context, ids []uint64) ([]rotation.Track, error) {
	rows, err := s.q.Query(ctx, trackSelect+`, t.title, a.name AS artist_name
FROM tracks t
LEFT JOIN artists a ON a.id = t.artist_id
WHERE `+trackValid+` AND t.id = ANY($1)
ORDER BY t.id`, int64s(ids))
	if err != nil {
		return nil, fmt.Errorf("select tracks: %w", err)
	}
	out, err := db.CollectAll(rows, trackInfoRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan tracks: %w", err)
	}
	return out, nil
}

// int64s converts IDs to the array parameters of the queries; an empty list
// is an empty array, not NULL.
func int64s(ids []uint64) []int64 {
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[i] = int64(id) //nolint:gosec // G115: IDs are bigint keys of the database
	}
	return out
}

type playRow struct {
	TrackID  uint64    `db:"track_id"`
	ArtistID *uint64   `db:"artist_id"`
	Start    time.Time `db:"start"`
	Planned  bool      `db:"planned"`
}

func (r playRow) domain() rotation.Play {
	return rotation.Play{TrackID: r.TrackID, ArtistID: r.ArtistID, Start: r.Start.UTC(), Planned: r.Planned}
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
SELECT pl.track_id, pl.artist_id, pl.started_at AS start, FALSE AS planned
FROM play_log pl
WHERE pl.status IN ('aired', 'truncated') AND pl.started_at >= $1 AND pl.started_at < $2
UNION ALL
SELECT si.track_id, t.artist_id, si.planned_start_utc AS start, si.status = 'planned' AS planned
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

// Window returns the plays in [from, to) that a build reads as history: the
// aired and truncated rows of the play log, and the planned and queued rows of
// the plan, each flagged Planned when it is a planned item. Unlike History it
// keeps the planned items of every hour; the run of builds leaves out the hour
// it builds (rotation.window). Same index and plan as History.
func (s *Store) Window(ctx context.Context, from, to time.Time) ([]rotation.Play, error) {
	rows, err := s.q.Query(ctx, `
SELECT pl.track_id, pl.artist_id, pl.started_at AS start, FALSE AS planned
FROM play_log pl
WHERE pl.status IN ('aired', 'truncated') AND pl.started_at >= $1 AND pl.started_at < $2
UNION ALL
SELECT si.track_id, t.artist_id, si.planned_start_utc AS start, si.status = 'planned' AS planned
FROM scheduled_items si
JOIN tracks t ON t.id = si.track_id
WHERE si.status IN ('planned', 'queued') AND si.planned_start_utc >= $1 AND si.planned_start_utc < $2`,
		from, to)
	if err != nil {
		return nil, fmt.Errorf("select plays: %w", err)
	}
	plays, err := db.CollectAll(rows, playRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan plays: %w", err)
	}
	return plays, nil
}
