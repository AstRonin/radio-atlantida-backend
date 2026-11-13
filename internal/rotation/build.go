package rotation

import (
	"maps"
	"math/rand/v2"
	"slices"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

const (
	minuteMS = 60_000
	// The middle rule: a track that starts at about 30:00, within ±5
	// minutes (decision 7).
	middleTargetMS = 30 * minuteMS
	middleWindowMS = 5 * minuteMS
	// minAirMS is the shortest part a crossing track may be cut to: the
	// shortest on-air part of a track (the cue rule of the library). A gap
	// shorter than that is left.
	minAirMS = 5000
)

// Build plans one hour of a clock (§7, §7.2, docs/phase-23-plan.md §3.3).
// It is deterministic for a given input and seed.
//
//  1. Anchors: the template inserts and the fixed one-time inserts, by
//     target; a one-time insert at the offset of a template insert replaces
//     it. A playlist insert draws its track now.
//  2. The end block (one-time end inserts, then the end tracks of the rule
//     playlists) ends at 60:00. The start block (an insert at 0:00, the
//     one-time start inserts, the start tracks) opens the hour.
//  3. Middle and spread slots are soft anchors in the free time.
//  4. Fill: before each track a fill playlist is drawn by its share, then
//     an allowed track of it. The tracks of the one-time inserts of the hour
//     are not drawn again in that hour (they count as played). Before a hard point the last track is the best
//     fit to its window, else it is cut at the target and faded out.
//  5. Inserts chain: one starts when the content before it ends, not
//     before its window opens; past its window it is skipped (template) or
//     refused (one-time).
//
// A pin of a rule track takes over one slot of its rule (slotClaims). A
// refused claiming pin gives the slot back in the same build: the hour is
// built again without the refused inserts, and Refused holds those of every
// attempt.
func Build(in HourInput) HourPlan {
	refused := map[uint64]Refusal{}
	for {
		plan := buildHour(in)
		maps.Copy(refused, plan.Refused)
		if !refusedClaim(in.HourInserts, plan.Refused) {
			plan.Refused = refused
			return plan
		}
		// Every insert refused now goes, not only the claiming ones: an insert
		// refused here and placed by the next attempt would be planned and
		// dropped at once.
		in.HourInserts = slices.DeleteFunc(slices.Clone(in.HourInserts), func(h Insert) bool {
			_, gone := plan.Refused[h.ID]
			return gone
		})
	}
}

// refusedClaim reports whether a refused insert carries a slot claim.
func refusedClaim(inserts []Insert, refused map[uint64]Refusal) bool {
	return slices.ContainsFunc(inserts, func(h Insert) bool {
		_, gone := refused[h.ID]
		return gone && h.RulePlaylistID != nil
	})
}

// buildHour is one attempt of Build.
func buildHour(in HourInput) HourPlan {
	b := newBuilder(in)
	anchors, block, blockStart := b.opening()
	hards := make([]hard, 0, len(anchors)+2)
	for i := range anchors {
		a := &anchors[i]
		hards = append(hards, hard{target: a.target, earliest: a.earliest, latest: a.latest, insert: a})
	}
	if len(block) > 0 {
		hards = append(hards, hard{target: blockStart, earliest: blockStart, latest: blockStart, endBlock: true})
	}
	slices.SortStableFunc(hards, func(a, b hard) int { return a.target - b.target })
	hards = append(hards, hard{target: clocks.HourMS, earliest: clocks.HourMS, latest: clocks.HourMS})

	for _, h := range hards {
		switch {
		case h.insert != nil:
			b.fillTo(h)
			b.placeAnchor(*h.insert)
		case h.endBlock && b.cursor > blockStart:
			b.dropBlock(block, "the content before it runs past its start")
		case h.endBlock:
			b.fillTo(h)
			for _, bi := range block {
				b.place(bi.track, bi.item, bi.item.OffsetMS, clocks.HourMS, kindRule, false)
			}
		default:
			b.fillTo(h)
		}
	}
	for _, s := range b.softs {
		b.warnRule(WarnRuleSlotSkipped, s.playlist, "its window %s to %s is taken", clockTime(s.earliest), clockTime(s.latest))
	}
	slices.SortStableFunc(b.plan.Items, func(a, b Item) int { return a.OffsetMS - b.OffsetMS })
	return b.plan
}

func newBuilder(in HourInput) *builder {
	return &builder{
		in:     in,
		rng:    rand.New(rand.NewPCG(in.Seed, in.Seed^0x9e3779b97f4a7c15)), //nolint:gosec // G404: rotation needs a seeded, reproducible source, not a secure one
		chk:    newChecker(in),
		plan:   HourPlan{Refused: map[uint64]Refusal{}},
		minGap: max(in.Settings.MinGapMS, 1),
		tracks: map[uint64][]Track{},
		cursor: in.FromMS,
	}
}
