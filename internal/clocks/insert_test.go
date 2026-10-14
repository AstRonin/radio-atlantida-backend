package clocks

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

func insert(p Placement, minutes int) Element {
	e := placed(p, minutes)
	e.PlaylistIDs, e.DurationToleranceMS = nil, 0
	return e
}

func insertFixed(at, minutes int) Element {
	e := fixed(at, minutes)
	e.PlaylistIDs, e.DurationToleranceMS = nil, 0
	return e
}

func TestWithInsertPlacement(t *testing.T) {
	t.Parallel()
	start := withID(placed(PlacementStart, 1), 1)
	f1, f2, f3 := withID(flow(10), 2), withID(flow(10), 3), withID(flow(10), 4)
	end := withID(placed(PlacementEnd, 1), 5)
	elements := []Element{start, f1, f2, f3, end}

	tests := []struct {
		name   string
		ins    Element
		index  int
		origin []int
	}{
		{"start goes after the start elements", insert(PlacementStart, 3), 1, []int{0, -1, 1, 2, 3, 4}},
		{"end goes last", insert(PlacementEnd, 3), 5, []int{0, 1, 2, 3, 4, -1}},
		// :20 ± 1 min: start (1) + 10 + 10 = 21 ends by 21 → after f2.
		{"fixed after the flow that fits before it", insertFixed(20, 3), 3, []int{0, 1, 2, -1, 3, 4}},
		{"fixed at :05 before the flow that does not fit", insertFixed(5, 3), 1, []int{0, -1, 1, 2, 3, 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, err := WithInsert(elements, tt.ins)
			require.NoError(t, err)
			require.Equal(t, tt.index, r.Index)
			require.Equal(t, tt.origin, r.Origin)
			require.Empty(t, r.Replaced)
			for _, s := range r.Layout.Segments {
				require.NotEqual(t, r.Index, s.Holder, "the insert never holds a segment that has other elements")
			}
		})
	}
}

func TestWithInsertReplacesOverlappingFixed(t *testing.T) {
	t.Parallel()
	old := withID(fixed(30, 2), 3)
	elements := []Element{withID(flow(20), 1), withID(flow(5), 2), old, withID(flow(20), 4)}

	r, err := WithInsert(elements, insertFixed(31, 3))

	require.NoError(t, err)
	require.Equal(t, []Element{old}, r.Replaced)
	require.Equal(t, 2, r.Index)
	require.Equal(t, []int{0, 1, -1, 3}, r.Origin)
	require.Len(t, r.Layout.Segments, 2)
}

func TestWithInsertRefusals(t *testing.T) {
	t.Parallel()
	elements := []Element{withID(placed(PlacementStart, 50), 1), withID(flow(5), 2)}
	tests := []struct {
		name  string
		ins   Element
		field string
		rule  validation.Rule
	}{
		{"fixed without minute", func() Element { e := insertFixed(10, 1); e.FixedMinute = nil; return e }(), "fixed_minute", validation.Required},
		{"window outside the hour", insertFixed(0, 1), "fixed_minute", validation.DoesNotFit},
		{"minute on a start", func() Element { e := insert(PlacementStart, 1); e.FixedMinute = new(3); return e }(), "fixed_minute", validation.Invalid},
		{"start elements overrun", insert(PlacementStart, 15), "placement", validation.DoesNotFit},
		{"fixed after content that runs past it", insertFixed(30, 2), "fixed_minute", validation.DoesNotFit},
		{"unknown placement", insert(PlacementFlow, 1), "placement", validation.Invalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := WithInsert(elements, tt.ins)
			var v *validation.Error
			require.ErrorAs(t, err, &v)
			require.Equal(t, tt.field, v.Fields[0].Field)
			require.Equal(t, tt.rule, v.Fields[0].Rule)
		})
	}
}
