package rotation

import (
	"slices"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// softAnchors returns the middle and spread slots, by target. A spread
// playlist cuts the free time (the hour without the start block, the
// inserts, and the end block) into equal parts; each slot lies at a random
// point of the middle half of its part, with a window of a quarter part on
// each side. A claiming pin does not count as busy and every slot is drawn,
// the claimed ones too, so the slots that remain keep their windows.
func (b *builder) softAnchors(anchors []anchor, blockStart int, hasBlock bool) []soft {
	type span struct{ from, to int }
	// The slots of a partial hour are drawn over the whole hour, so the time
	// that has passed is not busy: its slots fall out in fillTo, and the slots
	// that remain are not moved or added to make up for them.
	busy := []span{{0, b.cursor - b.in.FromMS}}
	for _, a := range anchors {
		if a.hourInsert != nil && b.claims.pins[a.hourInsert.ID] {
			continue
		}
		busy = append(busy, span{a.target, min(a.target+a.track.DurationMS, clocks.HourMS)})
	}
	if hasBlock {
		busy = append(busy, span{blockStart, clocks.HourMS})
	}
	slices.SortFunc(busy, func(a, b span) int { return a.from - b.from })
	var free []span
	at, total := 0, 0
	for _, s := range busy {
		if s.from > at {
			free = append(free, span{at, s.from})
			total += s.from - at
		}
		at = max(at, s.to)
	}
	if at < clocks.HourMS {
		free = append(free, span{at, clocks.HourMS})
		total += clocks.HourMS - at
	}
	clockAt := func(x int) int {
		for _, f := range free {
			if x < f.to-f.from {
				return f.from + max(x, 0)
			}
			x -= f.to - f.from
		}
		return clocks.HourMS
	}

	var out []soft
	for i, p := range b.in.Clock.Playlists {
		switch p.EffectiveRule() {
		case clocks.RuleMiddle:
			if b.claims.middle[i] {
				continue
			}
			out = append(out, soft{
				target: middleTargetMS, earliest: middleTargetMS - middleWindowMS,
				latest: middleTargetMS + middleWindowMS, playlist: i,
			})
		case clocks.RuleSpread:
			n := spreadCount(p)
			part := total / n
			if part <= 0 {
				b.warnRule(WarnRuleSlotSkipped, i, "the hour has no free time for its %d tracks", n)
				continue
			}
			slots := make([]soft, 0, n)
			for k := range n {
				x := k*part + part/4 + b.rng.IntN(max(part/2, 1))
				slots = append(slots, soft{target: clockAt(x), earliest: clockAt(x - part/4), latest: clockAt(x + part/4), playlist: i})
			}
			for _, origin := range b.claims.spread[i] {
				k := nearestSlot(slots, origin)
				slots = slices.Delete(slots, k, k+1)
			}
			out = append(out, slots...)
		}
	}
	slices.SortStableFunc(out, func(a, b soft) int { return a.target - b.target })
	return out
}

// nearestSlot returns the index of the slot whose window holds origin, else
// of the slot whose window is nearest; the earlier slot on a tie.
func nearestSlot(slots []soft, origin int) int {
	best, bestDist := 0, -1
	for k, s := range slots {
		d := max(s.earliest-origin, origin-s.latest, 0)
		if bestDist < 0 || d < bestDist {
			best, bestDist = k, d
		}
	}
	return best
}

// nextSoft returns the first slot that is still open and lies before limit.
func (b *builder) nextSoft(limit int) *soft {
	if len(b.softs) == 0 || b.softs[0].target >= limit {
		return nil
	}
	return &b.softs[0]
}

func (b *builder) popSoft() { b.softs = b.softs[1:] }

// placeSoft places the rule track of a slot at the cursor. It is skipped
// when it would end next to the insert or end track that follows, or past
// the end of the hour.
func (b *builder) placeSoft(s soft, h hard) {
	pid := b.in.Clock.Playlists[s.playlist].PlaylistID
	room := h.earliest
	if h.insert != nil || h.endBlock {
		room -= b.minGap // a fill track must follow it
	}
	var fit []Track
	for _, t := range b.allowedOf(b.tracks[pid], b.cursor) {
		if b.cursor+t.DurationMS <= room {
			fit = append(fit, t)
		}
	}
	if len(fit) == 0 {
		if len(b.allowedOf(b.tracks[pid], b.cursor)) == 0 {
			b.ruleMissing(s.playlist, b.cursor, "the slot at "+clockTime(s.target))
			return
		}
		b.warnRule(WarnRuleSlotSkipped, s.playlist, "the slot at %s is skipped: its track would end next to the insert or past the hour",
			clockTime(s.target))
		return
	}
	fit = b.avoidClaimed(fit)
	t := fit[b.rng.IntN(len(fit))]
	b.place(t, Item{Source: SourceRule, PlaylistID: new(pid)}, b.cursor, clocks.HourMS, kindRule, true)
}
