package library

import (
	"context"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// CueState locks the track row until the transaction ends and returns its
// duration, cue, and whether a plan item of it is queued
// (ix_scheduled_items_track_id_planned_start_utc).
func (s *Tracks) CueState(ctx context.Context, id uint64) (library.CueState, error) {
	var rows []struct {
		DurationMS *int `gorm:"column:duration_ms"`
		CueInMS    int  `gorm:"column:cue_in_ms"`
		CueOutMS   *int `gorm:"column:cue_out_ms"`
		FadeInMS   int  `gorm:"column:fade_in_ms"`
		FadeOutMS  int  `gorm:"column:fade_out_ms"`
		Queued     bool
	}
	err := s.g.WithContext(ctx).Raw(`
SELECT tr.duration_ms, tr.cue_in_ms, tr.cue_out_ms, tr.fade_in_ms, tr.fade_out_ms,
       EXISTS (SELECT 1 FROM scheduled_items si WHERE si.track_id = tr.id AND si.status = 'queued') AS queued
FROM tracks tr
WHERE tr.id = ?
FOR UPDATE OF tr`, id).Scan(&rows).Error
	if err != nil {
		return library.CueState{}, fmt.Errorf("lock track %d: %w", id, err)
	}
	if len(rows) == 0 {
		return library.CueState{}, library.ErrTrackNotFound
	}
	r := rows[0]
	return library.CueState{
		DurationMS: r.DurationMS,
		Cue:        library.Cue{InMS: r.CueInMS, OutMS: r.CueOutMS, FadeInMS: r.FadeInMS, FadeOutMS: r.FadeOutMS},
		Queued:     r.Queued,
	}, nil
}

// SetCue writes the cue of a track. ck_tracks_cue and ck_tracks_fades
// guard the rules that need no duration.
func (s *Tracks) SetCue(ctx context.Context, id uint64, c library.Cue) error {
	res := s.g.WithContext(ctx).Exec(`
UPDATE tracks SET cue_in_ms = ?, cue_out_ms = ?, fade_in_ms = ?, fade_out_ms = ? WHERE id = ?`,
		c.InMS, c.OutMS, c.FadeInMS, c.FadeOutMS, id)
	if res.Error != nil {
		return fmt.Errorf("update cue of track %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrTrackNotFound
	}
	return nil
}

// PlannedHours returns the range of the built plan hours that hold a
// planned item of the track: plan_hours holds the plan horizon (48 rows at
// most), each probed on ix_scheduled_items_track_id_planned_start_utc.
func (s *Tracks) PlannedHours(ctx context.Context, id uint64) (*library.HourRange, error) {
	var r struct {
		First *time.Time
		Last  *time.Time
	}
	err := s.g.WithContext(ctx).Raw(`
SELECT min(h.hour_start_utc) AS first, max(h.hour_start_utc) AS last
FROM plan_hours h
WHERE EXISTS (SELECT 1 FROM scheduled_items si
              WHERE si.track_id = ? AND si.status = 'planned'
                AND si.planned_start_utc >= h.hour_start_utc
                AND si.planned_start_utc < h.hour_start_utc + interval '1 hour')`, id).Scan(&r).Error
	if err != nil {
		return nil, fmt.Errorf("select planned hours of track %d: %w", id, err)
	}
	if r.First == nil || r.Last == nil {
		return nil, nil
	}
	return &library.HourRange{From: r.First.UTC(), To: r.Last.UTC().Add(time.Hour)}, nil
}

// MissedHours returns the range of the hours of the open missed items of the
// track: play_log rows that are missed and unresolved. A missed item is moved
// out of scheduled_items into the play log (outage.MarkMissed), so it is
// read there and is not bounded by the plan horizon; the probe is on
// ix_play_log_track_id_started_at.
func (s *Tracks) MissedHours(ctx context.Context, id uint64) (*library.HourRange, error) {
	var r struct {
		First *time.Time
		Last  *time.Time
	}
	err := s.g.WithContext(ctx).Raw(`
SELECT min(date_trunc('hour', p.planned_start_utc)) AS first, max(date_trunc('hour', p.planned_start_utc)) AS last
FROM play_log p
WHERE p.track_id = ? AND p.status = 'missed' AND p.resolution = 'open'`, id).Scan(&r).Error
	if err != nil {
		return nil, fmt.Errorf("select missed hours of track %d: %w", id, err)
	}
	if r.First == nil || r.Last == nil {
		return nil, nil
	}
	return &library.HourRange{From: r.First.UTC(), To: r.Last.UTC().Add(time.Hour)}, nil
}
