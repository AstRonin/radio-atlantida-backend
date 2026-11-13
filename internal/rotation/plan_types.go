package rotation

import (
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// InsertError is the failure of an hour_insert or reschedule operation
// whose insert cannot be placed whole in its hour. Nothing was changed.
type InsertError struct {
	Reason  RefusalReason
	Message string
}

func (e *InsertError) Error() string { return "the insert cannot be placed in that hour: " + e.Message }

// FailureReason is the stable reason sent with the operation's failure
// event.
func (e *InsertError) FailureReason() string { return string(e.Reason) }

// PlanHour records a built hour (plan_hours).
type PlanHour struct {
	Start          time.Time
	ClockID        uint64
	ClockUpdatedAt time.Time
	GridSlotID     *uint64
	Seed           int64
	BuiltAt        time.Time
	// InputsHash fingerprints what the hour was built from (inputsHash); empty
	// for an hour built before migration 0016.
	InputsHash string
}

// current reports whether the hour was built from this version of c.
func (h PlanHour) current(c clocks.Clock) bool {
	return h.ClockID == c.ID && h.ClockUpdatedAt.Equal(c.UpdatedAt)
}

// PlannedPlaylist is the playlist a planned item was drawn from, with the code
// of its type (music, jingle, news, segment, or a custom type).
type PlannedPlaylist struct {
	ID   uint64
	Name string
	Type string
}

// PlannedItem is an item of the track plan, for the panel.
type PlannedItem struct {
	// Playlist is the fill or rule playlist of the item; nil for a general item
	// and for an item built before its playlist was stored (migration 0019).
	Playlist   *PlannedPlaylist
	ID         uint64
	TrackID    uint64
	Title      string
	ArtistName *string
	DurationMS int
	Start      time.Time
	// InMS and OutMS are the in and out points from the start of the file.
	InMS      int
	OutMS     int
	Truncated bool
	Source    string
	Status    string
	// InsertID is the template insert of an insert item; HourInsertID the
	// one-time insert of an hour_insert or reschedule item.
	InsertID     *uint64
	HourInsertID *uint64
}

// planItemPlanned is the status of a planned item that is not yet queued in
// Liquidsoap (scheduled_items.status).
const planItemPlanned = "planned"

// PlanItemRef is a stored planned item, as a pin reads it: its hour and the
// placement it was built with.
type PlanItemRef struct {
	ID        uint64
	HourStart time.Time
	TrackID   uint64
	Source    Source
	Status    string
	// PlaylistID is the fill or rule playlist of the item; nil for an item
	// built before its playlist was stored (migration 0019).
	PlaylistID *uint64
	// OffsetMS is the planned start, from the start of the hour.
	OffsetMS int
}

// PlannedHour is a built hour with its items and its one-time inserts.
type PlannedHour struct {
	PlanHour
	ClockName string
	Items     []PlannedItem
	Inserts   []Insert
}

// Range is a span of time, [From, To).
type Range struct {
	From, To time.Time
}

func (r Range) contains(t time.Time) bool { return !t.Before(r.From) && t.Before(r.To) }

func (r SyncResult) String() string {
	return fmt.Sprintf("built %d hours, cleared %d, %d warnings", r.Built, r.Cleared, r.Warnings)
}

// SyncResult counts what a sync did.
type SyncResult struct {
	Built, Cleared, Warnings int
	// Skipped counts the grid hours that were current or whose clock no
	// longer exists; Items is the number of items of the built hours.
	Skipped, Items int
	// ByCode counts the warnings of the built hours by code.
	ByCode map[WarningCode]int
}
