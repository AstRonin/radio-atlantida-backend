package dto

import "time"

// BroadcastStatus is whether the stream reaches Icecast, and since when
// (docs/phase-16-plan.md §4): the payload of the broadcast.state event
// plus the server time. The panel reads it when its event connection opens.
type BroadcastStatus struct {
	State       string     `json:"state" enum:"connected,reconnecting,unreachable,unknown" doc:"connected: Liquidsoap streams to Icecast and no outage is open; reconnecting: Liquidsoap lost its Icecast output and no outage is open yet; unreachable: a broadcast outage is open; unknown: no evidence yet after a start of the backend, or no heartbeat from Liquidsoap."`
	Since       *time.Time `json:"since" nullable:"true" doc:"When the state last changed (UTC); kept across restarts of the backend. Null while unknown."`
	Reason      *string    `json:"reason,omitempty" doc:"The open outage's reason or component (unreachable only)."`
	OutageID    *uint64    `json:"outage_id,omitempty" doc:"The open outage (unreachable only)."`
	MountOK     *bool      `json:"mount_ok" nullable:"true" doc:"Result of the last Icecast mount check; null before the first."`
	LastCheckAt *time.Time `json:"last_check_at" nullable:"true" doc:"When the mount was last checked; null before the first check."`
	Time        time.Time  `json:"time" doc:"Server time (UTC) of this answer, so the panel can correct its clock for the timer."`
}
