// Package rotation builds the track plan (§7 of the design): the concrete
// tracks of every grid hour within the plan horizon (24 to 48 hours), kept
// in scheduled_items.
//
// Build is a pure function of its input and a seed: the clock and its
// layout, the library, the history of aired and planned plays, the
// separation intervals, the filler pool, and how waiting segments are
// resolved. It picks a track for every element from the union of its
// playlists, honouring separation and play rules (never relaxed) and the
// duration tolerance, fills short segments with filler, truncates the track
// that crosses a segment end, and applies the emergency steps when nothing
// fits. Audio is never altered.
//
// The Service keeps the plan in step with the grid (Sync): it builds missing
// and stale hours, clears hours the grid no longer assigns, creates filler
// reminders, and resolves them when the operator answers or the deadline
// passes. Every hour is written in its own short transaction under an
// advisory lock. The plan job (internal/jobs) and the clock_build operation
// started by clock and grid changes both call Sync.
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
	ID         uint64
	ArtistID   *uint64
	DurationMS int
	// PlaylistIDs are the playlists that hold the track, in ID order.
	PlaylistIDs []uint64
	// RuleIDs are the play rules of the track and of all its playlists.
	RuleIDs []uint64
}

func (t Track) inAny(playlistIDs []uint64) bool {
	return slices.ContainsFunc(playlistIDs, func(id uint64) bool {
		_, found := slices.BinarySearch(t.PlaylistIDs, id)
		return found
	})
}

// Library is the snapshot of the library a build reads.
type Library struct {
	// Tracks in ID order.
	Tracks []Track
	Rules  map[uint64]playrules.RuleInput
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

// maxRuleInterval is the longest minimum interval of the play rules.
func (l *Library) maxRuleInterval() time.Duration {
	longest := 0
	for _, r := range l.Rules {
		if r.MinIntervalMinutes != nil {
			longest = max(longest, *r.MinIntervalMinutes)
		}
	}
	return time.Duration(longest) * time.Minute
}

// Play is a start of a track that counts for separation and play rules:
// aired or truncated in the play log, planned or queued in the plan. Missed
// items do not count (§7).
type Play struct {
	TrackID  uint64
	ArtistID *uint64
	Start    time.Time
}

// Separation are the separation intervals (separation_rules): the longest
// rule of each scope. Zero means no separation.
type Separation struct {
	Track  time.Duration
	Artist time.Duration
}

// PoolRow is an unused row of a clock's filler pool.
type PoolRow struct {
	ID      uint64
	TrackID uint64
}

// Source says why an item is in the plan (scheduled_items.source).
type Source string

// Sources.
const (
	SourceClock          Source = "clock"
	SourceFillerPool     Source = "filler_pool"
	SourceFillerRandom   Source = "filler_random"
	SourceFillerPlaylist Source = "filler_playlist"
	SourceFillerArtist   Source = "filler_artist"
	SourceFillerAnswer   Source = "filler_answer"
	// SourceReschedule: a missed item placed into this hour by the manager
	// (§7.1).
	SourceReschedule Source = "reschedule"
)

// Item is a planned track.
type Item struct {
	TrackID uint64
	// ElementID is the element the track was planned for; nil for filler.
	ElementID *uint64
	// OffsetMS is the planned start, from the start of the hour.
	OffsetMS int
	// OutMS is the out point from the start of the track (planned_out_ms).
	OutMS     int
	Truncated bool
	Source    Source
	// PoolRowID is the pool row a filler_pool item took.
	PoolRowID *uint64
	// RescheduleOfID is the missed play log row a reschedule item airs
	// again.
	RescheduleOfID *uint64
}

// Pin is an element whose track is fixed: a one-time insert of the hour
// (hour_inserts), a rescheduled missed item (§7.1).
type Pin struct {
	InsertID uint64
	TrackID  uint64
	// MissedID is the missed play log row; nil when retention deleted it.
	MissedID *uint64
}

// WarningCode is the stable code of a build warning; the panel translates by
// code.
type WarningCode string

// Warning codes (docs/phase-8-plan.md §3).
const (
	// WarnBlockedByPlayRules: every candidate is blocked by play rules; the
	// element is skipped (§7.3).
	WarnBlockedByPlayRules WarningCode = "element_blocked_by_play_rules"
	// WarnToleranceRelaxed: the track was chosen with a relaxed tolerance.
	WarnToleranceRelaxed WarningCode = "element_tolerance_relaxed"
	// WarnGeneralPool: the track was chosen from the whole library.
	WarnGeneralPool WarningCode = "element_general_pool"
	// WarnEmergency: no track could be chosen; the element is skipped and
	// the emergency playlist covers it (§8.6).
	WarnEmergency WarningCode = "element_emergency"
	// WarnPoolTrackRefused: a pool track breaks a rule; it is skipped and
	// stays in the pool.
	WarnPoolTrackRefused WarningCode = "pool_track_refused"
	// WarnPoolEmpty: the pool has no unused track; the default mode filled
	// the segment.
	WarnPoolEmpty WarningCode = "pool_empty"
	// WarnFillerWaiting: the segment waits for the answer of a filler
	// reminder.
	WarnFillerWaiting WarningCode = "filler_waiting"
	// WarnGapUnfilled: no filler track could close the gap.
	WarnGapUnfilled WarningCode = "gap_unfilled"
	// WarnElementReplaced: a rescheduled item at a fixed minute replaces
	// the clock's fixed element in this hour (§7.2).
	WarnElementReplaced WarningCode = "element_replaced"
	// WarnRescheduleDropped: a rescheduled item no longer fits its hour or
	// breaks a rule there; it is removed and its missed item is open again.
	WarnRescheduleDropped WarningCode = "reschedule_dropped"
)

// WarningCodes lists every warning code.
func WarningCodes() []WarningCode {
	return []WarningCode{
		WarnBlockedByPlayRules, WarnToleranceRelaxed, WarnGeneralPool, WarnEmergency,
		WarnPoolTrackRefused, WarnPoolEmpty, WarnFillerWaiting, WarnGapUnfilled,
		WarnElementReplaced, WarnRescheduleDropped,
	}
}

// Warning is a problem of a build. Element and Segment are indexes into the
// clock.
type Warning struct {
	Code    WarningCode
	Message string
	Element *int
	Segment *int
	TrackID *uint64
}

// filler reports whether the warning belongs to the filler (log component).
func (w Warning) filler() bool {
	switch w.Code {
	case WarnPoolTrackRefused, WarnPoolEmpty, WarnFillerWaiting, WarnGapUnfilled:
		return true
	}
	return false
}

// Waiting says how a build fills the segments that wait for the operator: a
// short segment of an incomplete clock (holder none) or of an ask holder.
type Waiting int

// Waiting modes.
const (
	// WaitAsk leaves the gaps; a reminder asks the operator.
	WaitAsk Waiting = iota
	// WaitDefault fills them with the default mode (deadline passed).
	WaitDefault
	// WaitAnswer fills the first with the answer track, the others with the
	// default mode.
	WaitAnswer
)

// Settings are the settings of a build.
type Settings struct {
	// MinFillerGapMS: a smaller gap gets no filler.
	MinFillerGapMS int
	// MaxRelaxedToleranceMS caps the relaxed tolerance of the emergency
	// steps.
	MaxRelaxedToleranceMS int
}

// HourInput is everything a build reads.
type HourInput struct {
	// Start is the UTC start of the hour.
	Start time.Time
	// Location is the station timezone, for play rule windows and the
	// per-hour limit.
	Location *time.Location
	Clock    clocks.Clock
	Layout   clocks.Layout
	// DefaultFiller fills segments without a filler mode of their own, an
	// empty pool, and expired reminders: the clock's default, else the
	// global fallback (§7). Its mode is random, playlist, or artist.
	DefaultFiller clocks.Filler
	Library       *Library
	// History are the plays around the hour, without the planned items of
	// the hour itself.
	History    []Play
	Separation Separation
	// Pool are the unused rows of the clock's filler pool, in order.
	Pool    []PoolRow
	Waiting Waiting
	// AnswerTrackID is the answer of WaitAnswer.
	AnswerTrackID uint64
	// Pins are the elements with a fixed track, by element index.
	Pins     map[int]Pin
	Settings Settings
	Seed     uint64
}

// HourPlan is the result of a build.
type HourPlan struct {
	// Items in time order.
	Items    []Item
	Warnings []Warning
	// Asked are the segments that wait for the operator (WaitAsk) or were
	// resolved for it (WaitDefault, WaitAnswer).
	Asked []int
	// AskMode is the filler mode of the first asked segment (none or ask).
	AskMode clocks.FillerMode
	// PoolEmpty: a pool segment found the pool empty; the operator is
	// reminded to refill it.
	PoolEmpty bool
	// AnswerRefused: the answer track breaks separation or a play rule at
	// its time, or is not in the library.
	AnswerRefused bool
	// PinsRefused are the inserts whose pinned track could not be placed
	// whole: it breaks a rule at its start, does not fit its segment, or
	// its segment was full.
	PinsRefused []uint64
}
