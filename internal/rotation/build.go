package rotation

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

const (
	// maxFillerTracks bounds the filler tracks of one gap (§7: "until the
	// shortfall is closed or the iteration limit is reached").
	maxFillerTracks = 3
	// maxRelaxSteps bounds the emergency relaxation of a tolerance; each
	// step doubles it, up to Settings.MaxRelaxedToleranceMS.
	maxRelaxSteps = 8
	// minRelaxStepMS is the first relaxed tolerance of an element whose
	// tolerance is zero.
	minRelaxStepMS = 1000
	minuteMS       = 60_000
)

// window is where a segment ends, in ms from the start of the hour: the
// start window of the fixed element that closes it, or the end of the hour.
type window struct {
	earliest, target, latest int
}

func windowOf(elements []clocks.Element, s clocks.Segment) window {
	if s.ClosedBy == nil {
		return window{clocks.HourMS, clocks.HourMS, clocks.HourMS}
	}
	f := elements[*s.ClosedBy]
	t := *f.FixedMinute * minuteMS
	return window{t - f.FixedToleranceMS, t, t + f.FixedToleranceMS}
}

// builder holds the state of one build.
type builder struct {
	in   HourInput
	rng  *rand.Rand
	chk  *checker
	pool []PoolRow
	plan HourPlan
	// answered: the answer track was offered to a segment.
	answered bool
	// placed are the inserts whose pinned track is planned.
	placed map[uint64]bool
}

// Build plans one hour of a clock (§7, §7.2). It is deterministic for a
// given input and seed. Segments are filled in order:
//
//   - The fixed element that opens a segment starts where the content
//     before it ends, but not before the earliest start of its window.
//   - Each element gets a track from the union of its playlists that passes
//     separation and its play rules, within nominal ± tolerance: at random
//     with equal weights, or, for the last element of a segment, the best
//     fit to the time left.
//   - An item that ends past the segment's latest end is the crossing track:
//     cut at the segment's target end if it starts before it (truncated,
//     faded out by Liquidsoap), removed otherwise. The rest of the segment
//     is removed; overflow never triggers filler.
//   - A gap before the segment's earliest end is closed with filler.
func Build(in HourInput) HourPlan {
	b := &builder{
		in:     in,
		rng:    rand.New(rand.NewPCG(in.Seed, in.Seed^0x9e3779b97f4a7c15)), //nolint:gosec // G404: rotation needs a seeded, reproducible source, not a secure one
		chk:    newChecker(in),
		pool:   slices.Clone(in.Pool),
		placed: map[uint64]bool{},
	}
	elements := in.Clock.Elements
	segs := in.Layout.Segments
	cursor := 0
	for k, seg := range segs {
		w := windowOf(elements, seg)
		stopped := false
		if k > 0 {
			cursor = max(cursor, windowOf(elements, segs[k-1]).earliest)
			cursor, stopped = b.element(*segs[k-1].ClosedBy, cursor, w, len(seg.Elements) == 0)
		}
		for j, i := range seg.Elements {
			if stopped {
				break
			}
			cursor, stopped = b.element(i, cursor, w, j == len(seg.Elements)-1)
		}
		if !stopped {
			cursor = b.fill(seg, cursor, w)
		}
	}
	// A pinned element after the end of a full segment is never reached.
	for _, i := range slices.Sorted(maps.Keys(in.Pins)) {
		p := in.Pins[i]
		if !b.placed[p.InsertID] && !slices.Contains(b.plan.PinsRefused, p.InsertID) {
			b.refusePin(i, p, "its segment is full before it")
		}
	}
	return b.plan
}

// at returns the instant of an offset into the hour.
func (b *builder) at(offsetMS int) time.Time {
	return b.in.Start.Add(time.Duration(offsetMS) * time.Millisecond)
}

// element plans element i at cursor and returns the new cursor and whether
// the segment is full.
func (b *builder) element(i, cursor int, w window, last bool) (int, bool) {
	if cursor >= w.latest {
		return cursor, true
	}
	if p, ok := b.in.Pins[i]; ok {
		return b.pinned(i, p, cursor, w)
	}
	e := b.in.Clock.Elements[i]
	target := e.NominalDurationMS
	if last {
		target = min(max(w.target-cursor, e.NominalDurationMS-e.DurationToleranceMS), e.NominalDurationMS+e.DurationToleranceMS)
	}
	t, ok := b.pick(i, e, b.at(cursor), target, last)
	if !ok {
		return cursor, false
	}
	return b.place(t, &e.ID, cursor, w, SourceClock, nil)
}

// pinned plans an element with a fixed track (a rescheduled item). The
// track must play whole: it is never truncated, relaxed, or replaced; if it
// cannot, the insert is refused and the segment goes on without it.
func (b *builder) pinned(i int, p Pin, cursor int, w window) (int, bool) {
	at := b.at(cursor)
	t, ok := b.in.Library.track(p.TrackID)
	switch {
	case !ok:
		b.refusePin(i, p, "the track has no duration")
		return cursor, false
	case !b.chk.eligible(t, at):
		b.refusePin(i, p, fmt.Sprintf("the track breaks separation or a play rule at %s", b.clock(at)))
		return cursor, false
	case cursor+t.DurationMS > w.latest:
		b.refusePin(i, p, fmt.Sprintf("starting at %s it runs past the end of its segment", b.clock(at)))
		return cursor, false
	}
	cursor, full := b.place(t, nil, cursor, w, SourceReschedule, nil)
	b.plan.Items[len(b.plan.Items)-1].RescheduleOfID = copyPtr(p.MissedID)
	b.placed[p.InsertID] = true
	return cursor, full
}

func (b *builder) refusePin(i int, p Pin, reason string) {
	b.plan.PinsRefused = append(b.plan.PinsRefused, p.InsertID)
	b.warn(WarnRescheduleDropped, &i, nil, &p.TrackID, "the rescheduled item cannot be placed: %s", reason)
}

// pick chooses the track of an element starting at at, with the emergency
// steps of §7 when nothing fits.
func (b *builder) pick(i int, e clocks.Element, at time.Time, target int, best bool) (Track, bool) {
	var eligible []Track
	candidates, blocked := 0, 0
	for _, t := range b.in.Library.Tracks {
		if !t.inAny(e.PlaylistIDs) {
			continue
		}
		candidates++
		switch {
		case !b.chk.allowed(t, at):
			blocked++
		case b.chk.separated(t, at):
			eligible = append(eligible, t)
		}
	}
	if candidates > 0 && blocked == candidates {
		b.warn(WarnBlockedByPlayRules, &i, nil, nil, "play rules block every track of the element at %s; it is skipped", b.clock(at))
		return Track{}, false
	}
	if t, ok := b.choose(eligible, target, e.DurationToleranceMS, best); ok {
		return t, true
	}

	tol := e.DurationToleranceMS
	for range maxRelaxSteps {
		next := min(max(2*tol, minRelaxStepMS), b.in.Settings.MaxRelaxedToleranceMS)
		if next <= tol {
			break
		}
		tol = next
		if t, ok := b.choose(eligible, target, tol, best); ok {
			b.warn(WarnToleranceRelaxed, &i, nil, &t.ID, "no track fits ± %d s at %s; chosen with ± %d s",
				e.DurationToleranceMS/1000, b.clock(at), tol/1000)
			return t, true
		}
	}

	if t, ok := b.closest(b.allEligible(at, func(Track) bool { return true }), target); ok {
		b.warn(WarnGeneralPool, &i, nil, &t.ID, "no track of the element's playlists fits at %s; chosen from the whole library", b.clock(at))
		return t, true
	}
	b.warn(WarnEmergency, &i, nil, nil, "no track passes the rules at %s; the element is skipped", b.clock(at))
	return Track{}, false
}

// choose returns a track within target ± tol: at random, every track with
// the same weight, or the best fit.
func (b *builder) choose(ts []Track, target, tol int, best bool) (Track, bool) {
	var fit []Track
	for _, t := range ts {
		if abs(t.DurationMS-target) <= tol {
			fit = append(fit, t)
		}
	}
	if len(fit) == 0 {
		return Track{}, false
	}
	if best {
		return b.closest(fit, target)
	}
	return fit[b.rng.IntN(len(fit))], true
}

// closest returns the track whose duration is nearest to target; ties are
// broken at random.
func (b *builder) closest(ts []Track, target int) (Track, bool) {
	if len(ts) == 0 {
		return Track{}, false
	}
	bestDiff := abs(ts[0].DurationMS - target)
	var ties []Track
	for _, t := range ts {
		switch d := abs(t.DurationMS - target); {
		case d < bestDiff:
			bestDiff, ties = d, append(ties[:0], t)
		case d == bestDiff:
			ties = append(ties, t)
		}
	}
	return ties[b.rng.IntN(len(ties))], true
}

// allEligible returns the library tracks that match and pass the rules at
// at.
func (b *builder) allEligible(at time.Time, match func(Track) bool) []Track {
	var out []Track
	for _, t := range b.in.Library.Tracks {
		if match(t) && b.chk.eligible(t, at) {
			out = append(out, t)
		}
	}
	return out
}

// place adds a track at cursor under the crossing rule and returns the new
// cursor and whether the segment is full.
func (b *builder) place(t Track, elementID *uint64, cursor int, w window, src Source, poolRow *uint64) (int, bool) {
	it := Item{TrackID: t.ID, ElementID: elementID, OffsetMS: cursor, OutMS: t.DurationMS, Source: src, PoolRowID: poolRow}
	end, full := cursor+t.DurationMS, false
	if end > w.latest {
		if cursor >= w.target {
			return cursor, true // removed: it would not play at all
		}
		it.OutMS, it.Truncated = w.target-cursor, true
		end, full = w.target, true
	}
	b.plan.Items = append(b.plan.Items, it)
	b.chk.add(t.ID, t.ArtistID, b.at(cursor))
	return end, full
}

// fillState is the filler source of one gap; it falls back to the default
// mode when the answer or the pool cannot serve.
type fillState struct {
	mode   clocks.Filler
	answer bool
}

// fill closes the gap before the segment's earliest end with filler (§7)
// and returns the new cursor.
func (b *builder) fill(seg clocks.Segment, cursor int, w window) int {
	minGap := max(b.in.Settings.MinFillerGapMS, 1)
	if w.earliest-cursor < minGap {
		return cursor
	}
	holder := b.in.Clock.Elements[seg.Holder].Filler
	st := fillState{mode: holder}
	asks := holder.Mode == clocks.FillerAsk || (holder.Mode == clocks.FillerNone && seg.ShortfallMS > 0)
	switch {
	case asks:
		if len(b.plan.Asked) == 0 {
			b.plan.AskMode = holder.Mode
		}
		b.plan.Asked = append(b.plan.Asked, seg.Index)
		switch b.in.Waiting {
		case WaitAsk:
			b.warn(WarnFillerWaiting, nil, &seg.Index, nil, "segment %d is short by %d s and waits for the filler reminder",
				seg.Index, (w.earliest-cursor+999)/1000)
			return cursor
		case WaitAnswer:
			st.answer = !b.answered
			b.answered = true
		}
		st.mode = b.in.DefaultFiller
	case holder.Mode == clocks.FillerNone:
		st.mode = b.in.DefaultFiller
	}

	for range maxFillerTracks {
		if w.earliest-cursor < minGap {
			break
		}
		t, src, row, ok := b.fillerTrack(&st, seg.Index, b.at(cursor), w.target-cursor)
		if !ok {
			b.warn(WarnGapUnfilled, nil, &seg.Index, nil, "no filler track passes the rules for the %d s gap of segment %d",
				(w.earliest-cursor+999)/1000, seg.Index)
			break
		}
		var full bool
		if cursor, full = b.place(t, nil, cursor, w, src, row); full {
			break
		}
	}
	return cursor
}

// fillerTrack picks one filler track at at, the best fit to want.
func (b *builder) fillerTrack(st *fillState, segment int, at time.Time, want int) (Track, Source, *uint64, bool) {
	if st.answer {
		st.answer = false
		if t, ok := b.in.Library.track(b.in.AnswerTrackID); ok && b.chk.eligible(t, at) {
			return t, SourceFillerAnswer, nil, true
		}
		b.plan.AnswerRefused = true
	}
	if st.mode.Mode == clocks.FillerPool {
		if t, row, ok := b.poolTrack(segment, at); ok {
			return t, SourceFillerPool, &row, true
		}
		st.mode = b.in.DefaultFiller
	}
	match, src := func(Track) bool { return true }, SourceFillerRandom
	if id := st.mode.SourceID; id != nil {
		switch st.mode.Mode {
		case clocks.FillerPlaylist:
			match, src = func(t Track) bool { return t.inAny([]uint64{*id}) }, SourceFillerPlaylist
		case clocks.FillerArtist:
			match, src = func(t Track) bool { return t.ArtistID != nil && *t.ArtistID == *id }, SourceFillerArtist
		}
		// A source with no track at all (a fallback playlist deleted since
		// it was set) fills at random.
		if !slices.ContainsFunc(b.in.Library.Tracks, match) {
			match, src = func(Track) bool { return true }, SourceFillerRandom
		}
	}
	t, ok := b.closest(b.allEligible(at, match), want)
	return t, src, nil, ok
}

// poolTrack takes the next pool track that passes the rules at at. A
// refused track is skipped with a warning and stays unused in the pool.
func (b *builder) poolTrack(segment int, at time.Time) (Track, uint64, bool) {
	if len(b.pool) == 0 {
		b.plan.PoolEmpty = true
		b.warn(WarnPoolEmpty, nil, &segment, nil, "the filler pool is empty; the default filler mode is used")
		return Track{}, 0, false
	}
	for len(b.pool) > 0 {
		row := b.pool[0]
		b.pool = b.pool[1:]
		if t, ok := b.in.Library.track(row.TrackID); ok && b.chk.eligible(t, at) {
			return t, row.ID, true
		}
		b.warn(WarnPoolTrackRefused, nil, &segment, &row.TrackID, "pool track %d breaks separation or a play rule at %s; skipped",
			row.TrackID, b.clock(at))
	}
	return Track{}, 0, false
}

func (b *builder) warn(code WarningCode, element, segment *int, trackID *uint64, format string, args ...any) {
	b.plan.Warnings = append(b.plan.Warnings, Warning{
		Code: code, Message: fmt.Sprintf(format, args...),
		Element: copyPtr(element), Segment: copyPtr(segment), TrackID: copyPtr(trackID),
	})
}

// clock formats an instant as station local HH:MM:SS.
func (b *builder) clock(at time.Time) string { return at.In(b.in.Location).Format(time.TimeOnly) }

func copyPtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return new(*p)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
