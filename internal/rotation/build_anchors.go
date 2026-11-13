package rotation

import (
	"fmt"
	"slices"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// opening does what comes before the fill: it resolves the anchors and the
// end block, places the start block, and sets the soft slots. It returns the
// anchors still to be placed and the end block with its start.
func (b *builder) opening() ([]anchor, []blockItem, int) {
	b.index()

	anchors := b.anchors()
	var starts, ends []Insert
	for _, ins := range b.in.HourInserts {
		switch ins.Placement {
		case PlacementStart:
			starts = append(starts, ins)
		case PlacementEnd:
			ends = append(ends, ins)
		}
	}
	block, blockStart := b.endBlock(ends)

	// The start block: an insert at 0:00, the one-time start inserts, the
	// start tracks.
	for len(anchors) > 0 && anchors[0].target == 0 {
		b.placeAnchor(anchors[0])
		anchors = anchors[1:]
	}
	// The start of a partial hour has passed.
	if b.in.FromMS == 0 {
		for i := range starts {
			b.placeStartInsert(&starts[i])
		}
		for i, p := range b.in.Clock.Playlists {
			if r := p.EffectiveRule(); r == clocks.RuleStart || r == clocks.RuleStartEnd {
				b.placeStartRule(i)
			}
		}
	}

	b.softs = b.softAnchors(anchors, blockStart, len(block) > 0)
	return anchors, block, blockStart
}

// anchors resolves the timed inserts of the hour, in target order, and
// records their plays at the target, so the tracks around them respect
// them.
func (b *builder) anchors() []anchor {
	var out []anchor
	replaced := map[int]bool{}
	for i := range b.in.HourInserts {
		h := &b.in.HourInserts[i]
		if h.Placement != PlacementFixed || h.OffsetMS == nil {
			continue
		}
		for j, ci := range b.in.Clock.Inserts {
			if ci.OffsetMS == *h.OffsetMS && !replaced[j] {
				replaced[j] = true
				b.warn(Warning{
					Code: WarnInsertReplaced, InsertIndex: new(j), HourInsertID: new(h.ID),
					Message: fmt.Sprintf("a one-time insert replaces the insert at %s in this hour", clockTime(ci.OffsetMS)),
				})
			}
		}
		out = append(out, anchor{
			target: *h.OffsetMS, earliest: clip(*h.OffsetMS - h.ToleranceMS), latest: clip(*h.OffsetMS + h.ToleranceMS),
			hourInsert: h,
		})
	}
	for i, ci := range b.in.Clock.Inserts {
		if replaced[i] {
			continue
		}
		out = append(out, anchor{
			target: ci.OffsetMS, earliest: clip(ci.OffsetMS - ci.ToleranceMS), latest: clip(ci.OffsetMS + ci.ToleranceMS),
			item: Item{Source: SourceInsert, ClockInsertID: new(ci.ID), InsertIndex: new(i)},
		})
	}
	slices.SortStableFunc(out, func(a, b anchor) int { return a.target - b.target })

	kept := out[:0]
	for _, a := range out {
		if a.latest < b.in.FromMS {
			continue // its window has passed, in a partial hour
		}
		// An insert whose window holds FromMS starts there, not at its target.
		at := max(a.target, b.in.FromMS)
		if a.hourInsert != nil {
			t, ok := b.hourTrack(a.hourInsert, at)
			if !ok {
				continue
			}
			a.track, a.item = t, hourItem(a.hourInsert)
		} else {
			ci := b.in.Clock.Inserts[*a.item.InsertIndex]
			t, ok := b.templateTrack(*a.item.InsertIndex, ci, at)
			if !ok {
				continue
			}
			a.track = t
		}
		b.chk.add(a.track.ID, a.track.ArtistID, b.at(at))
		kept = append(kept, a)
	}
	return kept
}

// templateTrack returns the track of a template insert at offset: its
// track, or a random allowed track of its playlist.
func (b *builder) templateTrack(i int, ci clocks.Insert, offset int) (Track, bool) {
	skip := func(format string, args ...any) (Track, bool) {
		b.warn(Warning{
			Code: WarnInsertSkipped, InsertIndex: new(i), TrackID: ci.TrackID,
			Message: fmt.Sprintf("the insert at %s is skipped: ", clockTime(ci.OffsetMS)) + fmt.Sprintf(format, args...),
		})
		return Track{}, false
	}
	if ci.TrackID != nil {
		t, ok := b.in.Library.track(*ci.TrackID)
		switch {
		case !ok:
			return skip("its track has no duration")
		case !b.chk.allowed(t, b.at(offset)):
			return skip("its track breaks a play rule at %s", b.clock(offset))
		}
		return t, true
	}
	ts := b.tracks[*ci.PlaylistID]
	if len(ts) == 0 {
		return skip("its playlist has no track that can be planned")
	}
	allowed := b.allowedOf(ts, offset)
	if len(allowed) == 0 {
		return skip("play rules block every track of its playlist at %s", b.clock(offset))
	}
	return allowed[b.rng.IntN(len(allowed))], true
}

// hourTrack returns the track of a one-time insert if it may start at
// offset; otherwise the insert is refused.
func (b *builder) hourTrack(h *Insert, offset int) (Track, bool) {
	t, ok := b.in.Library.track(h.TrackID)
	switch {
	case !ok:
		b.refuse(h, ReasonDoesNotFit, "the track has no duration")
		return Track{}, false
	case offset < 0 || offset+t.DurationMS > clocks.HourMS:
		b.refuse(h, ReasonDoesNotFit, "it runs past the end of the hour")
		return Track{}, false
	case !b.chk.allowed(t, b.at(offset)):
		b.refuse(h, ReasonBlocked, fmt.Sprintf("the track breaks a play rule at %s", b.clock(offset)))
		return Track{}, false
	}
	return t, true
}

func hourItem(h *Insert) Item {
	it := Item{Source: SourceHourInsert, HourInsertID: new(h.ID)}
	if h.MissedID != nil {
		it.Source, it.RescheduleOfID = SourceReschedule, new(*h.MissedID)
	}
	return it
}

func (b *builder) refuse(h *Insert, reason RefusalReason, msg string) {
	b.plan.Refused[h.ID] = Refusal{Reason: reason, Message: msg}
}

// endBlock picks the tracks that end the hour, built backwards from 60:00:
// the one-time end inserts, then the end tracks of the rule playlists in
// clock order. It returns them in play order with the block's start, and
// records their plays.
func (b *builder) endBlock(ends []Insert) ([]blockItem, int) {
	var slots []blockItem
	for i := range ends {
		slots = append(slots, blockItem{playlist: -1, hourInsert: &ends[i]})
	}
	for i, p := range b.in.Clock.Playlists {
		if r := p.EffectiveRule(); r == clocks.RuleEnd || r == clocks.RuleStartEnd {
			slots = append(slots, blockItem{playlist: i})
		}
	}
	start := clocks.HourMS
	var out []blockItem
	for k := len(slots) - 1; k >= 0; k-- {
		s := slots[k]
		if s.hourInsert != nil {
			t, ok := b.in.Library.track(s.hourInsert.TrackID)
			if !ok {
				b.refuse(s.hourInsert, ReasonDoesNotFit, "the track has no duration")
				continue
			}
			t, ok = b.hourTrack(s.hourInsert, start-t.DurationMS)
			if !ok {
				continue
			}
			s.track, s.item = t, hourItem(s.hourInsert)
		} else {
			if b.claims.end[s.playlist] {
				b.burnDraw()
				continue
			}
			pid := b.in.Clock.Playlists[s.playlist].PlaylistID
			var fit []Track
			for _, t := range b.tracks[pid] {
				if t.DurationMS <= start && b.chk.allowed(t, b.at(start-t.DurationMS)) {
					fit = append(fit, t)
				}
			}
			if len(fit) == 0 {
				b.ruleMissing(s.playlist, start, "the end track")
				continue
			}
			fit = b.avoidClaimed(fit)
			s.track = fit[b.rng.IntN(len(fit))]
			s.item = Item{Source: SourceRule, PlaylistID: new(pid)}
		}
		start -= s.track.DurationMS
		s.item.OffsetMS = start
		b.chk.add(s.track.ID, s.track.ArtistID, b.at(start))
		out = append(out, s)
	}
	slices.Reverse(out)
	return out, start
}

// dropBlock gives up the end block: the chain before it runs past its
// start.
func (b *builder) dropBlock(block []blockItem, reason string) {
	for _, bi := range block {
		if bi.hourInsert != nil {
			b.refuse(bi.hourInsert, ReasonDoesNotFit, reason)
		} else {
			b.warnRule(WarnRuleSlotSkipped, bi.playlist, "the end track is skipped: %s", reason)
		}
	}
}

// ruleMissing warns that a rule playlist has no track for a slot: blocked
// by play rules, or without a track that can be planned.
func (b *builder) ruleMissing(i, offset int, slot string) {
	if len(b.tracks[b.in.Clock.Playlists[i].PlaylistID]) == 0 {
		b.warnRule(WarnRuleSlotSkipped, i, "%s is skipped: the playlist has no track that can be planned", slot)
		return
	}
	b.warnRule(WarnRuleBlocked, i, "%s is skipped: play rules block every fitting track of the playlist at %s", slot, b.clock(offset))
}

// placeAnchor places a timed insert where the content before it ends, but
// not before its window opens. Past its window, a template insert is
// skipped and a one-time insert refused. A template insert that crosses
// the end of the hour is cut there; a one-time insert plays whole.
func (b *builder) placeAnchor(a anchor) {
	start := max(b.cursor, a.earliest)
	switch {
	case start > a.latest && a.hourInsert != nil:
		b.refuse(a.hourInsert, ReasonDoesNotFit,
			fmt.Sprintf("the content before it runs to %s, past its window %s to %s", clockTime(start), clockTime(a.earliest), clockTime(a.latest)))
		return
	case start > a.latest:
		b.warn(Warning{
			Code: WarnInsertSkipped, InsertIndex: a.item.InsertIndex, TrackID: new(a.track.ID),
			Message: fmt.Sprintf("the insert at %s is skipped: the content before it runs to %s, past its window",
				clockTime(a.target), clockTime(start)),
		})
		return
	case a.hourInsert != nil && start+a.track.DurationMS > clocks.HourMS:
		b.refuse(a.hourInsert, ReasonDoesNotFit, fmt.Sprintf("starting at %s it runs past the end of the hour", clockTime(start)))
		return
	}
	b.place(a.track, a.item, start, clocks.HourMS, kindInsert, false)
}

// placeStartInsert places a one-time start insert at the cursor.
func (b *builder) placeStartInsert(h *Insert) {
	t, ok := b.hourTrack(h, b.cursor)
	if !ok {
		return
	}
	b.place(t, hourItem(h), b.cursor, clocks.HourMS, kindInsert, true)
}

// placeStartRule places the start track of a rule playlist at the cursor.
func (b *builder) placeStartRule(i int) {
	if b.claims.start[i] {
		b.burnDraw()
		return
	}
	pid := b.in.Clock.Playlists[i].PlaylistID
	allowed := b.allowedOf(b.tracks[pid], b.cursor)
	if len(allowed) == 0 {
		b.ruleMissing(i, b.cursor, "the start track")
		return
	}
	allowed = b.avoidClaimed(allowed)
	t := allowed[b.rng.IntN(len(allowed))]
	b.place(t, Item{Source: SourceRule, PlaylistID: new(pid)}, b.cursor, clocks.HourMS, kindRule, true)
}
