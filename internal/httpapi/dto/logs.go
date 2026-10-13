package dto

import "time"

// LogFile is one daily log file (§10.1).
type LogFile struct {
	Name      string `json:"name" example:"app-2026-10-13.log"`
	Date      string `json:"date" format:"date" doc:"UTC date of the file."`
	SizeBytes int64  `json:"size_bytes"`
}

// LogFileList lists the log files, newest first.
type LogFileList struct {
	Items []LogFile `json:"items" nullable:"false"`
}

// LogEntry is one log entry. Times are UTC; the panel converts them to the
// selected timezone.
type LogEntry struct {
	Time      time.Time      `json:"time"`
	Level     string         `json:"level" example:"ERROR"`
	Message   string         `json:"message"`
	Component string         `json:"component,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Error     string         `json:"error,omitempty"`
	Attrs     map[string]any `json:"attrs,omitempty" doc:"Other fields of the entry."`
}

// LogEntryPage is a page of entries, newest first.
type LogEntryPage struct {
	Items []LogEntry `json:"items" nullable:"false"`
	Total int        `json:"total" doc:"Number of entries in the file that match the filters."`
}
