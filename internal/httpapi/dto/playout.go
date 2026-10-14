package dto

import "time"

// PlayoutCallbackBody is one event from Liquidsoap: a line of its playout
// journal (§8.3, dev/liquidsoap/radio.liq). Liquidsoap sends absent
// annotations as empty strings.
type PlayoutCallbackBody struct {
	Event     string    `json:"event" enum:"start,end,output"`
	Time      time.Time `json:"time" doc:"When Liquidsoap wrote the event (UTC)."`
	Source    string    `json:"source,omitempty" enum:"plan,emergency" doc:"start and end: a plan item or an emergency track."`
	RID       string    `json:"rid,omitempty" doc:"Liquidsoap request ID."`
	ItemID    string    `json:"item_id,omitempty" pattern:"^[0-9]*$" doc:"Plan item (source plan); empty otherwise."`
	TrackID   string    `json:"track_id,omitempty" pattern:"^[0-9]*$" doc:"Emergency track (source emergency); empty otherwise."`
	URI       string    `json:"uri,omitempty" doc:"The request as pushed."`
	Filename  string    `json:"filename,omitempty" doc:"The resolved file."`
	ElapsedMS int       `json:"elapsed_ms,omitempty" minimum:"0" doc:"end: how long the track played."`
	Connected bool      `json:"connected,omitempty" doc:"output: the Icecast output is connected."`
}

// PlayoutLink is the state of the Liquidsoap control link.
type PlayoutLink struct {
	Connected bool       `json:"connected"`
	Since     *time.Time `json:"since,omitempty" doc:"When the link last changed state."`
	LastOKAt  *time.Time `json:"last_ok_at,omitempty" doc:"Last successful command."`
}

// PlayoutOutput is the Icecast output state reported by Liquidsoap.
type PlayoutOutput struct {
	State string    `json:"state" enum:"connected,disconnected"`
	Since time.Time `json:"since"`
}

// NowPlaying is the track on air.
type NowPlaying struct {
	ItemID     *uint64   `json:"item_id,omitempty" doc:"Plan item; absent for an emergency track."`
	TrackID    uint64    `json:"track_id"`
	Title      string    `json:"title"`
	ArtistName *string   `json:"artist_name,omitempty"`
	Source     string    `json:"source" enum:"plan,emergency"`
	StartedAt  time.Time `json:"started_at"`
	OutMS      *int      `json:"out_ms,omitempty" doc:"Planned out point (plan items) or duration (emergency tracks), in ms."`
	Truncated  bool      `json:"truncated"`
}

// PlayoutQueuedItem is a plan item waiting in a Liquidsoap queue.
type PlayoutQueuedItem struct {
	ID              uint64    `json:"id"`
	TrackID         uint64    `json:"track_id"`
	Title           string    `json:"title"`
	ArtistName      *string   `json:"artist_name,omitempty"`
	PlannedStartUTC time.Time `json:"planned_start_utc"`
	PlannedOutMS    int       `json:"planned_out_ms"`
	Truncated       bool      `json:"truncated"`
	Queue           string    `json:"queue" enum:"hour_0,hour_1" doc:"The Liquidsoap queue of its hour."`
}

// PlayoutStatus is the playout state.
type PlayoutStatus struct {
	ControlLink    PlayoutLink         `json:"control_link"`
	Output         *PlayoutOutput      `json:"output,omitempty" doc:"Absent until Liquidsoap reports it."`
	NowPlaying     *NowPlaying         `json:"now_playing,omitempty" doc:"Absent between tracks or when nothing is known."`
	LastCallbackAt *time.Time          `json:"last_callback_at,omitempty" doc:"When the last callback from Liquidsoap arrived."`
	MusicGain      *float64            `json:"music_gain,omitempty" doc:"The gain last set; absent before the first connect."`
	Queued         []PlayoutQueuedItem `json:"queued" nullable:"false" doc:"In planned order."`
}

// MusicGainBody sets the live music gain (§8.5).
type MusicGainBody struct {
	Gain float64 `json:"gain" minimum:"0" maximum:"1" doc:"1 normal, 0.2 under voice, 0 off."`
}

// MusicGain is the music gain that was set.
type MusicGain struct {
	Gain float64 `json:"gain"`
}
