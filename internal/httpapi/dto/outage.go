package dto

import "time"

// PlayHistoryEntry is a row of the played-tracks list: an aired,
// truncated, or missed item (§7.1).
type PlayHistoryEntry struct {
	ID         uint64  `json:"id"`
	Status     string  `json:"status" enum:"aired,truncated,missed" doc:"missed: planned but not aired (shown as \"Not aired\")."`
	Source     string  `json:"source" enum:"auto,live,emergency"`
	TrackID    uint64  `json:"track_id"`
	Title      string  `json:"title"`
	ArtistName *string `json:"artist_name,omitempty"`
	// PlannedStartUTC is absent for emergency tracks, which were never
	// planned.
	PlannedStartUTC  *time.Time        `json:"planned_start_utc,omitempty"`
	StartedAt        *time.Time        `json:"started_at,omitempty" doc:"Absent for missed items."`
	PlannedInMS      int               `json:"planned_in_ms" minimum:"0" doc:"In point the item was planned with, from the start of the file (its cue in); 0 for emergency tracks."`
	PlannedOutMS     *int              `json:"planned_out_ms,omitempty" doc:"Out point from the start of the file; the planned length is planned_out_ms - planned_in_ms. Absent for emergency tracks."`
	ActualDurationMS *int              `json:"actual_duration_ms,omitempty" doc:"How long it played; absent for missed items."`
	Clock            *PlayHistoryClock `json:"clock,omitempty"`
	Outage           *OutageRef        `json:"outage,omitempty" doc:"Missed items: the outage that kept them off the air."`
	Resolution       *string           `json:"resolution,omitempty" enum:"open,rescheduled,dismissed" doc:"Missed items only."`
	ResolvedAt       *time.Time        `json:"resolved_at,omitempty"`
	ResolvedItemID   *uint64           `json:"resolved_item_id,omitempty" doc:"The plan item that airs a rescheduled item again."`
}

// PlayHistoryClock is the clock an item was planned in.
type PlayHistoryClock struct {
	ID   uint64  `json:"id"`
	Name *string `json:"name,omitempty" doc:"Absent when the clock was deleted."`
}

// OutageRef is the outage of a missed item.
type OutageRef struct {
	ID        uint64  `json:"id"`
	Component string  `json:"component" enum:"icecast,liquidsoap,backend,network"`
	Reason    *string `json:"reason,omitempty"`
}

// PlayHistory is a page of the played-tracks list, newest first.
type PlayHistory struct {
	Items []PlayHistoryEntry `json:"items"`
	// NextCursor is absent on the last page.
	NextCursor *string `json:"next_cursor,omitempty" doc:"Pass as cursor for the next (older) page; absent on the last page."`
}

// MissedItemList lists missed items.
type MissedItemList struct {
	Items []PlayHistoryEntry `json:"items"`
}

// RescheduleBody places a missed item into a later hour (§7.1, §7.2).
type RescheduleBody struct {
	TargetHourStartUTC time.Time `json:"target_hour_start_utc" doc:"Start of a future station hour within the track plan."`
	Placement          string    `json:"placement" enum:"start,end,fixed"`
	FixedMinute        *int      `json:"fixed_minute,omitempty" minimum:"0" maximum:"59" doc:"Required for placement fixed."`
	FixedToleranceMS   *int      `json:"fixed_tolerance_ms,omitempty" minimum:"0" maximum:"600000" doc:"Start tolerance of a fixed placement; default rotation.default_fixed_tolerance_ms."`
}

// Outage is a broadcast interruption (§7.1).
type Outage struct {
	ID           uint64     `json:"id"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty" doc:"Absent while the outage is open."`
	DetectedBy   string     `json:"detected_by" enum:"icecast,liquidsoap_callback,liquidsoap_control,backend"`
	Component    string     `json:"component" enum:"icecast,liquidsoap,backend,network"`
	Reason       *string    `json:"reason,omitempty"`
	ReconciledAt *time.Time `json:"reconciled_at,omitempty" doc:"When its missed items were marked."`
	MissedCount  int        `json:"missed_count"`
}

// OutageList lists outages, newest first.
type OutageList struct {
	Items []Outage `json:"items"`
}
