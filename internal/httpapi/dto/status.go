package dto

import "time"

// LiveStatus answers whether the station is on air, for the update script's
// live check (deploy plan 8.5). A null field means unknown, never "no": a
// caller treats unknown as on air.
type LiveStatus struct {
	OnAir        *bool     `json:"on_air" nullable:"true" doc:"Whether a broadcast is on air. Null means unknown; treat it as on air."`
	OpenOutageID *uint64   `json:"open_outage_id" nullable:"true" doc:"ID of the open outage, if any. Null means none or unknown."`
	CurrentClock *string   `json:"current_clock" nullable:"true" doc:"Name of the clock playing this hour. Null means unknown."`
	Time         time.Time `json:"time" doc:"Server time (UTC) of this answer."`
}
