package ws

import (
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

// EventErrorAlert is the type of [ErrorAlertEvent].
const EventErrorAlert = "error.alert"

// ErrorAlertEvent carries an ERROR log entry to the panel, which shows it as
// a popup (§10.1).
type ErrorAlertEvent struct {
	Type string         `json:"type" doc:"Always error.alert."`
	Time time.Time      `json:"time" doc:"UTC time of the log entry."`
	Data ErrorAlertData `json:"data"`
}

// ErrorAlertData is the payload of [ErrorAlertEvent].
type ErrorAlertData struct {
	Component string `json:"component,omitempty" doc:"Component that logged the error."`
	Message   string `json:"message" doc:"Log message."`
	Error     string `json:"error,omitempty" doc:"Error text."`
	RequestID string `json:"request_id,omitempty" doc:"Request ID, for errors raised by an API request."`
}

// NewErrorAlert builds the event for an alert from the logger.
func NewErrorAlert(a logging.Alert) ErrorAlertEvent {
	return ErrorAlertEvent{
		Type: EventErrorAlert,
		Time: a.Time.UTC(),
		Data: ErrorAlertData{
			Component: a.Component,
			Message:   a.Message,
			Error:     a.Error,
			RequestID: a.RequestID,
		},
	}
}

// OperationCompletedEvent is the terminal event of a successful operation
// (§6.5). Its type is <kind>.completed.
type OperationCompletedEvent struct {
	Type        string                 `json:"type" doc:"<kind>.completed"`
	Time        time.Time              `json:"time" doc:"UTC time the operation finished."`
	OperationID uint64                 `json:"operation_id"`
	Data        OperationCompletedData `json:"data"`
}

// OperationCompletedData is the payload of [OperationCompletedEvent].
type OperationCompletedData struct {
	Kind    string `json:"kind" doc:"Operation kind."`
	Summary string `json:"summary" doc:"Short result summary."`
}

// OperationFailedEvent is the terminal event of a failed operation (§6.5).
// Its type is <kind>.failed.
type OperationFailedEvent struct {
	Type        string              `json:"type" doc:"<kind>.failed"`
	Time        time.Time           `json:"time" doc:"UTC time the operation failed."`
	OperationID uint64              `json:"operation_id"`
	Data        OperationFailedData `json:"data"`
}

// OperationFailedData is the payload of [OperationFailedEvent].
type OperationFailedData struct {
	Kind      string `json:"kind" doc:"Operation kind."`
	Component string `json:"component" doc:"Component that failed (log component)."`
	Error     string `json:"error" doc:"Short error message."`
	// Reason is set for refusals the panel explains by code.
	Reason *string `json:"reason,omitempty" enum:"insert_does_not_fit,insert_blocked" doc:"Stable reason of a refused one-time insert (hour_insert, reschedule): it does not fit its window or the hour, or the track breaks a play rule at its start."`
}

// OperationProgressEvent reports the progress of a long operation. It is
// optional; the terminal event always follows. Its type is <kind>.progress.
type OperationProgressEvent struct {
	Type        string                `json:"type" doc:"<kind>.progress"`
	Time        time.Time             `json:"time"`
	OperationID uint64                `json:"operation_id"`
	Data        OperationProgressData `json:"data"`
}

// OperationProgressData is the payload of [OperationProgressEvent].
type OperationProgressData struct {
	Kind    string `json:"kind" doc:"Operation kind."`
	Done    int    `json:"done" doc:"Units of work done."`
	Total   int    `json:"total" doc:"Units of work in total; 0 if unknown."`
	Message string `json:"message,omitempty" doc:"For example: 12 of 40 files analysed."`
}

// EventTrackPlanWarning is the type of [TrackPlanWarningEvent].
const EventTrackPlanWarning = "track_plan.warning"

// TrackPlanWarningEvent reports the warnings of one built hour of the track
// plan: skipped inserts and rule slots, emergency picks, dropped one-time
// inserts (§7, §7.3).
type TrackPlanWarningEvent struct {
	Type string               `json:"type" doc:"Always track_plan.warning."`
	Time time.Time            `json:"time" doc:"UTC time of the build."`
	Data TrackPlanWarningData `json:"data"`
}

// TrackPlanWarningData is the payload of [TrackPlanWarningEvent].
type TrackPlanWarningData struct {
	HourStartUTC time.Time          `json:"hour_start_utc"`
	ClockID      uint64             `json:"clock_id"`
	ClockName    string             `json:"clock_name"`
	Warnings     []TrackPlanWarning `json:"warnings" nullable:"false"`
}

// TrackPlanWarning is one warning of a build. The panel translates by code.
type TrackPlanWarning struct {
	Code          string  `json:"code" enum:"insert_replaced,insert_skipped,hour_insert_dropped,rule_slot_skipped,rule_blocked_by_play_rules,fill_general_pool,fill_emergency,gap_unfilled,reschedule_dropped"`
	Message       string  `json:"message" doc:"English message for developers."`
	PlaylistIndex *int    `json:"playlist_index,omitempty" doc:"Index into the clock's playlists."`
	InsertIndex   *int    `json:"insert_index,omitempty" doc:"Index into the clock's inserts."`
	TrackID       *uint64 `json:"track_id,omitempty"`
	HourInsertID  *uint64 `json:"hour_insert_id,omitempty" doc:"The one-time insert the warning is about."`
}

// EventNowPlaying is the type of [NowPlayingEvent].
const EventNowPlaying = "playout.now_playing"

// NowPlayingEvent reports the track that has just started on air (§6). The
// panel computes the progress from started_at itself.
type NowPlayingEvent struct {
	Type string         `json:"type" doc:"Always playout.now_playing."`
	Time time.Time      `json:"time"`
	Data NowPlayingData `json:"data"`
}

// NowPlayingData is the payload of [NowPlayingEvent].
type NowPlayingData struct {
	ItemID     *uint64   `json:"item_id,omitempty" doc:"Plan item; absent for an emergency track."`
	TrackID    uint64    `json:"track_id"`
	Title      string    `json:"title"`
	ArtistName *string   `json:"artist_name,omitempty"`
	Source     string    `json:"source" enum:"plan,emergency"`
	StartedAt  time.Time `json:"started_at"`
	InMS       int       `json:"in_ms" minimum:"0" doc:"In point in the file (the cue in), in ms; the track plays out_ms - in_ms."`
	OutMS      *int      `json:"out_ms,omitempty" doc:"Out point in the file: planned (plan items), or the cue out or the duration (emergency tracks), in ms."`
	Truncated  bool      `json:"truncated" doc:"The plan cuts the track at out_ms with a fade-out."`
}

// EventTrackEnded is the type of [TrackEndedEvent].
const EventTrackEnded = "playout.track_ended"

// TrackEndedEvent reports that a track has left the air and its end is in
// the play log (docs/phase-17-plan.md §9). It is sent after the write, so
// status is final; a repeated end of the same item sends nothing. The panel
// clears its now playing unless a newer track has started, and reloads the
// air window.
type TrackEndedEvent struct {
	Type string         `json:"type" doc:"Always playout.track_ended."`
	Time time.Time      `json:"time"`
	Data TrackEndedData `json:"data"`
}

// TrackEndedData is the payload of [TrackEndedEvent].
type TrackEndedData struct {
	ItemID           *uint64   `json:"item_id,omitempty" doc:"Plan item; absent for an emergency track."`
	TrackID          uint64    `json:"track_id"`
	Source           string    `json:"source" enum:"plan,emergency"`
	Status           string    `json:"status" enum:"aired,truncated" doc:"The play log status: truncated when it played less than planned (cut by the plan, the hour switch, or a skip)."`
	StartedAt        time.Time `json:"started_at"`
	EndedAt          time.Time `json:"ended_at" doc:"When Liquidsoap reported the end."`
	ActualDurationMS int       `json:"actual_duration_ms" doc:"How long it played."`
}

// EventLiquidsoapState is the type of [LiquidsoapStateEvent].
const EventLiquidsoapState = "liquidsoap.state"

// LiquidsoapStateEvent reports a change of the Liquidsoap control link
// (§6, §7.1).
type LiquidsoapStateEvent struct {
	Type string              `json:"type" doc:"Always liquidsoap.state."`
	Time time.Time           `json:"time"`
	Data LiquidsoapStateData `json:"data"`
}

// LiquidsoapStateData is the payload of [LiquidsoapStateEvent].
type LiquidsoapStateData struct {
	Connected bool `json:"connected" doc:"The backend reaches Liquidsoap's control interface."`
}

// EventIcecastState is the type of [IcecastStateEvent].
const EventIcecastState = "icecast.state"

// Icecast output states (§6.5).
const (
	IcecastConnected    = "connected"
	IcecastDisconnected = "disconnected"
)

// IcecastStateEvent reports the connection state of the Icecast output, as
// Liquidsoap reports it (§6.5, §8.3): the raw report. The panel shows
// [BroadcastStateEvent].
type IcecastStateEvent struct {
	Type string           `json:"type" doc:"Always icecast.state."`
	Time time.Time        `json:"time"`
	Data IcecastStateData `json:"data"`
}

// IcecastStateData is the payload of [IcecastStateEvent].
type IcecastStateData struct {
	State string `json:"state" enum:"connected,disconnected"`
}

// EventBroadcastState is the type of [BroadcastStateEvent].
const EventBroadcastState = "broadcast.state"

// BroadcastStateEvent reports a change of the derived broadcast state:
// whether the stream reaches Icecast, and since when (§6.5, §7.1). It is
// sent only when the state changes; a panel reads GET /api/broadcast/status
// when its event connection opens.
type BroadcastStateEvent struct {
	Type string             `json:"type" doc:"Always broadcast.state."`
	Time time.Time          `json:"time"`
	Data BroadcastStateData `json:"data"`
}

// BroadcastStateData is the payload of [BroadcastStateEvent] and the body of
// the broadcast status.
type BroadcastStateData struct {
	State       string     `json:"state" enum:"connected,reconnecting,unreachable,unknown" doc:"connected: Liquidsoap streams to Icecast and no outage is open; reconnecting: Liquidsoap lost its Icecast output and no outage is open yet; unreachable: a broadcast outage is open; unknown: no evidence yet after a start of the backend, or no heartbeat from Liquidsoap."`
	Since       *time.Time `json:"since" nullable:"true" doc:"When the state last changed (UTC); kept across restarts of the backend. Null while unknown."`
	Reason      *string    `json:"reason,omitempty" doc:"The open outage's reason or component (unreachable only)."`
	OutageID    *uint64    `json:"outage_id,omitempty" doc:"The open outage (unreachable only)."`
	MountOK     *bool      `json:"mount_ok" nullable:"true" doc:"Result of the last Icecast mount check; null before the first."`
	LastCheckAt *time.Time `json:"last_check_at" nullable:"true" doc:"When the mount was last checked; null before the first check."`
}

// EventOutageStarted is the type of [OutageStartedEvent].
const EventOutageStarted = "broadcast_outage.started"

// EventOutageEnded is the type of [OutageEndedEvent].
const EventOutageEnded = "broadcast_outage.ended"

// OutageStartedEvent reports an outage that has just opened (§6.5, §7.1).
// The panel shows a banner until the outage ends.
type OutageStartedEvent struct {
	Type string            `json:"type" doc:"Always broadcast_outage.started."`
	Time time.Time         `json:"time"`
	Data OutageStartedData `json:"data"`
}

// OutageStartedData is the payload of [OutageStartedEvent].
type OutageStartedData struct {
	OutageID   uint64    `json:"outage_id"`
	StartedAt  time.Time `json:"started_at"`
	Component  string    `json:"component" enum:"icecast,liquidsoap,backend,network"`
	DetectedBy string    `json:"detected_by" enum:"icecast,liquidsoap_callback,liquidsoap_control,backend"`
	Reason     *string   `json:"reason,omitempty"`
}

// OutageEndedEvent reports an outage that has closed, after its missed
// items were marked (§6.5, §7.1).
type OutageEndedEvent struct {
	Type string          `json:"type" doc:"Always broadcast_outage.ended."`
	Time time.Time       `json:"time"`
	Data OutageEndedData `json:"data"`
}

// OutageEndedData is the payload of [OutageEndedEvent].
type OutageEndedData struct {
	OutageID  uint64    `json:"outage_id"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Component string    `json:"component" enum:"icecast,liquidsoap,backend,network"`
	// MissedCount is absent when the marking failed; it is retried, and the
	// missed_items operation events report the result.
	MissedCount *int `json:"missed_count,omitempty" doc:"Items marked missed; absent when the marking failed (it is retried)."`
}

// EventLiveState is the type of [LiveStateEvent].
const EventLiveState = "live.state"

// LiveStateEvent reports a change of the live voice input (§8.4).
type LiveStateEvent struct {
	Type string        `json:"type" doc:"Always live.state."`
	Time time.Time     `json:"time"`
	Data LiveStateData `json:"data"`
}

// LiveStateData is the payload of [LiveStateEvent].
type LiveStateData struct {
	State      string     `json:"state" enum:"off,connecting,on_air,reconnecting" doc:"connecting: waiting for audio; on_air: audio reaches Liquidsoap; reconnecting: audio arrives but Liquidsoap's live input is not reachable."`
	SessionID  *uint64    `json:"session_id,omitempty" doc:"Absent when off."`
	OnAirSince *time.Time `json:"on_air_since,omitempty" doc:"When audio first reached Liquidsoap in this session."`
	Reason     *string    `json:"reason,omitempty" enum:"stopped,replaced,no_audio,dropout,shutdown" doc:"Why the session ended; only with state off."`
}
