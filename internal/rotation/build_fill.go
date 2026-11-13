package rotation

import (
	"fmt"
	"slices"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// fillTo fills the time up to the window of h with fill tracks, placing the
// rule tracks of the slots on the way.
func (b *builder) fillTo(h hard) {
	emergency := false
	for {
		s := b.nextSoft(h.target)
		if s != nil && b.cursor > s.latest {
			// In a partial hour, a slot whose window opened before the build
			// begins is expected to pass.
			if s.earliest >= b.in.FromMS {
				b.warnRule(WarnRuleSlotSkipped, s.playlist, "the slot %s to %s is passed", clockTime(s.earliest), clockTime(s.latest))
			}
			b.popSoft()
			continue
		}
		if s != nil && b.cursor >= s.earliest && b.prev == kindFill {
			b.placeSoft(*s, h)
			b.popSoft()
			continue
		}
		if h.earliest-b.cursor < b.minGap {
			return // the gap stays: the next point starts earlier within its window, or later
		}

		t, pid, ok := b.drawFill()
		src := SourceFill
		if !ok {
			t, ok = b.closest(b.fillAllowed(b.in.Library.Tracks, b.cursor), h.target-b.cursor)
			if !ok {
				b.warn(Warning{Code: WarnEmergency, Message: fmt.Sprintf(
					"no track passes the play rules at %s; the gap to %s is left for the emergency source", b.clock(b.cursor), clockTime(h.target))})
				return
			}
			if !emergency {
				emergency = true
				b.warn(Warning{Code: WarnGeneralPool, TrackID: new(t.ID), Message: fmt.Sprintf(
					"no fill track passes the play rules at %s; chosen from the whole library", b.clock(b.cursor))})
			}
			src, pid = SourceGeneral, nil
		}
		end := b.cursor + t.DurationMS
		switch {
		case end > h.earliest:
			if bt, bpid, ok := b.bestFit(h.earliest, h.latest, h.target); ok && src == SourceFill {
				b.place(bt, Item{Source: SourceFill, PlaylistID: bpid}, b.cursor, clocks.HourMS, kindFill, true)
				return
			}
			if end <= h.latest {
				b.place(t, Item{Source: src, PlaylistID: pid}, b.cursor, clocks.HourMS, kindFill, true)
				return
			}
			if h.target-b.cursor < minAirMS {
				b.warn(Warning{Code: WarnGapUnfilled, Message: fmt.Sprintf(
					"the %d s gap before %s is too short for any track", (h.target-b.cursor)/1000, clockTime(h.target))})
				return
			}
			b.place(t, Item{Source: src, PlaylistID: pid}, b.cursor, h.target, kindFill, true)
			return
		case s != nil && b.cursor < s.earliest && end > s.earliest && src == SourceFill:
			if bt, bpid, ok := b.bestFit(s.earliest, min(s.latest, h.earliest), s.target); ok {
				t, pid = bt, bpid
			}
		}
		b.place(t, Item{Source: src, PlaylistID: pid}, b.cursor, clocks.HourMS, kindFill, true)
	}
}

// drawFill draws a fill playlist by share, then a random track of it that
// may start at the cursor. A playlist without an allowed track leaves the
// draw for this track.
func (b *builder) drawFill() (Track, *uint64, bool) {
	left := slices.Clone(b.fill)
	for len(left) > 0 {
		total := 0
		for _, f := range left {
			total += f.weight
		}
		r := b.rng.IntN(total)
		k := 0
		for ; r >= left[k].weight; k++ {
			r -= left[k].weight
		}
		allowed := b.fillAllowed(b.tracks[left[k].playlistID], b.cursor)
		if len(allowed) > 0 {
			return allowed[b.rng.IntN(len(allowed))], new(left[k].playlistID), true
		}
		left = slices.Delete(left, k, k+1)
	}
	return Track{}, nil, false
}

// bestFit returns the allowed fill track that, started at the cursor, ends
// within [earliest, latest] closest to target; ties are broken at random.
func (b *builder) bestFit(earliest, latest, target int) (Track, *uint64, bool) {
	type cand struct {
		t   Track
		pid uint64
	}
	seen := map[uint64]bool{}
	var best []cand
	bestDiff := -1
	for _, f := range b.fill {
		for _, t := range b.fillAllowed(b.tracks[f.playlistID], b.cursor) {
			end := b.cursor + t.DurationMS
			if seen[t.ID] || end < earliest || end > latest {
				continue
			}
			seen[t.ID] = true
			switch d := abs(end - target); {
			case bestDiff < 0 || d < bestDiff:
				bestDiff, best = d, append(best[:0], cand{t, f.playlistID})
			case d == bestDiff:
				best = append(best, cand{t, f.playlistID})
			}
		}
	}
	if len(best) == 0 {
		return Track{}, nil, false
	}
	c := best[b.rng.IntN(len(best))]
	return c.t, new(c.pid), true
}

// closest returns the track whose length is nearest to want; ties are
// broken at random.
func (b *builder) closest(ts []Track, want int) (Track, bool) {
	if len(ts) == 0 {
		return Track{}, false
	}
	bestDiff := abs(ts[0].DurationMS - want)
	var ties []Track
	for _, t := range ts {
		switch d := abs(t.DurationMS - want); {
		case d < bestDiff:
			bestDiff, ties = d, append(ties[:0], t)
		case d == bestDiff:
			ties = append(ties, t)
		}
	}
	return ties[b.rng.IntN(len(ties))], true
}

// place adds a track at offset, cut at cutAt with the crossing rule (it is
// marked truncated and faded out by Liquidsoap), and moves the cursor to
// its end. record adds its start to the checker; anchors were recorded
// when they were resolved.
func (b *builder) place(t Track, it Item, offset, cutAt int, k kind, record bool) {
	it.TrackID, it.OffsetMS, it.InMS, it.OutMS = t.ID, offset, t.CueInMS, t.CueInMS+t.DurationMS
	end := offset + t.DurationMS
	if cutAt = min(cutAt, clocks.HourMS); end > cutAt {
		it.OutMS, it.Truncated, end = t.CueInMS+cutAt-offset, true, cutAt
	}
	b.plan.Items = append(b.plan.Items, it)
	if record {
		b.chk.add(t.ID, t.ArtistID, b.at(offset))
	}
	b.cursor, b.prev = end, k
}
