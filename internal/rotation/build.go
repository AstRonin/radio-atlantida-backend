package rotation

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

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
	cursor int
	prev   kind
	softs  []soft
}

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
//     an allowed track of it. Before a hard point the last track is the best
//     fit to its window, else it is cut at the target and faded out.
//  5. Inserts chain: one starts when the content before it ends, not
//     before its window opens; past its window it is skipped (template) or
//     refused (one-time).
func Build(in HourInput) HourPlan {
	b := &builder{
		in:     in,
		rng:    rand.New(rand.NewPCG(in.Seed, in.Seed^0x9e3779b97f4a7c15)), //nolint:gosec // G404: rotation needs a seeded, reproducible source, not a secure one
		chk:    newChecker(in),
		plan:   HourPlan{Refused: map[uint64]Refusal{}},
		minGap: max(in.Settings.MinGapMS, 1),
		tracks: map[uint64][]Track{},
	}
	b.index()

	anchors := b.anchors()
	var starts, ends []Insert
	for _, ins := range in.HourInserts {
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
	for i := range starts {
		b.placeStartInsert(&starts[i])
	}
	for i, p := range in.Clock.Playlists {
		if r := p.EffectiveRule(); r == clocks.RuleStart || r == clocks.RuleStartEnd {
			b.placeStartRule(i)
		}
	}

	b.softs = b.softAnchors(anchors, blockStart, len(block) > 0)
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

// index groups the tracks of the clock's playlists and insert playlists,
// and lists the fill playlists with their shares (decision 9: without
// shares, every fill playlist weighs 1).
func (b *builder) index() {
	want := map[uint64]bool{}
	for _, p := range b.in.Clock.Playlists {
		want[p.PlaylistID] = true
		if p.EffectiveRule() == clocks.RuleFill {
			w := 1
			if p.FillWeight != nil {
				w = *p.FillWeight
			}
			b.fill = append(b.fill, fillSource{playlistID: p.PlaylistID, weight: w})
		}
	}
	for _, ins := range b.in.Clock.Inserts {
		if ins.PlaylistID != nil {
			want[*ins.PlaylistID] = true
		}
	}
	for _, t := range b.in.Library.Tracks {
		for _, id := range t.PlaylistIDs {
			if want[id] {
				b.tracks[id] = append(b.tracks[id], t)
			}
		}
	}
}

// at returns the instant of an offset into the hour.
func (b *builder) at(offsetMS int) time.Time {
	return b.in.Start.Add(time.Duration(offsetMS) * time.Millisecond)
}

// allowedOf returns the tracks of ts that may start at offset.
func (b *builder) allowedOf(ts []Track, offset int) []Track {
	at := b.at(offset)
	var out []Track
	for _, t := range ts {
		if b.chk.allowed(t, at) {
			out = append(out, t)
		}
	}
	return out
}

func clip(ms int) int { return min(max(ms, 0), clocks.HourMS) }

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
		if a.hourInsert != nil {
			t, ok := b.hourTrack(a.hourInsert, a.target)
			if !ok {
				continue
			}
			a.track, a.item = t, hourItem(a.hourInsert)
		} else {
			ci := b.in.Clock.Inserts[*a.item.InsertIndex]
			t, ok := b.templateTrack(*a.item.InsertIndex, ci, a.target)
			if !ok {
				continue
			}
			a.track = t
		}
		b.chk.add(a.track.ID, a.track.ArtistID, b.at(a.target))
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
	pid := b.in.Clock.Playlists[i].PlaylistID
	allowed := b.allowedOf(b.tracks[pid], b.cursor)
	if len(allowed) == 0 {
		b.ruleMissing(i, b.cursor, "the start track")
		return
	}
	t := allowed[b.rng.IntN(len(allowed))]
	b.place(t, Item{Source: SourceRule, PlaylistID: new(pid)}, b.cursor, clocks.HourMS, kindRule, true)
}

// softAnchors returns the middle and spread slots, by target. A spread
// playlist cuts the free time (the hour without the start block, the
// inserts, and the end block) into equal parts; each slot lies at a random
// point of the middle half of its part, with a window of a quarter part on
// each side.
func (b *builder) softAnchors(anchors []anchor, blockStart int, hasBlock bool) []soft {
	type span struct{ from, to int }
	busy := []span{{0, b.cursor}}
	for _, a := range anchors {
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
			out = append(out, soft{
				target: middleTargetMS, earliest: middleTargetMS - middleWindowMS,
				latest: middleTargetMS + middleWindowMS, playlist: i,
			})
		case clocks.RuleSpread:
			n := 1
			if c := p.EffectiveCount(); c != nil {
				n = *c
			}
			part := total / n
			if part <= 0 {
				b.warnRule(WarnRuleSlotSkipped, i, "the hour has no free time for its %d tracks", n)
				continue
			}
			for k := range n {
				x := k*part + part/4 + b.rng.IntN(max(part/2, 1))
				out = append(out, soft{target: clockAt(x), earliest: clockAt(x - part/4), latest: clockAt(x + part/4), playlist: i})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b soft) int { return a.target - b.target })
	return out
}

// nextSoft returns the first slot that is still open and lies before limit.
func (b *builder) nextSoft(limit int) *soft {
	if len(b.softs) == 0 || b.softs[0].target >= limit {
		return nil
	}
	return &b.softs[0]
}

func (b *builder) popSoft() { b.softs = b.softs[1:] }

// fillTo fills the time up to the window of h with fill tracks, placing the
// rule tracks of the slots on the way.
func (b *builder) fillTo(h hard) {
	emergency := false
	for {
		s := b.nextSoft(h.target)
		if s != nil && b.cursor > s.latest {
			b.warnRule(WarnRuleSlotSkipped, s.playlist, "the slot %s to %s is passed", clockTime(s.earliest), clockTime(s.latest))
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
			t, ok = b.closest(b.allowedOf(b.in.Library.Tracks, b.cursor), h.target-b.cursor)
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
	t := fit[b.rng.IntN(len(fit))]
	b.place(t, Item{Source: SourceRule, PlaylistID: new(pid)}, b.cursor, clocks.HourMS, kindRule, true)
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
		allowed := b.allowedOf(b.tracks[left[k].playlistID], b.cursor)
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
		for _, t := range b.allowedOf(b.tracks[f.playlistID], b.cursor) {
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

func (b *builder) warn(w Warning) { b.plan.Warnings = append(b.plan.Warnings, w) }

func (b *builder) warnRule(code WarningCode, playlist int, format string, args ...any) {
	b.warn(Warning{Code: code, PlaylistIndex: new(playlist), Message: fmt.Sprintf(format, args...)})
}

// clock formats an offset into the hour as station local HH:MM:SS.
func (b *builder) clock(offset int) string {
	return b.at(offset).In(b.in.Location).Format(time.TimeOnly)
}

// clockTime formats an offset into the hour as MM:SS.
func clockTime(ms int) string { return fmt.Sprintf("%02d:%02d", ms/minuteMS, ms/1000%60) }

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
