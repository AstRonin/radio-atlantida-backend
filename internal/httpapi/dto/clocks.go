package dto

import "time"

// ClockElement is one slot of a clock (§4, §7.2).
type ClockElement struct {
	ID                  uint64   `json:"id"`
	Placement           string   `json:"placement" enum:"flow,start,end,fixed"`
	FixedMinute         *int     `json:"fixed_minute,omitempty" doc:"Target start minute of a fixed element."`
	FixedToleranceMS    int      `json:"fixed_tolerance_ms" doc:"A fixed element may start this much before or after its minute."`
	NominalDurationMS   int      `json:"nominal_duration_ms"`
	DurationToleranceMS int      `json:"duration_tolerance_ms" doc:"A chosen track lasts nominal ± tolerance."`
	FillerMode          string   `json:"filler_mode" enum:"none,ask,random,playlist,artist,pool" doc:"Filler of the segment this element holds (see ClockSegment.holder_index)."`
	FillerSourceID      *uint64  `json:"filler_source_id,omitempty" doc:"Playlist ID for playlist, artist ID for artist."`
	PlaylistIDs         []uint64 `json:"playlist_ids" nullable:"false" doc:"The track is chosen from the union of their tracks, in ID order."`
}

// ClockSegment is a part of the hour between fixed elements, fitted
// separately (§7.2). Times are milliseconds from the start of the hour, on
// nominal durations.
type ClockSegment struct {
	Index          int    `json:"index"`
	StartMS        int    `json:"start_ms" doc:"Where its content starts: the start of the hour or the end of the fixed element before it."`
	EndMS          int    `json:"end_ms" doc:"Target end: the minute of the fixed element that closes it, or 3600000."`
	ContentMS      int    `json:"content_ms" doc:"Sum of the nominal durations of its elements."`
	ShortfallMS    int    `json:"shortfall_ms" doc:"Time left unfilled; a fixed element may start up to its tolerance early, so its segment is short only beyond that."`
	OverflowMS     int    `json:"overflow_ms" doc:"How far the last segment runs past the hour; the crossing track is cut and faded out."`
	ElementIndexes []int  `json:"element_indexes" nullable:"false" doc:"Indexes into elements, in play order: start, flow, end elements."`
	HolderIndex    int    `json:"holder_index" doc:"The element whose filler_mode fills this segment: its last element, else the fixed element that opens it, else the one that closes it."`
	FillerMode     string `json:"filler_mode" enum:"none,ask,random,playlist,artist,pool" doc:"The holder's filler mode."`
	ClosedByIndex  *int   `json:"closed_by_index,omitempty" doc:"The fixed element that ends the segment; absent for the last segment."`
	FixedStartMS   *int   `json:"fixed_start_ms,omitempty" doc:"Where the closing fixed element starts, as close to its minute as its tolerance allows."`
}

// ClockLayout is the hour built on nominal durations.
type ClockLayout struct {
	ShortfallMS int            `json:"shortfall_ms" doc:"Sum of the segments' shortfalls."`
	Segments    []ClockSegment `json:"segments" nullable:"false"`
}

// Clock is a 60-minute template with its layout.
type Clock struct {
	ID                    uint64         `json:"id"`
	Name                  string         `json:"name"`
	Status                string         `json:"status" enum:"complete,incomplete" doc:"incomplete: a short segment has no filler mode."`
	DefaultFillerMode     *string        `json:"default_filler_mode,omitempty" enum:"random,playlist,artist" doc:"Used when the pool is empty or a reminder expires; absent: the global setting filler.fallback."`
	DefaultFillerSourceID *uint64        `json:"default_filler_source_id,omitempty"`
	Elements              []ClockElement `json:"elements" nullable:"false" doc:"In position order."`
	Layout                ClockLayout    `json:"layout"`
	UpdatedAt             time.Time      `json:"updated_at"`
}

// ClockSummary is a clock in the list.
type ClockSummary struct {
	ID           uint64    `json:"id"`
	Name         string    `json:"name"`
	Status       string    `json:"status" enum:"complete,incomplete"`
	ElementCount int       `json:"element_count"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ClockList is every clock in name order.
type ClockList struct {
	Items []ClockSummary `json:"items" nullable:"false"`
}

// ClockElementBody is an element of a clock save. Its position is its
// index in elements.
type ClockElementBody struct {
	ID                  *uint64  `json:"id,omitempty" minimum:"1" doc:"An existing element of the clock; leave out for a new one. A new fixed element whose window overlaps an existing one replaces it."`
	Placement           string   `json:"placement,omitempty" enum:"flow,start,end,fixed" default:"flow"`
	FixedMinute         *int     `json:"fixed_minute,omitempty" minimum:"0" maximum:"59" doc:"Required for a fixed element only."`
	FixedToleranceMS    *int     `json:"fixed_tolerance_ms,omitempty" minimum:"0" maximum:"600000" doc:"Default: setting rotation.default_fixed_tolerance_ms."`
	NominalDurationMS   int      `json:"nominal_duration_ms" minimum:"1" maximum:"3600000"`
	DurationToleranceMS *int     `json:"duration_tolerance_ms,omitempty" minimum:"0" maximum:"600000" doc:"Default: setting rotation.default_duration_tolerance_ms."`
	FillerMode          string   `json:"filler_mode,omitempty" enum:"none,ask,random,playlist,artist,pool" default:"none"`
	FillerSourceID      *uint64  `json:"filler_source_id,omitempty" minimum:"1" doc:"Playlist ID for playlist, artist ID for artist; absent otherwise."`
	PlaylistIDs         []uint64 `json:"playlist_ids" minItems:"1" maxItems:"100" uniqueItems:"true" doc:"One or more playlists of any type."`
}

// ClockBody is the body of POST /api/clocks and PUT /api/clocks/{id}. PUT
// replaces the clock: elements left out are removed.
type ClockBody struct {
	Name                  string             `json:"name" minLength:"1" maxLength:"200"`
	DefaultFillerMode     *string            `json:"default_filler_mode,omitempty" nullable:"true" enum:"random,playlist,artist" doc:"Leave out for the global setting filler.fallback."`
	DefaultFillerSourceID *uint64            `json:"default_filler_source_id,omitempty" nullable:"true" minimum:"1"`
	Elements              []ClockElementBody `json:"elements" minItems:"1" maxItems:"200"`
}

// WeekdayHour is an hour of the week, station local.
type WeekdayHour struct {
	Weekday int `json:"weekday" doc:"1 = Monday ... 7 = Sunday."`
	Hour    int `json:"hour"`
}

// ClockWarning is a problem the dry check found; it does not block the
// save. The panel translates by code.
type ClockWarning struct {
	Code         string        `json:"code" enum:"element_no_tracks,element_no_fitting_track,element_few_tracks,element_blocked_by_play_rules,segment_overflow"`
	Message      string        `json:"message" doc:"English message for developers."`
	ElementIndex *int          `json:"element_index,omitempty"`
	SegmentIndex *int          `json:"segment_index,omitempty"`
	Hours        []WeekdayHour `json:"hours,omitempty" nullable:"false" doc:"The grid hours in which play rules block every fitting track."`
}

// ClockCheck is the dry check of a save (§6.0).
type ClockCheck struct {
	ShortfallMS          int            `json:"shortfall_ms"`
	RequiresFillerChoice bool           `json:"requires_filler_choice" doc:"Open the blocking filler dialog for short_segments."`
	ShortSegments        []int          `json:"short_segments" nullable:"false" doc:"Short segments without a filler mode."`
	Warnings             []ClockWarning `json:"warnings" nullable:"false"`
	ReplacedElements     []ClockElement `json:"replaced_elements" nullable:"false" doc:"Fixed elements removed because a new fixed element overlaps them."`
}

// ClockSaveResult is the saved clock with its dry check.
type ClockSaveResult struct {
	Clock Clock      `json:"clock"`
	Check ClockCheck `json:"check"`
}

// SegmentFillerChoice is the filler chosen for one segment.
type SegmentFillerChoice struct {
	Segment        int     `json:"segment" minimum:"0" doc:"Index of the segment."`
	FillerMode     string  `json:"filler_mode" enum:"ask,random,playlist,artist,pool"`
	FillerSourceID *uint64 `json:"filler_source_id,omitempty" minimum:"1" doc:"Playlist ID for playlist, artist ID for artist."`
}

// SaveWithFillerBody is the body of POST /api/clocks/{id}/save-with-filler.
type SaveWithFillerBody struct {
	Segments     []SegmentFillerChoice `json:"segments" minItems:"1" maxItems:"100" doc:"A choice for every short segment; segments filled by the same element need the same choice."`
	PoolTrackIDs []uint64              `json:"pool_track_ids,omitempty" nullable:"false" uniqueItems:"true" maxItems:"1000" doc:"Replaces the unused tracks of the clock's filler pool, in this order. Leave out to keep the pool."`
}

// FillerPoolEntry is a track of a clock's filler pool.
type FillerPoolEntry struct {
	TrackID  uint64     `json:"track_id"`
	Title    string     `json:"title"`
	Position int        `json:"position"`
	UsedAt   *time.Time `json:"used_at,omitempty" doc:"When it was taken for filler; used tracks stay as history."`
}

// FillerPool is the filler pool of a clock in position order.
type FillerPool struct {
	Items []FillerPoolEntry `json:"items" nullable:"false"`
}

// SetFillerPool is the body of PUT /api/clocks/{id}/filler-pool.
type SetFillerPool struct {
	TrackIDs []uint64 `json:"track_ids" nullable:"false" uniqueItems:"true" maxItems:"1000" doc:"Replaces the unused tracks, in this order; used ones stay."`
}
