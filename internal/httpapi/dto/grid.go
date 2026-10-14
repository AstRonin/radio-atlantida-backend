package dto

import "time"

// GridClockRef is the clock of a grid slot.
type GridClockRef struct {
	ID     uint64 `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status" enum:"complete,incomplete"`
}

// GridSlot assigns a clock to a weekday and hour for a range of dates, in
// station local time (§4, §10.2).
type GridSlot struct {
	ID        uint64       `json:"id"`
	Weekday   int          `json:"weekday" doc:"1 = Monday ... 7 = Sunday."`
	Hour      int          `json:"hour"`
	Clock     GridClockRef `json:"clock"`
	ValidFrom string       `json:"valid_from" format:"date"`
	ValidTo   *string      `json:"valid_to,omitempty" format:"date" doc:"Absent for an open end."`
}

// GridSlotList is a list of slots in weekday, hour, valid_from order.
type GridSlotList struct {
	Items []GridSlot `json:"items" nullable:"false"`
}

// GridAssignBody is the body of POST /api/grid/assign.
type GridAssignBody struct {
	ClockID   uint64  `json:"clock_id" minimum:"1"`
	Weekdays  []int   `json:"weekdays" minItems:"1" maxItems:"7" uniqueItems:"true" minimum:"1" maximum:"7" doc:"1 = Monday ... 7 = Sunday."`
	Hours     []int   `json:"hours" minItems:"1" maxItems:"24" uniqueItems:"true" minimum:"0" maximum:"23" doc:"Station local hours."`
	ValidFrom string  `json:"valid_from" format:"date" doc:"Station local date, from today to the grid horizon (setting rotation.grid_horizon_days)."`
	ValidTo   *string `json:"valid_to,omitempty" nullable:"true" format:"date" doc:"Inclusive; leave out for an open end."`
}

// GridUnassignBody is the body of POST /api/grid/unassign.
type GridUnassignBody struct {
	Weekdays  []int   `json:"weekdays" minItems:"1" maxItems:"7" uniqueItems:"true" minimum:"1" maximum:"7"`
	Hours     []int   `json:"hours" minItems:"1" maxItems:"24" uniqueItems:"true" minimum:"0" maximum:"23"`
	ValidFrom string  `json:"valid_from" format:"date"`
	ValidTo   *string `json:"valid_to,omitempty" nullable:"true" format:"date"`
}

// GridReplacedSlot is an existing slot changed by an assignment.
type GridReplacedSlot struct {
	Slot      GridSlot   `json:"slot" doc:"The slot as it was."`
	Change    string     `json:"change" enum:"trimmed,split,deleted"`
	Remaining []GridSlot `json:"remaining" nullable:"false" doc:"What is left of it; split leaves two slots, the later one new."`
}

// GridAssignResult is the outcome of an assignment.
type GridAssignResult struct {
	Created  []GridSlot         `json:"created" nullable:"false"`
	Replaced []GridReplacedSlot `json:"replaced" nullable:"false"`
}

// GridHour is one assigned hour of the grid.
type GridHour struct {
	Date     string       `json:"date" format:"date" doc:"Station local date."`
	Weekday  int          `json:"weekday"`
	Hour     int          `json:"hour" doc:"Station local hour."`
	StartUTC time.Time    `json:"start_utc"`
	SlotID   uint64       `json:"slot_id"`
	Clock    GridClockRef `json:"clock"`
}

// GridSchedule is the grid resolved to hours, in time order. Hours without
// a clock are left out.
type GridSchedule struct {
	Items []GridHour `json:"items" nullable:"false"`
}
