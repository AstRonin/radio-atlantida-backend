// Package playrules stores the play rules and their links to tracks and
// playlists (§4, §7.3 of the design). It implements playrules.Store of the
// domain package with the same name; import it as playrulesrepo next to the
// domain.
//
// pgx, not GORM: the rules have time and smallint[] columns, which
// database/sql (under GORM) returns only as text, and the link sets are one
// statement each with array parameters. Rotation reads the same tables on
// its hot path.
package playrules

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
)

// Store is the play rule repository.
type Store struct {
	q db.Querier
}

// New returns a repository on q (the pool).
func New(q db.Querier) *Store { return &Store{q: q} }

// WithTx returns the repository bound to tx, as the domain's interface
// type (Go has no covariant return types).
func (s *Store) WithTx(tx db.Tx) playrules.Store { return &Store{q: tx.Pgx()} }

var writeErrors = map[string]error{
	"uq_play_rules_name":                 playrules.ErrNameTaken,
	"fk_track_play_rules_rule_id":        playrules.ErrUnknownRule,
	"fk_playlist_play_rules_rule_id":     playrules.ErrUnknownRule,
	"fk_track_play_rules_track_id":       playrules.ErrUnknownTrack,
	"fk_playlist_play_rules_playlist_id": playrules.ErrUnknownPlaylist,
}

func writeError(op string, err error) error {
	if ce, ok := db.AsConstraintError(err); ok {
		if mapped, ok := writeErrors[ce.Constraint]; ok {
			return mapped
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

type ruleRow struct {
	ID                 uint64      `db:"id"`
	Name               string      `db:"name"`
	WindowFrom         pgtype.Time `db:"window_from"`
	WindowTo           pgtype.Time `db:"window_to"`
	Weekdays           []int16     `db:"weekdays"`
	MinIntervalMinutes *int32      `db:"min_interval_minutes"`
	MaxPerHour         *int32      `db:"max_per_hour"`
	Notes              string      `db:"notes"`
	CreatedAt          time.Time   `db:"created_at"`
	TrackIDs           []uint64    `db:"track_ids"`
	PlaylistIDs        []uint64    `db:"playlist_ids"`
}

func (r ruleRow) domain() playrules.Rule {
	out := playrules.Rule{
		ID: r.ID,
		RuleInput: playrules.RuleInput{
			Name:               r.Name,
			WindowFrom:         timeOfDay(r.WindowFrom),
			WindowTo:           timeOfDay(r.WindowTo),
			MinIntervalMinutes: intPtr(r.MinIntervalMinutes),
			MaxPerHour:         intPtr(r.MaxPerHour),
			Notes:              r.Notes,
		},
		CreatedAt:   r.CreatedAt.UTC(),
		TrackIDs:    r.TrackIDs,
		PlaylistIDs: r.PlaylistIDs,
	}
	if r.Weekdays != nil {
		out.Weekdays = make([]int, len(r.Weekdays))
		for i, d := range r.Weekdays {
			out.Weekdays[i] = int(d)
		}
	}
	return out
}

const microsPerMinute = int64(time.Minute / time.Microsecond)

func timeOfDay(t pgtype.Time) *playrules.TimeOfDay {
	if !t.Valid {
		return nil
	}
	v := playrules.TimeOfDay(t.Microseconds / microsPerMinute)
	return &v
}

func pgTime(t *playrules.TimeOfDay) pgtype.Time {
	if t == nil {
		return pgtype.Time{}
	}
	return pgtype.Time{Microseconds: int64(*t) * microsPerMinute, Valid: true}
}

func intPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

func int32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v) //nolint:gosec // G115: validated range (1 ... 10080 minutes, 1 ... 60 per hour).
	return &n
}

func weekdays(days []int) []int16 {
	if days == nil {
		return nil
	}
	out := make([]int16, len(days))
	for i, d := range days {
		out[i] = int16(d) //nolint:gosec // G115: validated range 1 ... 7.
	}
	return out
}

// The target lists use ix_track_play_rules_rule_id and
// ix_playlist_play_rules_rule_id.
const selectRules = `
SELECT r.id, r.name, r.window_from, r.window_to, r.weekdays, r.min_interval_minutes,
       r.max_per_hour, r.notes, r.created_at,
       ARRAY(SELECT tr.track_id FROM track_play_rules tr WHERE tr.rule_id = r.id ORDER BY tr.track_id) AS track_ids,
       ARRAY(SELECT pr.playlist_id FROM playlist_play_rules pr WHERE pr.rule_id = r.id ORDER BY pr.playlist_id) AS playlist_ids
FROM play_rules r`

// List returns every rule in name order. The manager keeps tens of rules,
// so the list is not paged.
func (s *Store) List(ctx context.Context) ([]playrules.Rule, error) {
	rows, err := s.q.Query(ctx, selectRules+` ORDER BY r.name, r.id`)
	if err != nil {
		return nil, fmt.Errorf("select play rules: %w", err)
	}
	rs, err := db.CollectAll(rows, ruleRow.domain)
	if err != nil {
		return nil, fmt.Errorf("select play rules: %w", err)
	}
	return rs, nil
}

// Get returns one rule, or playrules.ErrNotFound.
func (s *Store) Get(ctx context.Context, id uint64) (playrules.Rule, error) {
	rows, err := s.q.Query(ctx, selectRules+` WHERE r.id = $1`, id)
	if err != nil {
		return playrules.Rule{}, fmt.Errorf("select play rule %d: %w", id, err)
	}
	r, err := db.CollectOne(rows, ruleRow.domain, playrules.ErrNotFound)
	if err != nil && !errors.Is(err, playrules.ErrNotFound) {
		return playrules.Rule{}, fmt.Errorf("select play rule %d: %w", id, err)
	}
	return r, err
}

func ruleArgs(in playrules.RuleInput) []any {
	return []any{
		in.Name, pgTime(in.WindowFrom), pgTime(in.WindowTo), weekdays(in.Weekdays),
		int32Ptr(in.MinIntervalMinutes), int32Ptr(in.MaxPerHour), in.Notes,
	}
}

// Create adds a rule.
func (s *Store) Create(ctx context.Context, in playrules.RuleInput) (uint64, error) {
	var id uint64
	err := s.q.QueryRow(ctx, `
INSERT INTO play_rules (name, window_from, window_to, weekdays, min_interval_minutes, max_per_hour, notes)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id`, ruleArgs(in)...).Scan(&id)
	if err != nil {
		return 0, writeError("insert play rule", err)
	}
	return id, nil
}

// Update changes a rule.
func (s *Store) Update(ctx context.Context, id uint64, in playrules.RuleInput) error {
	tag, err := s.q.Exec(ctx, `
UPDATE play_rules
SET name = $2, window_from = $3, window_to = $4, weekdays = $5,
    min_interval_minutes = $6, max_per_hour = $7, notes = $8
WHERE id = $1`, append([]any{id}, ruleArgs(in)...)...)
	if err != nil {
		return writeError(fmt.Sprintf("update play rule %d", id), err)
	}
	if tag.RowsAffected() == 0 {
		return playrules.ErrNotFound
	}
	return nil
}

// Delete removes a rule. Its links refuse it (RESTRICT on rule_id).
func (s *Store) Delete(ctx context.Context, id uint64) error {
	tag, err := s.q.Exec(ctx, `DELETE FROM play_rules WHERE id = $1`, id)
	if err != nil {
		if ce, ok := db.AsConstraintError(err); ok && ce.Kind == db.ForeignKey {
			return playrules.ErrInUse
		}
		return fmt.Errorf("delete play rule %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return playrules.ErrNotFound
	}
	return nil
}

type refRow struct {
	Kind string `db:"kind"`
	ID   uint64 `db:"id"`
	Name string `db:"name"`
}

// Users returns the tracks and playlists the rule is applied to, at most
// limit of each kind.
func (s *Store) Users(ctx context.Context, id uint64, limit int) ([]playrules.Ref, error) {
	rows, err := s.q.Query(ctx, `
(SELECT 'track' AS kind, t.id, t.title AS name
 FROM track_play_rules tr JOIN tracks t ON t.id = tr.track_id
 WHERE tr.rule_id = $1 ORDER BY t.title, t.id LIMIT $2)
UNION ALL
(SELECT 'playlist' AS kind, p.id, p.name
 FROM playlist_play_rules pr JOIN playlists p ON p.id = pr.playlist_id
 WHERE pr.rule_id = $1 ORDER BY p.name, p.id LIMIT $2)`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("select users of play rule %d: %w", id, err)
	}
	refs, err := db.CollectAll(rows, func(r refRow) playrules.Ref {
		return playrules.Ref{Kind: r.Kind, ID: r.ID, Name: r.Name}
	})
	if err != nil {
		return nil, fmt.Errorf("select users of play rule %d: %w", id, err)
	}
	return refs, nil
}

// lock runs a SELECT ... FOR KEY SHARE of one row, so the row cannot be
// deleted before the transaction ends.
func (s *Store) lock(ctx context.Context, sql string, id uint64, notFound error) error {
	var got uint64
	err := s.q.QueryRow(ctx, sql, id).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound
	}
	if err != nil {
		return fmt.Errorf("lock %d: %w", id, err)
	}
	return nil
}

// LockRule locks a rule. playrules.ErrNotFound.
func (s *Store) LockRule(ctx context.Context, id uint64) error {
	return s.lock(ctx, `SELECT id FROM play_rules WHERE id = $1 FOR KEY SHARE`, id, playrules.ErrNotFound)
}

// LockTrack locks a track. playrules.ErrTrackNotFound.
func (s *Store) LockTrack(ctx context.Context, id uint64) error {
	return s.lock(ctx, `SELECT id FROM tracks WHERE id = $1 FOR KEY SHARE`, id, playrules.ErrTrackNotFound)
}

// LockPlaylist locks a playlist. playrules.ErrPlaylistNotFound.
func (s *Store) LockPlaylist(ctx context.Context, id uint64) error {
	return s.lock(ctx, `SELECT id FROM playlists WHERE id = $1 FOR KEY SHARE`, id, playrules.ErrPlaylistNotFound)
}

// Each link set is one statement: the DELETE removes the links not in the
// list, the INSERT adds the missing ones. Both see the same snapshot and
// touch disjoint rows.
const (
	setTrackRules = `
WITH removed AS (
    DELETE FROM track_play_rules WHERE track_id = $1 AND rule_id <> ALL($2::bigint[])
)
INSERT INTO track_play_rules (track_id, rule_id)
SELECT $1, unnest($2::bigint[])
ON CONFLICT DO NOTHING`
	setPlaylistRules = `
WITH removed AS (
    DELETE FROM playlist_play_rules WHERE playlist_id = $1 AND rule_id <> ALL($2::bigint[])
)
INSERT INTO playlist_play_rules (playlist_id, rule_id)
SELECT $1, unnest($2::bigint[])
ON CONFLICT DO NOTHING`
	setRuleTracks = `
WITH removed AS (
    DELETE FROM track_play_rules WHERE rule_id = $1 AND track_id <> ALL($2::bigint[])
)
INSERT INTO track_play_rules (track_id, rule_id)
SELECT unnest($2::bigint[]), $1
ON CONFLICT DO NOTHING`
	setRulePlaylists = `
WITH removed AS (
    DELETE FROM playlist_play_rules WHERE rule_id = $1 AND playlist_id <> ALL($2::bigint[])
)
INSERT INTO playlist_play_rules (playlist_id, rule_id)
SELECT unnest($2::bigint[]), $1
ON CONFLICT DO NOTHING`
)

func (s *Store) setLinks(ctx context.Context, op, sql string, id uint64, ids []uint64) error {
	if ids == nil {
		ids = []uint64{}
	}
	if _, err := s.q.Exec(ctx, sql, id, ids); err != nil {
		return writeError(fmt.Sprintf("%s %d", op, id), err)
	}
	return nil
}

// SetTargets makes trackIDs and playlistIDs the targets of the rule. Run it
// in a transaction: two statements.
func (s *Store) SetTargets(ctx context.Context, ruleID uint64, trackIDs, playlistIDs []uint64) error {
	if err := s.setLinks(ctx, "set tracks of play rule", setRuleTracks, ruleID, trackIDs); err != nil {
		return err
	}
	return s.setLinks(ctx, "set playlists of play rule", setRulePlaylists, ruleID, playlistIDs)
}

// SetTrackRules makes ruleIDs the rules of the track.
func (s *Store) SetTrackRules(ctx context.Context, trackID uint64, ruleIDs []uint64) error {
	return s.setLinks(ctx, "set play rules of track", setTrackRules, trackID, ruleIDs)
}

// SetPlaylistRules makes ruleIDs the rules of the playlist.
func (s *Store) SetPlaylistRules(ctx context.Context, playlistID uint64, ruleIDs []uint64) error {
	return s.setLinks(ctx, "set play rules of playlist", setPlaylistRules, playlistID, ruleIDs)
}

func (s *Store) ids(ctx context.Context, op, sql string, id uint64) ([]uint64, error) {
	rows, err := s.q.Query(ctx, sql, id)
	if err != nil {
		return nil, fmt.Errorf("%s %d: %w", op, id, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uint64])
	if err != nil {
		return nil, fmt.Errorf("%s %d: %w", op, id, err)
	}
	return ids, nil
}

// TrackRules returns the rule IDs of a track (primary key lookup).
func (s *Store) TrackRules(ctx context.Context, trackID uint64) ([]uint64, error) {
	return s.ids(ctx, "select play rules of track",
		`SELECT rule_id FROM track_play_rules WHERE track_id = $1 ORDER BY rule_id`, trackID)
}

// PlaylistRules returns the rule IDs of a playlist (primary key lookup).
func (s *Store) PlaylistRules(ctx context.Context, playlistID uint64) ([]uint64, error) {
	return s.ids(ctx, "select play rules of playlist",
		`SELECT rule_id FROM playlist_play_rules WHERE playlist_id = $1 ORDER BY rule_id`, playlistID)
}
