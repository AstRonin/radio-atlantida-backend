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
	TrackID     uint64   `db:"track_id"`
	AirMS       *int32   `db:"air_ms"`
	PlaylistIDs []uint64 `db:"playlist_ids"`
	RuleIDs     []uint64 `db:"rule_ids"`
}

func (r candidateRow) domain() clocks.Candidate {
	c := clocks.Candidate{TrackID: r.TrackID, PlaylistIDs: r.PlaylistIDs, RuleIDs: r.RuleIDs}
	if r.AirMS != nil {
		c.AirMS = new(int(*r.AirMS))
	}
	return c
}

// Candidates returns the tracks of the playlists, each with its length on
// air (null when rotation cannot plan it, as in the rotation library), the
// requested playlists that hold it, and the play rules of the track and of
// every playlist it is in (§7.3); and the rules attached to each requested
// playlist. Membership uses ix_track_playlists_playlist_id;
// the rule lookups use ix_track_play_rules_track_id,
// ix_track_playlists_track_id, and ix_playlist_play_rules_playlist_id.
func (s *CheckStore) Candidates(ctx context.Context, playlistIDs []uint64) (clocks.Candidates, error) {
	out := clocks.Candidates{Rules: map[uint64]playrules.RuleInput{}, PlaylistRules: map[uint64][]uint64{}}
	if len(playlistIDs) == 0 {
		return out, nil
	}
	rows, err := s.q.Query(ctx, `
SELECT t.id AS track_id,
       CASE WHEN t.duration_ms IS NOT NULL AND COALESCE(t.cue_out_ms, t.duration_ms) > t.cue_in_ms
                 AND (t.cue_out_ms IS NULL OR t.cue_out_ms <= t.duration_ms)
            THEN COALESCE(t.cue_out_ms, t.duration_ms) - t.cue_in_ms END AS air_ms,
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

	rows, err = s.q.Query(ctx, `
SELECT playlist_id, rule_id FROM playlist_play_rules WHERE playlist_id = ANY($1)`, playlistIDs)
	if err != nil {
		return clocks.Candidates{}, fmt.Errorf("select playlist rules: %w", err)
	}
	links, err := db.CollectAll(rows, func(r linkRow) linkRow { return r })
	if err != nil {
		return clocks.Candidates{}, fmt.Errorf("scan playlist rules: %w", err)
	}

	var ruleIDs []uint64
	seen := map[uint64]bool{}
	add := func(id uint64) {
		if !seen[id] {
			seen[id] = true
			ruleIDs = append(ruleIDs, id)
		}
	}
	for _, l := range links {
		out.PlaylistRules[l.PlaylistID] = append(out.PlaylistRules[l.PlaylistID], l.RuleID)
		add(l.RuleID)
	}
	for _, t := range out.Tracks {
		for _, id := range t.RuleIDs {
			add(id)
		}
	}
	if len(ruleIDs) == 0 {
		return out, nil
	}
	rows, err = s.q.Query(ctx, `
SELECT id, window_from, window_to, weekdays, min_interval_minutes FROM play_rules WHERE id = ANY($1)`, ruleIDs)
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

type linkRow struct {
	PlaylistID uint64 `db:"playlist_id"`
	RuleID     uint64 `db:"rule_id"`
}

// ruleRow holds the time limits and the track interval of a play rule; the
// other limits depend on the plan and are not part of the dry check.
type ruleRow struct {
	ID                 uint64      `db:"id"`
	WindowFrom         pgtype.Time `db:"window_from"`
	WindowTo           pgtype.Time `db:"window_to"`
	Weekdays           []int16     `db:"weekdays"`
	MinIntervalMinutes *int32      `db:"min_interval_minutes"`
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
	return out
}
