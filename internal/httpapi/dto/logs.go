package dto

import "time"

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
	// NextCursor resumes the search with the next, older page. Absent on the
	// last page.
	NextCursor string `json:"next_cursor,omitempty" doc:"Pass as cursor to get the next, older page. Absent on the last page."`
	// Total is the number of entries that match over the whole range. Absent
	// when the count failed or timed out; the entries are still shown.
	Total *int64 `json:"total,omitempty" doc:"Number of entries that match over the whole range. Absent when the count failed or timed out."`
}
