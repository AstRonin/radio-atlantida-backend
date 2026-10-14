package dto

import "time"

// TrackPlanClock is the clock of a built hour.
type TrackPlanClock struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

// TrackPlanItem is a track of the plan (§7).
type TrackPlanItem struct {
	ID              uint64    `json:"id"`
	TrackID         uint64    `json:"track_id"`
	Title           string    `json:"title"`
	ArtistName      *string   `json:"artist_name,omitempty"`
	DurationMS      int       `json:"duration_ms" doc:"Length of the track file."`
	ElementID       *uint64   `json:"element_id,omitempty" doc:"The clock element it was planned for; absent for filler or a removed element."`
	PlannedStartUTC time.Time `json:"planned_start_utc"`
	PlannedOutMS    int       `json:"planned_out_ms" doc:"Out point from the start of the track; shorter than duration_ms when truncated."`
	Truncated       bool      `json:"truncated" doc:"The track crosses its segment end; it fades out and ends there."`
	Source          string    `json:"source" enum:"clock,filler_pool,filler_random,filler_playlist,filler_artist,filler_answer,reschedule"`
	Status          string    `json:"status" enum:"planned,queued,aired,missed,replaced"`
}

// TrackPlanHour is one built hour of the plan.
type TrackPlanHour struct {
	HourStartUTC    time.Time       `json:"hour_start_utc"`
	Clock           TrackPlanClock  `json:"clock"`
	GridSlotID      *uint64         `json:"grid_slot_id,omitempty"`
	Seed            int64           `json:"seed" doc:"Seed of the build; the same input and seed give the same plan."`
	BuiltAt         time.Time       `json:"built_at"`
	FillerRequestID *uint64         `json:"filler_request_id,omitempty" doc:"The filler reminder of this build."`
	Items           []TrackPlanItem `json:"items" nullable:"false" doc:"In time order."`
}

// TrackPlan is the built hours of a range, in time order.
type TrackPlan struct {
	Items []TrackPlanHour `json:"items" nullable:"false"`
}

// TrackPlanRebuildBody is the body of POST /api/track-plan/rebuild.
type TrackPlanRebuildBody struct {
	From time.Time `json:"from" doc:"Hours that start at or after this instant."`
	To   time.Time `json:"to" doc:"... and before this one; at most 7 days after from. Hours that have started are never rebuilt."`
}

// OperationAccepted is the answer to a request that started an
// asynchronous operation (§6.5).
type OperationAccepted struct {
	OperationID uint64 `json:"operation_id" doc:"Its outcome arrives as <kind>.completed or <kind>.failed, and through GET /api/operations/{id}."`
}

// FillerRequest is a filler reminder (§7).
type FillerRequest struct {
	ID            uint64    `json:"id"`
	ClockID       uint64    `json:"clock_id"`
	ClockName     string    `json:"clock_name"`
	HourStartUTC  time.Time `json:"hour_start_utc"`
	Reason        string    `json:"reason" enum:"short_clock,pool_empty"`
	Mode          string    `json:"mode" enum:"none,ask,pool" doc:"none: the clock is incomplete; ask: the holder asks at air time; pool: the pool is empty."`
	Status        string    `json:"status" enum:"pending,answered,defaulted,expired"`
	CreatedAt     time.Time `json:"created_at"`
	DeadlineAt    time.Time `json:"deadline_at" doc:"Without an answer by then, the clock's default filler mode applies."`
	AnswerTrackID *uint64   `json:"answer_track_id,omitempty"`
}

// FillerRequestList is a list of reminders, newest hour first.
type FillerRequestList struct {
	Items []FillerRequest `json:"items" nullable:"false"`
}

// FillerAnswerBody is the body of POST /api/filler-requests/{id}/answer.
type FillerAnswerBody struct {
	TrackID uint64 `json:"track_id" minimum:"1" doc:"Fills the hour's first waiting segment; the clock's default filler mode fills the others."`
}
