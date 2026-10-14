package clocks

import (
	"errors"
	"slices"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Inserted is a clock's elements with a one-time element for a single hour
// (a rescheduled missed item, §7.1, §7.2).
type Inserted struct {
	// Elements are the clock's elements with the insert in place and
	// without the fixed elements it replaced.
	Elements []Element
	// Index is the insert's index in Elements.
	Index int
	// Origin maps an index of Elements to the index of the same element in
	// the clock; -1 for the insert.
	Origin []int
	// Replaced are the fixed elements the insert replaced.
	Replaced []Element
	Layout   Layout
}

// WithInsert places a one-time element among a clock's elements, by the
// rules of a save (§7.2), and builds the layout. The clock template is not
// changed.
//
//   - A start element goes after the clock's start elements, an end
//     element after its end elements.
//   - A fixed element replaces every fixed element whose start window
//     overlaps its own, at the place of the first; otherwise it goes after
//     the flow elements that fit before its target start.
//
// ins carries the placement, the fixed minute and tolerance, and its
// nominal duration; its ID is 0. Refusals are field errors addressed to
// the request: placement, fixed_minute, fixed_tolerance_ms.
//
// A segment the insert would hold keeps the holder it had without it, so
// the insert never changes how a segment is filled.
func WithInsert(elements []Element, ins Element) (Inserted, error) {
	if err := checkInsert(ins); err != nil {
		return Inserted{}, err
	}
	out := Inserted{}
	var kept []Element
	var origin []int
	at := -1
	for i, e := range elements {
		if ins.Placement == PlacementFixed && e.Placement == PlacementFixed && overlaps(e, ins) {
			out.Replaced = append(out.Replaced, e)
			if at < 0 {
				at = len(kept)
			}
			continue
		}
		kept = append(kept, e)
		origin = append(origin, i)
	}
	if at < 0 {
		at = insertAt(kept, ins)
	}
	out.Elements = slices.Insert(kept, at, ins)
	out.Origin = slices.Insert(origin, at, -1)
	out.Index = at

	l, err := LayoutOf(out.Elements)
	if err != nil {
		return Inserted{}, insertRefusal(ins, err)
	}
	for k := range l.Segments {
		s := &l.Segments[k]
		if s.Holder != at {
			continue
		}
		if j := slices.Index(s.Elements, at); j > 0 {
			s.Holder = s.Elements[j-1]
		} else if k > 0 {
			s.Holder = *l.Segments[k-1].ClosedBy
		}
	}
	out.Layout = l
	return out, nil
}

// insertAt returns where a start, end, or (not replacing) fixed element
// goes among the elements.
func insertAt(elements []Element, ins Element) int {
	switch ins.Placement {
	case PlacementStart:
		last := -1
		for i, e := range elements {
			if e.Placement == PlacementStart {
				last = i
			}
		}
		return last + 1
	case PlacementEnd:
		return len(elements)
	}
	// Fixed: after every fixed element with an earlier target and after the
	// flow elements that end, on nominal durations, by the insert's latest
	// start; before the rest.
	latest := ins.target() + ins.FixedToleranceMS
	cursor, at, full := 0, 0, false
	for i, e := range elements {
		switch e.Placement {
		case PlacementFixed:
			if e.target() > ins.target() {
				return at
			}
			cursor, at, full = max(cursor, e.earliest())+e.NominalDurationMS, i+1, false
		case PlacementStart:
			// The start elements open the first segment wherever they are;
			// the insert stays after those that lead the list.
			cursor += e.NominalDurationMS
			if at == i {
				at = i + 1
			}
		case PlacementFlow:
			if full || cursor+e.NominalDurationMS > latest {
				full = true // this and the next flow elements follow the insert
				continue
			}
			cursor += e.NominalDurationMS
			at = i + 1
		case PlacementEnd:
			// Placed at the end of the hour wherever they are.
		}
	}
	return at
}

func checkInsert(ins Element) error {
	errs := &validation.Error{}
	switch ins.Placement {
	case PlacementStart, PlacementEnd:
		if ins.FixedMinute != nil {
			errs.Add("fixed_minute", validation.Invalid, "only a fixed placement has a fixed minute")
		}
	case PlacementFixed:
		switch {
		case ins.FixedMinute == nil:
			errs.Add("fixed_minute", validation.Required, "a fixed placement needs its minute")
		case *ins.FixedMinute < 0 || *ins.FixedMinute > 59:
			errs.Add("fixed_minute", validation.Range, "must be between 0 and 59")
		case ins.FixedToleranceMS < 0:
			errs.Add("fixed_tolerance_ms", validation.Range, "must not be negative")
		case ins.target()-ins.FixedToleranceMS < 0 || ins.target()+ins.FixedToleranceMS > HourMS:
			errs.Add("fixed_minute", validation.DoesNotFit,
				"the start window :%02d ± %d s falls outside the hour", *ins.FixedMinute, ins.FixedToleranceMS/1000)
		case ins.earliest()+ins.NominalDurationMS > HourMS:
			errs.Add("fixed_minute", validation.DoesNotFit, "from its earliest start the item runs past the end of the hour")
		}
	default:
		errs.Add("placement", validation.Invalid, "must be start, end, or fixed")
	}
	return errs.Err()
}

// insertRefusal turns a layout refusal into a refusal of the request: the
// hour cannot hold the insert at that place.
func insertRefusal(ins Element, err error) error {
	field := "placement"
	if ins.Placement == PlacementFixed {
		field = "fixed_minute"
	}
	msg := "the hour cannot hold the item there"
	var v *validation.Error
	if errors.As(err, &v) && len(v.Fields) > 0 {
		msg += ": " + v.Fields[0].Message
	}
	return validation.Field(field, validation.DoesNotFit, "%s", msg)
}
