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
	InsertID        *uint64   `json:"insert_id,omitempty" doc:"The template insert of an insert item; absent when the insert was removed since."`
	HourInsertID    *uint64   `json:"hour_insert_id,omitempty" doc:"The one-time insert of an hour_insert or reschedule item."`
	PlannedStartUTC time.Time `json:"planned_start_utc"`
	PlannedInMS     int       `json:"planned_in_ms" minimum:"0" doc:"In point from the start of the file (the track's cue in when it was planned)."`
	PlannedOutMS    int       `json:"planned_out_ms" doc:"Out point from the start of the file; the length is planned_out_ms - planned_in_ms, shorter than the track's air_duration_ms when truncated."`
	Truncated       bool      `json:"truncated" doc:"The track crosses the next insert or the end of the hour; it fades out and ends there."`
	Source          string    `json:"source" enum:"fill,rule,insert,hour_insert,reschedule,general"`
	Status          string    `json:"status" enum:"planned,queued,aired,missed,replaced"`
}

// TrackPlanInsert is a one-time insert of a built hour (§7.2).
type TrackPlanInsert struct {
	ID          uint64  `json:"id"`
	Placement   string  `json:"placement" enum:"start,end,fixed"`
	OffsetMS    *int    `json:"offset_ms,omitempty" doc:"Target start of a fixed insert, whole seconds."`
	ToleranceMS int     `json:"tolerance_ms"`
	TrackID     uint64  `json:"track_id"`
	MissedID    *uint64 `json:"missed_id,omitempty" doc:"The missed item it airs again (a reschedule)."`
}

// TrackPlanHour is one built hour of the plan.
type TrackPlanHour struct {
	HourStartUTC time.Time         `json:"hour_start_utc"`
	Clock        TrackPlanClock    `json:"clock"`
	GridSlotID   *uint64           `json:"grid_slot_id,omitempty"`
	Seed         int64             `json:"seed" doc:"Seed of the build; the same input and seed give the same plan."`
	BuiltAt      time.Time         `json:"built_at"`
	Items        []TrackPlanItem   `json:"items" nullable:"false" doc:"In time order."`
	Inserts      []TrackPlanInsert `json:"inserts" nullable:"false" doc:"One-time inserts of the hour, in ID order; they stay when the hour is rebuilt."`
}

// TrackPlan is the built hours of a range, in time order.
type TrackPlan struct {
	Items []TrackPlanHour `json:"items" nullable:"false"`
}

// TrackPlanRebuildBody is the body of POST /api/track-plan/rebuild.
type TrackPlanRebuildBody struct {
	From time.Time `json:"from" doc:"Hours that start at or after this instant."`
	To   time.Time `json:"to" doc:"... and before this one; at most 31 days after from. Hours that have started are never rebuilt."`
}

// HourInsertBody is the body of POST /api/track-plan/inserts.
type HourInsertBody struct {
	HourStartUTC time.Time `json:"hour_start_utc" doc:"Start of a future station hour within the track plan, not frozen."`
	OffsetMS     int       `json:"offset_ms" minimum:"0" maximum:"3599000" multipleOf:"1000" doc:"Target start, whole seconds (18:30:00 in its hour is 1800000)."`
	ToleranceMS  *int      `json:"tolerance_ms,omitempty" nullable:"true" minimum:"0" maximum:"600000" doc:"Default: setting rotation.default_fixed_tolerance_ms."`
	TrackID      uint64    `json:"track_id" minimum:"1"`
}

// OperationAccepted is the answer to a request that started an
// asynchronous operation (§6.5).
type OperationAccepted struct {
	OperationID uint64 `json:"operation_id" doc:"Its outcome arrives as <kind>.completed or <kind>.failed, and through GET /api/operations/{id}."`
}

// TrackSimulationItem is a track of a simulated hour (not stored).
type TrackSimulationItem struct {
	StartUTC     time.Time `json:"start_utc"`
	TrackID      uint64    `json:"track_id"`
	Title        string    `json:"title"`
	ArtistName   *string   `json:"artist_name,omitempty"`
	PlannedInMS  int       `json:"planned_in_ms" minimum:"0" doc:"In point from the start of the file."`
	PlannedOutMS int       `json:"planned_out_ms" doc:"Out point from the start of the file."`
	Truncated    bool      `json:"truncated"`
	Source       string    `json:"source" enum:"fill,rule,insert,hour_insert,reschedule,general"`
}

// TrackSimulationWarning is a warning of a simulated hour.
type TrackSimulationWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// TrackSimulationHour is one grid hour built by a simulation.
type TrackSimulationHour struct {
	HourStartUTC time.Time                `json:"hour_start_utc"`
	Clock        TrackPlanClock           `json:"clock"`
	ItemCount    int                      `json:"item_count"`
	Warnings     []TrackSimulationWarning `json:"warnings" nullable:"false"`
	Items        []TrackSimulationItem    `json:"items,omitempty" doc:"Only with items=true; in time order."`
}

// TrackSimulation is the result of a simulation of a date range. Nothing is
// stored: the plan, the scheduled items and the one-time inserts are not
// changed.
type TrackSimulation struct {
	From           string                `json:"from" doc:"First station local date."`
	To             string                `json:"to" doc:"Last station local date, inclusive."`
	Hours          []TrackSimulationHour `json:"hours" nullable:"false" doc:"The built hours, in time order."`
	HoursBuilt     int                   `json:"hours_built"`
	HoursSkipped   int                   `json:"hours_skipped" doc:"Grid hours whose clock no longer exists; not built."`
	Items          int                   `json:"items"`
	WarningsByCode map[string]int        `json:"warnings_by_code" nullable:"false" doc:"Warnings of all built hours, by code."`
	ElapsedMS      int64                 `json:"elapsed_ms"`
	Queries        int                   `json:"queries" doc:"Reads of the run (settings, grid, library, history, clocks, one-time inserts per hour)."`
	LibraryTracks  int                   `json:"library_tracks"`
	HistoryPlays   int                   `json:"history_plays" doc:"Plays read before from: the history of the first hour."`
}
