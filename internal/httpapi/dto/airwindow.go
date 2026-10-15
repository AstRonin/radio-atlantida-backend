package dto

import "time"

// AirWindow is the air as the manager needs it now (docs/phase-17-plan.md):
// the last items that were due, the item on air, and the next planned
// items, in air order.
type AirWindow struct {
	GeneratedAt          time.Time      `json:"generated_at" doc:"Server time (UTC) of this answer."`
	ControlLinkConnected bool           `json:"control_link_connected" doc:"false: the Liquidsoap control link is down, so the item on air is unknown; the list then has no now row although something may play."`
	Rows                 []AirWindowRow `json:"rows" doc:"In air order: played rows oldest first, at most one now row, then queued rows in planned order. At most air.played_count + 1 + air.queued_count rows."`
}

// AirWindowRow is one line of the air window. Every kind has the same shape.
type AirWindowRow struct {
	Kind         string     `json:"kind" enum:"played,now,queued"`
	Status       string     `json:"status" enum:"aired,truncated,missed,now,queued,planned" doc:"played rows: aired, truncated, or missed (shown as \"Not aired\"); now: on air; queued rows: queued (already pushed to Liquidsoap) or planned."`
	ItemID       *uint64    `json:"item_id" nullable:"true" doc:"The play log row (played) or the plan item (now, queued); null for a now row without a plan item, such as an emergency track."`
	TrackID      uint64     `json:"track_id"`
	Title        string     `json:"title"`
	ArtistName   *string    `json:"artist_name" nullable:"true"`
	Source       string     `json:"source" enum:"plan,emergency,live" doc:"plan: an item of the track plan; emergency: a track of the emergency playlist."`
	StartUTC     time.Time  `json:"start_utc" doc:"When it started (aired, truncated, now) or was planned to start (missed, queued)."`
	EndUTC       *time.Time `json:"end_utc" nullable:"true" doc:"Aired and truncated: start plus the length that aired. Now: start plus the out point. Missed and queued: the planned end. Null only when the length is unknown (an emergency track without a duration)."`
	Truncated    bool       `json:"truncated"`
	Resolution   *string    `json:"resolution" nullable:"true" enum:"open,rescheduled,dismissed" doc:"Missed rows only; null otherwise."`
	OutageID     *uint64    `json:"outage_id" nullable:"true" doc:"Missed rows: the outage that kept the item off the air; null otherwise."`
	InLiquidsoap bool       `json:"in_liquidsoap" doc:"true for the now row and for queued rows already pushed to Liquidsoap; false for planned rows and played rows."`
}
