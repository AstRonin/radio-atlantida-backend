// Package rotation builds the track plan (§7 of the design): the concrete
// tracks of every grid hour within the plan horizon (24 to 48 hours), kept
// in scheduled_items.
//
// Build is a pure function of its input and a seed: the clock (its
// playlists with their effective placement rules and its timed inserts),
// the one-time inserts of the hour, the library, and the history of aired
// and planned plays. It places the inserts as anchors, reserves the slots
// of the rule playlists, and fills the rest from the fill playlists, every
// track passing the play rules of the track and its playlists (never
// relaxed). The last track before a hard point is the best fit to the time
// left; when none lands in the window, the crossing track is cut at the
// target and faded out. Audio is never altered.
//
// The Service keeps the plan in step with the grid (Sync): it builds missing
// and stale hours and clears hours the grid no longer assigns. One-time
// inserts (hour_inserts: a track the manager adds to an hour, a rescheduled
// missed item) survive every rebuild of their hour. Every hour is written
// in its own short transaction under an advisory lock. The plan job
// (internal/jobs) and the operations started by clock, grid, and insert
// changes all go through it.
//
// Persistence goes through internal/repository/rotation.
package rotation

import (
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
)

// Track is a library track that can be planned: it has a duration.
type Track struct {
	ID       uint64
	ArtistID *uint64
	// DurationMS is the length on air: from the cue in point to the cue
	// out point (or the end of the file). The fit uses it.
	DurationMS int
	// CueInMS is where the on-air part starts in the file.
	CueInMS int
	// PlaylistIDs are the playlists that hold the track, in ID order.
	PlaylistIDs []uint64
	// RuleIDs are the play rules of the track and of all its playlists.
	RuleIDs []uint64
	// Title and ArtistName are shown in the sample hour of a clock save.
	Title      string
	ArtistName *string
}

// LibraryScope selects the tracks a snapshot holds: the tracks of Playlists
// and the tracks named in Tracks (one-time inserts, a missed track). A build
// reads only the tracks of the playlists its clocks and inserts use, so a
// snapshot of those playlists gives the same plan as the whole library.
type LibraryScope struct {
	// All reads every track with a duration (the unfiltered library).
	All       bool
	Playlists []uint64
	Tracks    []uint64
}

// Library is the snapshot of the library a build reads.
type Library struct {
	// Tracks in ID order.
	Tracks []Track
	Rules  map[uint64]playrules.RuleInput
	// Scope is what the snapshot was read with; the service sets it.
	Scope LibraryScope
	// digests caches clockDigest by clock version; cleared when tracks are
	// added to the snapshot.
	digests map[digestKey]string
}

// track returns the track with the ID.
func (l *Library) track(id uint64) (Track, bool) {
	i, found := slices.BinarySearchFunc(l.Tracks, id, func(t Track, id uint64) int {
		switch {
		case t.ID < id:
			return -1
		case t.ID > id:
			return 1
		}
		return 0
	})
	if !found {
		return Track{}, false
	}
	return l.Tracks[i], true
}

// maxRuleInterval is the longest track or artist interval of the play
// rules: the history a build reads on each side of the hour.
func (l *Library) maxRuleInterval() time.Duration {
	longest := 0
	for _, r := range l.Rules {
		if r.MinIntervalMinutes != nil {
			longest = max(longest, *r.MinIntervalMinutes)
		}
		if r.ArtistMinIntervalMinutes != nil {
			longest = max(longest, *r.ArtistMinIntervalMinutes)
		}
	}
	return time.Duration(longest) * time.Minute
}

// Play is a start of a track that counts for the play rules: aired or
// truncated in the play log, planned or queued in the plan. Missed items do
// not count (§7).
type Play struct {
	TrackID  uint64
	ArtistID *uint64
	Start    time.Time
	// Planned is true for a planned item of the plan (scheduled_items with
	// status planned); a rebuild of its hour replaces it. Aired, truncated
	// and queued plays are not.
	Planned bool
}

// Source says why an item is in the plan (scheduled_items.source).
type Source string

// Sources.
const (
	// SourceFill: a track of a fill playlist.
	SourceFill Source = "fill"
	// SourceRule: a track of a playlist with a placement rule other than
	// fill.
	SourceRule Source = "rule"
	// SourceInsert: a timed insert of the clock template.
	SourceInsert Source = "insert"
	// SourceHourInsert: a one-time insert the manager added to the hour.
	SourceHourInsert Source = "hour_insert"
	// SourceReschedule: a missed item placed into this hour by the manager
	// (§7.1).
	SourceReschedule Source = "reschedule"
	// SourceGeneral: no fill track was allowed; taken from the whole
	// library (emergency, §7).
	SourceGeneral Source = "general"
)

// Placement says where a one-time insert goes in its hour.
type Placement string

// Placements of a one-time insert.
const (
	PlacementStart Placement = "start"
	PlacementEnd   Placement = "end"
	PlacementFixed Placement = "fixed"
)

// Insert is a one-time insert of one hour (hour_inserts): a track the
// manager added, or a missed item rescheduled into it (§7.1, §7.2).
type Insert struct {
	ID        uint64
	HourStart time.Time
	TrackID   uint64
	// MissedID is the missed play log row; nil for a track the manager
	// added, and when retention deleted the row.
	MissedID  *uint64
	Placement Placement
	// OffsetMS is the target start of a fixed insert, whole seconds.
	OffsetMS    *int
	ToleranceMS int
}

// Item is a planned track.
type Item struct {
	TrackID uint64
	// OffsetMS is the planned start, from the start of the hour.
	OffsetMS int
	// InMS and OutMS are the in and out points from the start of the file
	// (planned_in_ms, planned_out_ms); the item plays OutMS - InMS.
	InMS      int
	OutMS     int
	Truncated bool
	Source    Source
	// PlaylistID is the fill or rule playlist the track was drawn from.
	PlaylistID *uint64
	// ClockInsertID and InsertIndex name the template insert of an insert
	// item (its ID and its index in the clock).
	ClockInsertID *uint64
	InsertIndex   *int
	// HourInsertID is the one-time insert of an hour_insert or reschedule
	// item.
	HourInsertID *uint64
	// RescheduleOfID is the missed play log row a reschedule item airs
	// again.
	RescheduleOfID *uint64
}

// WarningCode is the stable code of a build warning; the panel translates by
// code.
type WarningCode string

// Warning codes (docs/phase-23-plan.md §3.3).
const (
	// WarnInsertReplaced: a one-time insert at the offset of a template
	// insert replaces it in this hour.
	WarnInsertReplaced WarningCode = "insert_replaced"
	// WarnInsertSkipped: a template insert has no allowed track, or the
	// chain before it runs past its window.
	WarnInsertSkipped WarningCode = "insert_skipped"
	// WarnHourInsertDropped: a one-time insert of the manager can no
	// longer be placed whole; it is removed.
	WarnHourInsertDropped WarningCode = "hour_insert_dropped"
	// WarnRuleSlotSkipped: a rule track could not be placed in its window,
	// or its playlist has no track that can be planned.
	WarnRuleSlotSkipped WarningCode = "rule_slot_skipped"
	// WarnRuleBlocked: the play rules block every track of a rule playlist
	// at its slot (§7.3).
	WarnRuleBlocked WarningCode = "rule_blocked_by_play_rules"
	// WarnGeneralPool: no fill track was allowed; the track was chosen from
	// the whole library.
	WarnGeneralPool WarningCode = "fill_general_pool"
	// WarnEmergency: no track passes the rules; the gap is left for the
	// emergency source (§8.6).
	WarnEmergency WarningCode = "fill_emergency"
	// WarnGapUnfilled: a gap no track can serve (it would play less than
	// the shortest track on air).
	WarnGapUnfilled WarningCode = "gap_unfilled"
	// WarnRescheduleDropped: a rescheduled item no longer fits its hour or
	// breaks a rule there; it is removed and its missed item is open again.
	WarnRescheduleDropped WarningCode = "reschedule_dropped"
)

// WarningCodes lists every warning code.
func WarningCodes() []WarningCode {
	return []WarningCode{
		WarnInsertReplaced, WarnInsertSkipped, WarnHourInsertDropped, WarnRuleSlotSkipped, WarnRuleBlocked,
		WarnGeneralPool, WarnEmergency, WarnGapUnfilled, WarnRescheduleDropped,
	}
}

// Warning is a problem of a build. PlaylistIndex and InsertIndex are
// indexes into the clock.
type Warning struct {
	Code          WarningCode
	Message       string
	PlaylistIndex *int
	InsertIndex   *int
	TrackID       *uint64
	HourInsertID  *uint64
}

// RefusalReason is why a one-time insert cannot be placed whole; the
// failure reason of an hour_insert or reschedule operation.
type RefusalReason string

// Refusal reasons.
const (
	// ReasonDoesNotFit: the insert does not fit its window or the hour.
	ReasonDoesNotFit RefusalReason = "insert_does_not_fit"
	// ReasonBlocked: the track breaks a play rule at its start.
	ReasonBlocked RefusalReason = "insert_blocked"
)

// RefusalReasons lists every refusal reason.
func RefusalReasons() []RefusalReason { return []RefusalReason{ReasonDoesNotFit, ReasonBlocked} }

// Refusal is a one-time insert a build could not place.
type Refusal struct {
	Reason  RefusalReason
	Message string
}

// Settings are the settings of a build.
type Settings struct {
	// MinGapMS: a smaller gap between anchors gets no music.
	MinGapMS int
}

// HourInput is everything a build reads.
type HourInput struct {
	// Start is the UTC start of the hour.
	Start time.Time
	// Location is the station timezone, for play rule windows and the
	// per-hour limit.
	Location *time.Location
	Clock    clocks.Clock
	// HourInserts are the one-time inserts of the hour, in ID order.
	HourInserts []Insert
	Library     *Library
	// History are the plays around the hour, without the planned items of
	// the hour itself.
	History  []Play
	Settings Settings
	Seed     uint64
}

// HourPlan is the result of a build.
type HourPlan struct {
	// Items in time order.
	Items    []Item
	Warnings []Warning
	// Refused are the one-time inserts that could not be placed whole, by
	// insert ID.
	Refused map[uint64]Refusal
}
