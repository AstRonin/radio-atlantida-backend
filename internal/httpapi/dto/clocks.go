package dto

import "time"

// ClockPlaylist is a playlist of a clock with its placement rule (§7.2).
type ClockPlaylist struct {
	PlaylistID     uint64  `json:"playlist_id"`
	Position       int     `json:"position" doc:"Order of the start and end tracks of several playlists."`
	ClockRule      *string `json:"clock_rule,omitempty" enum:"fill,spread,start,end,start_end,middle" doc:"Override of the playlist's own rule in this clock; absent: the playlist's rule."`
	ClockCount     *int    `json:"clock_count,omitempty" doc:"Tracks of a spread override."`
	FillWeight     *int    `json:"fill_weight,omitempty" doc:"Share of a fill playlist; either every fill playlist has one or none."`
	EffectiveRule  string  `json:"effective_rule" enum:"fill,spread,start,end,start_end,middle" doc:"The rule in this clock: the override, else the playlist's own."`
	EffectiveCount *int    `json:"effective_count,omitempty" doc:"Tracks of the effective spread rule."`
}

// ClockInsert is a timed insert of a clock template: it plays in every hour
// of the clock (§7.2).
type ClockInsert struct {
	ID          uint64  `json:"id"`
	OffsetMS    int     `json:"offset_ms" doc:"Target start from the start of the hour, whole seconds."`
	ToleranceMS int     `json:"tolerance_ms" doc:"It may start this much before or after its target."`
	TrackID     *uint64 `json:"track_id,omitempty" doc:"Exactly one of track_id and playlist_id is set."`
	PlaylistID  *uint64 `json:"playlist_id,omitempty" doc:"A track of this playlist is drawn for every hour."`
}

// Clock is a 60-minute template: playlists and timed inserts.
type Clock struct {
	ID        uint64          `json:"id"`
	Name      string          `json:"name"`
	Playlists []ClockPlaylist `json:"playlists" nullable:"false" doc:"In position order."`
	Inserts   []ClockInsert   `json:"inserts" nullable:"false" doc:"By offset."`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ClockSummary is a clock in the list.
type ClockSummary struct {
	ID            uint64    `json:"id"`
	Name          string    `json:"name"`
	PlaylistCount int       `json:"playlist_count"`
	InsertCount   int       `json:"insert_count"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ClockList is every clock in name order.
type ClockList struct {
	Items []ClockSummary `json:"items" nullable:"false"`
}

// ClockPlaylistBody is a playlist of a clock save. Its position is its
// index in playlists.
type ClockPlaylistBody struct {
	PlaylistID uint64  `json:"playlist_id" minimum:"1"`
	ClockRule  *string `json:"clock_rule,omitempty" nullable:"true" enum:"fill,spread,start,end,start_end,middle" doc:"Leave out for the playlist's own rule."`
	ClockCount *int    `json:"clock_count,omitempty" nullable:"true" minimum:"1" maximum:"12" doc:"Required with clock_rule spread, not allowed otherwise."`
	FillWeight *int    `json:"fill_weight,omitempty" nullable:"true" minimum:"1" maximum:"100" doc:"Share of a fill playlist; set on every fill playlist or on none."`
}

// ClockInsertBody is a timed insert of a clock save.
type ClockInsertBody struct {
	OffsetMS    int     `json:"offset_ms" minimum:"0" maximum:"3599000" multipleOf:"1000" doc:"Target start, whole seconds (29:40 is 1780000)."`
	ToleranceMS *int    `json:"tolerance_ms,omitempty" nullable:"true" minimum:"0" maximum:"600000" doc:"Default: setting rotation.default_fixed_tolerance_ms."`
	TrackID     *uint64 `json:"track_id,omitempty" nullable:"true" minimum:"1" doc:"Exactly one of track_id and playlist_id."`
	PlaylistID  *uint64 `json:"playlist_id,omitempty" nullable:"true" minimum:"1"`
}

// ClockBody is the body of POST /api/clocks and PUT /api/clocks/{id}. PUT
// replaces the clock: playlists and inserts left out are removed.
type ClockBody struct {
	Name      string              `json:"name" minLength:"1" maxLength:"200"`
	Playlists []ClockPlaylistBody `json:"playlists" minItems:"1" maxItems:"100" doc:"At least one fill playlist; any types."`
	Inserts   []ClockInsertBody   `json:"inserts,omitempty" nullable:"false" maxItems:"100"`
}

// WeekdayHour is an hour of the week, station local.
type WeekdayHour struct {
	Weekday int `json:"weekday" doc:"1 = Monday ... 7 = Sunday."`
	Hour    int `json:"hour"`
}

// ClockWarning is a problem of the dry check or of the sample hour; it does
// not block the save. The panel translates by code.
type ClockWarning struct {
	Code          string        `json:"code" enum:"playlist_no_tracks,fill_few_tracks,fill_no_interval_rule,rule_blocked_by_play_rules,insert_replaced,insert_skipped,hour_insert_dropped,rule_slot_skipped,fill_general_pool,fill_emergency,gap_unfilled,reschedule_dropped"`
	Message       string        `json:"message" doc:"English message for developers."`
	PlaylistIndex *int          `json:"playlist_index,omitempty" doc:"Index into playlists."`
	InsertIndex   *int          `json:"insert_index,omitempty" doc:"Index into inserts."`
	Hours         []WeekdayHour `json:"hours,omitempty" nullable:"false" doc:"The grid hours in which play rules block every track (rule_blocked_by_play_rules)."`
}

// ClockSampleItem is a track of the sample hour.
type ClockSampleItem struct {
	OffsetMS    int     `json:"offset_ms" doc:"Start from the start of the hour."`
	LengthMS    int     `json:"length_ms" doc:"How long it plays."`
	TrackID     uint64  `json:"track_id"`
	Title       string  `json:"title"`
	Artist      *string `json:"artist,omitempty"`
	Source      string  `json:"source" enum:"fill,rule,insert,hour_insert,reschedule,general"`
	PlaylistID  *uint64 `json:"playlist_id,omitempty" doc:"The fill or rule playlist it was drawn from."`
	InsertIndex *int    `json:"insert_index,omitempty" doc:"Index into inserts, for an insert item."`
	Truncated   bool    `json:"truncated" doc:"Cut at the next hard point and faded out."`
}

// ClockSample is an hour of the clock built with the real library and
// history but not stored: the next grid hour of the clock in the plan
// horizon, else the next station hour. Planned hours may differ by seed
// and history.
type ClockSample struct {
	HourStartUTC time.Time         `json:"hour_start_utc"`
	Items        []ClockSampleItem `json:"items" nullable:"false" doc:"In time order."`
}

// ClockSaveResult is the saved clock with its sample hour and warnings
// (§6.0).
type ClockSaveResult struct {
	Clock    Clock          `json:"clock"`
	Sample   *ClockSample   `json:"sample,omitempty" doc:"Absent only when the sample could not be built (the error is logged)."`
	Warnings []ClockWarning `json:"warnings" nullable:"false"`
	// BuildOperationID is absent only when the rebuild could not be
	// started; the plan job catches up within minutes.
	BuildOperationID *uint64 `json:"build_operation_id,omitempty" doc:"The clock_build operation that rebuilds the clock's planned hours (clock_build.completed or .failed)."`
}
