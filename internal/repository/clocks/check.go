package clocks

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
)

// CheckStore reads what the dry check needs (pgx).
type CheckStore struct {
	q db.Querier
}

// NewCheck returns the store on q (the pool).
func NewCheck(q db.Querier) *CheckStore { return &CheckStore{q: q} }

type candidateRow struct {
	TrackID     int64   `db:"track_id"`
	DurationMS  *int32  `db:"duration_ms"`
	PlaylistIDs []int64 `db:"playlist_ids"`
	RuleIDs     []int64 `db:"rule_ids"`
}

func (r candidateRow) domain() clocks.Candidate {
	c := clocks.Candidate{TrackID: r.TrackID, PlaylistIDs: r.PlaylistIDs, RuleIDs: r.RuleIDs}
	if r.DurationMS != nil {
		c.DurationMS = new(int(*r.DurationMS))
	}
	return c
}

// Candidates returns the tracks of the playlists, each with the requested
// playlists that hold it and the play rules of the track and of every
// playlist it is in (§7.3). Membership uses ix_track_playlists_playlist_id;
// the rule lookups use ix_track_play_rules_track_id,
// ix_track_playlists_track_id, and ix_playlist_play_rules_playlist_id.
func (s *CheckStore) Candidates(ctx context.Context, playlistIDs []int64) (clocks.Candidates, error) {
	out := clocks.Candidates{Rules: map[int64]playrules.RuleInput{}}
	if len(playlistIDs) == 0 {
		return out, nil
	}
	rows, err := s.q.Query(ctx, `
SELECT t.id AS track_id, t.duration_ms,
       array_agg(tp.playlist_id ORDER BY tp.playlist_id) AS playlist_ids,
       ARRAY(SELECT tr.rule_id FROM track_play_rules tr WHERE tr.track_id = t.id
             UNION
             SELECT pr.rule_id FROM track_playlists tp2
             JOIN playlist_play_rules pr ON pr.playlist_id = tp2.playlist_id
             WHERE tp2.track_id = t.id) AS rule_ids
FROM track_playlists tp
JOIN tracks t ON t.id = tp.track_id
WHERE tp.playlist_id = ANY($1)
GROUP BY t.id
ORDER BY t.id`, playlistIDs)
	if err != nil {
		return clocks.Candidates{}, fmt.Errorf("select candidates: %w", err)
	}
	out.Tracks, err = db.CollectAll(rows, candidateRow.domain)
	if err != nil {
		return clocks.Candidates{}, fmt.Errorf("scan candidates: %w", err)
	}

	var ruleIDs []int64
	seen := map[int64]bool{}
	for _, t := range out.Tracks {
		for _, id := range t.RuleIDs {
			if !seen[id] {
				seen[id] = true
				ruleIDs = append(ruleIDs, id)
			}
		}
	}
	if len(ruleIDs) == 0 {
		return out, nil
	}
	rows, err = s.q.Query(ctx, `
SELECT id, window_from, window_to, weekdays FROM play_rules WHERE id = ANY($1)`, ruleIDs)
	if err != nil {
		return clocks.Candidates{}, fmt.Errorf("select play rules: %w", err)
	}
	rules, err := db.CollectAll(rows, func(r ruleRow) ruleRow { return r })
	if err != nil {
		return clocks.Candidates{}, fmt.Errorf("scan play rules: %w", err)
	}
	for _, r := range rules {
		out.Rules[r.ID] = r.domain()
	}
	return out, nil
}

// ruleRow holds the time limits of a play rule; the interval and per-hour
// limits depend on the plan and are not part of the dry check.
type ruleRow struct {
	ID         int64       `db:"id"`
	WindowFrom pgtype.Time `db:"window_from"`
	WindowTo   pgtype.Time `db:"window_to"`
	Weekdays   []int16     `db:"weekdays"`
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
	return out
}

// TrackSeparationHours returns the longest track separation interval, or 0
// without a track rule.
func (s *CheckStore) TrackSeparationHours(ctx context.Context) (int, error) {
	var h int
	err := s.q.QueryRow(ctx,
		`SELECT COALESCE(max(min_interval_hours), 0) FROM separation_rules WHERE scope = 'track'`).Scan(&h)
	if err != nil {
		return 0, fmt.Errorf("select track separation: %w", err)
	}
	return h, nil
}
