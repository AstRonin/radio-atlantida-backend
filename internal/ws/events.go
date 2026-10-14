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
