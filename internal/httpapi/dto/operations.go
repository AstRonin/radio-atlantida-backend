package dto

import "time"

// Operation is the state of an asynchronous operation (§6.5).
type Operation struct {
	ID            uint64     `json:"id"`
	Kind          string     `json:"kind" doc:"Operation kind; its events are <kind>.completed, <kind>.failed, <kind>.progress."`
	Status        string     `json:"status" enum:"running,completed,failed"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty" doc:"Set when the operation is no longer running."`
	ErrorMessage  string     `json:"error_message,omitempty" doc:"Set for failed operations."`
	ResultSummary string     `json:"result_summary,omitempty" doc:"Set for completed operations."`
}

// OperationList is a page of operations in ID order.
type OperationList struct {
	Items []Operation `json:"items" nullable:"false"`
}
