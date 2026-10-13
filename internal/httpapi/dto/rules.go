package dto

import "time"

// PlayRule limits when a track may play (§7.3). Times and weekdays are in
// station local time.
type PlayRule struct {
	ID                 int64     `json:"id"`
	Name               string    `json:"name"`
	WindowFrom         *string   `json:"window_from,omitempty" doc:"Start of the allowed window, HH:MM."`
	WindowTo           *string   `json:"window_to,omitempty" doc:"End of the allowed window, HH:MM; earlier than window_from when the window crosses midnight."`
	Weekdays           []int     `json:"weekdays,omitempty" nullable:"false" doc:"Allowed days, 1 = Monday ... 7 = Sunday; absent means every day. With a window that crosses midnight, the day on which the window starts."`
	MinIntervalMinutes *int      `json:"min_interval_minutes,omitempty" doc:"Minutes between two starts of the same track."`
	MaxPerHour         *int      `json:"max_per_hour,omitempty" doc:"Starts of the same track within one clock hour."`
	Notes              string    `json:"notes"`
	TrackIDs           []int64   `json:"track_ids" nullable:"false" doc:"Tracks the rule is applied to, in ID order."`
	PlaylistIDs        []int64   `json:"playlist_ids" nullable:"false" doc:"Playlists the rule is applied to, in ID order; it applies to each of their tracks."`
	TrackCount         int       `json:"track_count"`
	PlaylistCount      int       `json:"playlist_count"`
	CreatedAt          time.Time `json:"created_at"`
}

// PlayRuleList is every play rule in name order.
type PlayRuleList struct {
	Items []PlayRule `json:"items" nullable:"false"`
}

// PlayRuleBody is the body of POST /api/play-rules and PUT
// /api/play-rules/{id}. Every limit is optional, but at least one is set;
// window_from and window_to are set together. PUT replaces the rule: a limit
// left out is removed.
type PlayRuleBody struct {
	Name               string  `json:"name" minLength:"1" maxLength:"200"`
	WindowFrom         *string `json:"window_from,omitempty" nullable:"true" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"HH:MM, station local time."`
	WindowTo           *string `json:"window_to,omitempty" nullable:"true" pattern:"^([01][0-9]|2[0-3]):[0-5][0-9]$" doc:"HH:MM; earlier than window_from for a window that crosses midnight (22:00 to 02:00)."`
	Weekdays           []int   `json:"weekdays,omitempty" nullable:"false" minItems:"1" maxItems:"7" uniqueItems:"true" minimum:"1" maximum:"7" doc:"1 = Monday ... 7 = Sunday; leave out for every day."`
	MinIntervalMinutes *int    `json:"min_interval_minutes,omitempty" nullable:"true" minimum:"1" maximum:"10080" doc:"At most 7 days."`
	MaxPerHour         *int    `json:"max_per_hour,omitempty" nullable:"true" minimum:"1" maximum:"60"`
	Notes              string  `json:"notes,omitempty" maxLength:"2000"`
}

// SetPlayRuleTargets is the body of PUT /api/play-rules/{id}/targets.
type SetPlayRuleTargets struct {
	TrackIDs    []int64 `json:"track_ids" nullable:"false" uniqueItems:"true" maxItems:"100000" doc:"Every track the rule applies to; tracks not listed lose it."`
	PlaylistIDs []int64 `json:"playlist_ids" nullable:"false" uniqueItems:"true" maxItems:"1000" doc:"Every playlist the rule applies to; playlists not listed lose it."`
}

// SeparationRule is the minimum time between two plays of the same track or
// of tracks by the same artist (§4). Rotation reads every value from here.
type SeparationRule struct {
	ID               int64  `json:"id"`
	Scope            string `json:"scope" enum:"track,artist"`
	MinIntervalHours int    `json:"min_interval_hours"`
	Notes            string `json:"notes"`
}

// SeparationRuleList is every separation rule.
type SeparationRuleList struct {
	Items []SeparationRule `json:"items" nullable:"false"`
}

// UpdateSeparationRule is the body of PUT /api/separation-rules/{id}.
type UpdateSeparationRule struct {
	MinIntervalHours int    `json:"min_interval_hours" minimum:"1" doc:"At most the play log retention (setting retention.play_log_days) in hours: separation is checked against aired history."`
	Notes            string `json:"notes,omitempty" maxLength:"2000"`
}
