package rotation

import (
	"math/rand/v2"
)

// kind is the kind of the item last placed, for the rule that a rule
// track never starts next to an insert or another rule track.
type kind int

const (
	kindNone kind = iota
	kindFill
	kindRule
	kindInsert
)

// hard is a point the content before it must reach: a timed insert (its
// start window), the block of end tracks (a fixed start), or the end of the
// hour.
type hard struct {
	target, earliest, latest int
	// insert is the timed insert of the point; nil for the end block and
	// the end of the hour.
	insert *anchor
	// endBlock marks the block of end tracks.
	endBlock bool
}

// anchor is a timed insert with its track: a template insert or a fixed
// one-time insert.
type anchor struct {
	target, earliest, latest int
	track                    Track
	item                     Item
	// hourInsert is set for a one-time insert, which is refused instead of
	// skipped.
	hourInsert *Insert
}

// soft is the slot of a middle or spread rule track: reached by a best-fit
// fill track, never by cutting.
type soft struct {
	target, earliest, latest int
	playlist                 int // index into the clock's playlists
}

// blockItem is a track of the end block, at a fixed offset.
type blockItem struct {
	track      Track
	item       Item
	playlist   int // the rule playlist; -1 for a one-time insert
	hourInsert *Insert
}

// fillSource is a fill playlist with its share.
type fillSource struct {
	playlistID uint64
	weight     int
}

// builder holds the state of one build.
type builder struct {
	in     HourInput
	rng    *rand.Rand
	chk    *checker
	plan   HourPlan
	minGap int
	// tracks are the tracks of each playlist of the clock and its inserts.
	tracks map[uint64][]Track
	fill   []fillSource
	// pinned are the tracks of the one-time inserts of the hour. They count
	// as played in this hour: fill draws never pick them again (decision 6).
	// Template inserts and rule slots are not affected.
	pinned map[uint64]bool
	claims slotClaims
	cursor int
	prev   kind
	softs  []soft
}

// slotClaims are the rule slots that pins of rule tracks took over
// (docs/track-pin-rule-plan.md). The maps are by index into the clock's
// playlists.
type slotClaims struct {
	start, end, middle map[int]bool
	// spread holds the origins of the claims on the slots of a spread
	// playlist, in insert ID order. Which slot each takes is known once the
	// windows are drawn (softAnchors).
	spread map[int][]int
	// tracks are the tracks of the claiming pins: rule draws avoid them.
	tracks map[uint64]bool
	// pins are the IDs of the claiming pins.
	pins map[uint64]bool
}
