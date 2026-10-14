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
// plan: skipped elements, emergency picks, filler problems (§7, §7.3).
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
	Code         string  `json:"code" enum:"element_blocked_by_play_rules,element_tolerance_relaxed,element_general_pool,element_emergency,pool_track_refused,pool_empty,filler_waiting,gap_unfilled"`
	Message      string  `json:"message" doc:"English message for developers."`
	ElementIndex *int    `json:"element_index,omitempty" doc:"Index into the clock's elements."`
	SegmentIndex *int    `json:"segment_index,omitempty"`
	TrackID      *uint64 `json:"track_id,omitempty"`
}

// EventFillerRequestCreated is the type of [FillerRequestCreatedEvent].
const EventFillerRequestCreated = "filler_request.created"

// FillerRequestCreatedEvent reports a new filler reminder: a short hour that
// waits for the operator, or an empty filler pool (§7).
type FillerRequestCreatedEvent struct {
	Type string                   `json:"type" doc:"Always filler_request.created."`
	Time time.Time                `json:"time"`
	Data FillerRequestCreatedData `json:"data"`
}

// FillerRequestCreatedData is the payload of [FillerRequestCreatedEvent].
type FillerRequestCreatedData struct {
	ID           uint64    `json:"id"`
	ClockID      uint64    `json:"clock_id"`
	ClockName    string    `json:"clock_name"`
	HourStartUTC time.Time `json:"hour_start_utc"`
	Reason       string    `json:"reason" enum:"short_clock,pool_empty"`
	DeadlineAt   time.Time `json:"deadline_at" doc:"Without an answer by then, the clock's default filler mode applies."`
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
	OutMS      *int      `json:"out_ms,omitempty" doc:"Planned out point (plan items) or duration (emergency tracks), in ms."`
	Truncated  bool      `json:"truncated" doc:"The plan cuts the track at out_ms with a fade-out."`
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
// Liquidsoap reports it (§6.5, §8.3).
type IcecastStateEvent struct {
	Type string           `json:"type" doc:"Always icecast.state."`
	Time time.Time        `json:"time"`
	Data IcecastStateData `json:"data"`
}

// IcecastStateData is the payload of [IcecastStateEvent].
type IcecastStateData struct {
	State string `json:"state" enum:"connected,disconnected"`
}
