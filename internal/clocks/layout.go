package clocks

import (
	"fmt"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// HourMS is the length of every clock (§4: length_seconds is fixed at 3600).
const HourMS = 3_600_000

const minuteMS = 60_000

// Segment is a part of the hour between fixed elements (§7.2). Segments are
// fitted separately; shortfall is counted per segment.
type Segment struct {
	Index int
	// StartMS is where the content of the segment starts: the start of the
	// hour or the end of the fixed element before it, on nominal durations.
	StartMS int
	// EndMS is the target end: the target start of the fixed element that
	// closes the segment, or the end of the hour.
	EndMS int
	// ContentMS is the sum of the nominal durations of its elements.
	ContentMS   int
	ShortfallMS int
	// OverflowMS is how far the last segment runs past the end of the hour.
	// The crossing track is cut there (§7); it is not refused.
	OverflowMS int
	// Elements are the indexes (into the clock's elements) of the segment's
	// elements in play order: start elements, flow elements, end elements.
	Elements []int
	// Holder is the element whose filler mode fills the segment: its last
	// element, else the fixed element that opens it, else the one that
	// closes it.
	Holder int
	// ClosedBy is the fixed element that ends the segment; nil for the last
	// segment, which ends with the hour.
	ClosedBy *int
	// FixedStartMS is where the closing fixed element starts on nominal
	// durations: as close to its target as its tolerance allows.
	FixedStartMS *int
}

// Layout is the hour built from a clock's elements on nominal durations
// (§6.0 dry check, §7.2).
type Layout struct {
	Segments []Segment
	// ShortfallMS is the sum of the segments' shortfalls.
	ShortfallMS int
}

// Uncovered returns the short segments whose holder has no filler mode: the
// segments the filler dialog asks about (§6.0).
func (l Layout) Uncovered(elements []Element) []Segment {
	var out []Segment
	for _, s := range l.Segments {
		if s.ShortfallMS > 0 && elements[s.Holder].Filler.Mode == FillerNone {
			out = append(out, s)
		}
	}
	return out
}

// Status is complete when every short segment has a filler mode (§4: a
// short clock with filler_mode none is not complete).
func (l Layout) Status(elements []Element) Status {
	if len(l.Uncovered(elements)) > 0 {
		return StatusIncomplete
	}
	return StatusComplete
}

// Arranged is the result of arranging a saved clock.
type Arranged struct {
	// Elements are the elements kept, in position order.
	Elements []Element
	// Replaced are existing fixed elements removed because a new fixed
	// element overlaps them (§7.2).
	Replaced []Element
	Layout   Layout
}

// Arrange checks the elements of a save, applies the replacement of fixed
// elements, and builds the layout. existing holds the IDs of the clock's
// stored elements; an element with an ID is one of them, an element without
// one is new. Refusals are field errors addressed by the request index
// (elements[2].fixed_minute).
func Arrange(elements []Element, existing map[int64]bool) (Arranged, error) {
	errs := &validation.Error{}
	if len(elements) == 0 {
		errs.Add("elements", validation.Required, "a clock needs at least one element")
		return Arranged{}, errs
	}
	seen := map[int64]bool{}
	for i, e := range elements {
		checkElement(errs, fmt.Sprintf("elements[%d]", i), e)
		switch {
		case e.ID == 0:
		case !existing[e.ID]:
			errs.Add(elementField(i, "id"), validation.UnknownID, "the clock has no element with this ID; leave it out for a new element")
		case seen[e.ID]:
			errs.Add(elementField(i, "id"), validation.Invalid, "element %d is listed twice", e.ID)
		}
		seen[e.ID] = true
	}
	if err := errs.Err(); err != nil {
		return Arranged{}, err
	}

	replaced := replaceFixed(errs, elements)
	if err := errs.Err(); err != nil {
		return Arranged{}, err
	}
	out := Arranged{}
	var reqIndex []int
	for i, e := range elements {
		if replaced[i] {
			out.Replaced = append(out.Replaced, e)
			continue
		}
		out.Elements = append(out.Elements, e)
		reqIndex = append(reqIndex, i)
	}
	l, err := layout(out.Elements, reqIndex)
	if err != nil {
		return Arranged{}, err
	}
	out.Layout = l
	return out, nil
}

// LayoutOf builds the layout of stored elements, which passed Arrange when
// they were saved.
func LayoutOf(elements []Element) (Layout, error) {
	idx := make([]int, len(elements))
	for i := range idx {
		idx[i] = i
	}
	return layout(elements, idx)
}

func elementField(i int, name string) string { return fmt.Sprintf("elements[%d].%s", i, name) }

// checkElement checks one element on its own.
func checkElement(errs *validation.Error, path string, e Element) {
	field := func(name string) string { return path + "." + name }
	if len(e.PlaylistIDs) == 0 {
		errs.Add(field("playlist_ids"), validation.Required, "an element needs at least one playlist")
	}
	ids := map[int64]bool{}
	for _, id := range e.PlaylistIDs {
		if ids[id] {
			errs.Add(field("playlist_ids"), validation.Invalid, "playlist %d is listed twice", id)
			break
		}
		ids[id] = true
	}
	if e.NominalDurationMS < 1 || e.NominalDurationMS > HourMS {
		errs.Add(field("nominal_duration_ms"), validation.Range, "must be between 1 ms and 60 minutes")
	}
	if e.DurationToleranceMS < 0 {
		errs.Add(field("duration_tolerance_ms"), validation.Range, "must not be negative")
	}
	if e.FixedToleranceMS < 0 {
		errs.Add(field("fixed_tolerance_ms"), validation.Range, "must not be negative")
	}
	checkFiller(errs, path, e.Filler, true)

	switch e.Placement {
	case PlacementFlow, PlacementStart, PlacementEnd:
		if e.FixedMinute != nil {
			errs.Add(field("fixed_minute"), validation.Invalid, "only a fixed element has a fixed minute")
		}
	case PlacementFixed:
		switch {
		case e.FixedMinute == nil:
			errs.Add(field("fixed_minute"), validation.Required, "a fixed element needs its minute")
		case *e.FixedMinute < 0 || *e.FixedMinute > 59:
			errs.Add(field("fixed_minute"), validation.Range, "must be between 0 and 59")
		case e.FixedToleranceMS >= 0 && (e.target()-e.FixedToleranceMS < 0 || e.target()+e.FixedToleranceMS > HourMS):
			errs.Add(field("fixed_minute"), validation.DoesNotFit,
				"the start window :%02d ± %d s falls outside the hour", *e.FixedMinute, e.FixedToleranceMS/1000)
		case e.NominalDurationMS >= 1 && e.earliest()+e.NominalDurationMS > HourMS:
			errs.Add(field("nominal_duration_ms"), validation.DoesNotFit,
				"from its earliest start the element runs past the end of the hour")
		}
	default:
		errs.Add(field("placement"), validation.Invalid, "must be flow, start, end, or fixed")
	}
}

// checkFiller checks a filler mode and its source. ask and pool are allowed
// on elements, not as a clock's default (they would fall back to
// themselves, §4).
func checkFiller(errs *validation.Error, path string, f Filler, element bool) {
	field := func(name string) string {
		if path == "" {
			return name
		}
		return path + "." + name
	}
	modeField, sourceField := field("filler_mode"), field("filler_source_id")
	if !element {
		modeField, sourceField = field("default_filler_mode"), field("default_filler_source_id")
	}
	switch f.Mode {
	case FillerPlaylist, FillerArtist:
		if f.SourceID == nil {
			errs.Add(sourceField, validation.Required, "the %s filler mode needs a %s", f.Mode, f.Mode)
		}
		return
	case FillerRandom:
	case FillerNone, FillerAsk, FillerPool:
		if !element {
			errs.Add(modeField, validation.Invalid, "a clock's default filler mode is random, playlist, or artist")
			return
		}
	default:
		errs.Add(modeField, validation.Invalid, "unknown filler mode %q", f.Mode)
		return
	}
	if f.SourceID != nil {
		errs.Add(sourceField, validation.Invalid, "only the playlist and artist filler modes have a source")
	}
}

// overlaps reports whether the start windows of two fixed elements share
// more than a point.
func overlaps(a, b Element) bool {
	d := a.target() - b.target()
	if d < 0 {
		d = -d
	}
	return d < a.FixedToleranceMS+b.FixedToleranceMS
}

// replaceFixed returns the request indexes of existing fixed elements that a
// new fixed element replaces (§7.2), and refuses two new or two existing
// fixed elements whose windows overlap.
func replaceFixed(errs *validation.Error, elements []Element) map[int]bool {
	var fixed []int
	for i, e := range elements {
		if e.Placement == PlacementFixed {
			fixed = append(fixed, i)
		}
	}
	replaced := map[int]bool{}
	for a, i := range fixed {
		for _, j := range fixed[a+1:] {
			ei, ej := elements[i], elements[j]
			if (ei.ID == 0) != (ej.ID == 0) && overlaps(ei, ej) {
				if ei.ID != 0 {
					replaced[i] = true
				} else {
					replaced[j] = true
				}
			}
		}
	}
	for a, i := range fixed {
		for _, j := range fixed[a+1:] {
			if replaced[i] || replaced[j] || !overlaps(elements[i], elements[j]) {
				continue
			}
			errs.Add(elementField(j, "fixed_minute"), validation.Overlap,
				"its start window overlaps the fixed element at :%02d (elements[%d])", *elements[i].FixedMinute, i)
		}
	}
	return replaced
}

// layout builds the segments of checked elements on nominal durations.
// reqIndex maps an element to its request index for refusals.
func layout(elements []Element, reqIndex []int) (Layout, error) {
	errs := &validation.Error{}
	segs := []Segment{{}}
	var fixed []int
	var starts, ends []int
	for i, e := range elements {
		cur := &segs[len(segs)-1]
		switch e.Placement {
		case PlacementStart:
			starts = append(starts, i)
		case PlacementEnd:
			ends = append(ends, i)
		case PlacementFlow:
			cur.Elements = append(cur.Elements, i)
		case PlacementFixed:
			if len(fixed) > 0 && e.target() <= elements[fixed[len(fixed)-1]].target() {
				errs.Add(elementField(reqIndex[i], "fixed_minute"), validation.Order,
					"fixed elements must be in minute order: move it before the fixed element at :%02d",
					*elements[fixed[len(fixed)-1]].FixedMinute)
			}
			cur.ClosedBy = new(i)
			fixed = append(fixed, i)
			segs = append(segs, Segment{Index: len(segs)})
		}
	}
	if err := errs.Err(); err != nil {
		return Layout{}, err
	}
	segs[0].Elements = append(starts, segs[0].Elements...)
	last := &segs[len(segs)-1]
	last.Elements = append(last.Elements, ends...)

	cursor := 0
	out := Layout{}
	for k := range segs {
		s := &segs[k]
		s.StartMS = cursor
		s.EndMS = HourMS
		if s.ClosedBy != nil {
			s.EndMS = elements[*s.ClosedBy].target()
		}
		latest := s.EndMS
		if s.ClosedBy != nil {
			latest += elements[*s.ClosedBy].FixedToleranceMS
		}

		// start and end elements that alone overrun the segment (§7.2).
		pinned := cursor
		for _, i := range s.Elements {
			if elements[i].Placement == PlacementFlow {
				continue
			}
			pinned += elements[i].NominalDurationMS
			if pinned > latest {
				errs.Add(elementField(reqIndex[i], "nominal_duration_ms"), validation.DoesNotFit,
					"the %s elements do not fit into their segment (%s)", elements[i].Placement, segmentName(elements, s))
				return Layout{}, errs
			}
		}

		for _, i := range s.Elements {
			s.ContentMS += elements[i].NominalDurationMS
		}
		end := cursor + s.ContentMS

		if s.ClosedBy == nil {
			s.ShortfallMS = max(0, HourMS-end)
			s.OverflowMS = max(0, end-HourMS)
		} else {
			f := elements[*s.ClosedBy]
			if end > latest {
				errs.Add(elementField(reqIndex[*s.ClosedBy], "fixed_minute"), validation.DoesNotFit,
					"the material before it runs %d s past its latest start", (end-latest+999)/1000)
				return Layout{}, errs
			}
			earliest := f.target() - f.FixedToleranceMS
			start := max(end, earliest)
			s.ShortfallMS = earliest - min(end, earliest)
			s.FixedStartMS = new(start)
			cursor = start + f.NominalDurationMS
			if cursor > HourMS {
				errs.Add(elementField(reqIndex[*s.ClosedBy], "nominal_duration_ms"), validation.DoesNotFit,
					"starting at %s it runs past the end of the hour", clockTime(start))
				return Layout{}, errs
			}
		}

		switch {
		case len(s.Elements) > 0:
			s.Holder = s.Elements[len(s.Elements)-1]
		case k > 0:
			s.Holder = *segs[k-1].ClosedBy
		default:
			s.Holder = *s.ClosedBy
		}
		out.ShortfallMS += s.ShortfallMS
	}
	out.Segments = segs
	return out, nil
}

func segmentName(elements []Element, s *Segment) string {
	if s.ClosedBy == nil {
		return "up to the end of the hour"
	}
	return fmt.Sprintf("before the fixed element at :%02d", *elements[*s.ClosedBy].FixedMinute)
}

// clockTime formats an offset into the hour as mm:ss.
func clockTime(ms int) string {
	return fmt.Sprintf("%02d:%02d", ms/minuteMS, ms%minuteMS/1000)
}
