package clocks

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

const minute = minuteMS

func flow(minutes int) Element {
	return Element{
		Placement: PlacementFlow, NominalDurationMS: minutes * minute, DurationToleranceMS: 30000,
		Filler: Filler{Mode: FillerNone}, PlaylistIDs: []uint64{1},
	}
}

func placed(p Placement, minutes int) Element {
	e := flow(minutes)
	e.Placement = p
	return e
}

func fixed(at, minutes int) Element {
	e := flow(minutes)
	e.Placement = PlacementFixed
	e.FixedMinute = new(at)
	e.FixedToleranceMS = minute
	return e
}

func withID(e Element, id uint64) Element {
	e.ID = id
	return e
}

func withFiller(e Element, m FillerMode) Element {
	e.Filler = Filler{Mode: m}
	return e
}

func TestArrangeLayout(t *testing.T) {
	t.Parallel()

	type seg struct {
		start, end, content, shortfall, overflow int
		elements                                 []int
		holder                                   int
		fixedStart                               *int
	}
	tests := []struct {
		name     string
		elements []Element
		want     []seg
		status   Status
	}{
		{
			name:     "full hour of flow elements",
			elements: []Element{flow(20), flow(20), flow(20)},
			want:     []seg{{end: HourMS, content: HourMS, elements: []int{0, 1, 2}, holder: 2}},
			status:   StatusComplete,
		},
		{
			name:     "short hour needs a filler",
			elements: []Element{flow(19), flow(19), flow(19)},
			want:     []seg{{end: HourMS, content: 57 * minute, shortfall: 3 * minute, elements: []int{0, 1, 2}, holder: 2}},
			status:   StatusIncomplete,
		},
		{
			name:     "short hour with a filler on the holder",
			elements: []Element{flow(19), flow(19), withFiller(flow(19), FillerRandom)},
			want:     []seg{{end: HourMS, content: 57 * minute, shortfall: 3 * minute, elements: []int{0, 1, 2}, holder: 2}},
			status:   StatusComplete,
		},
		{
			name:     "overflow past the hour is not short",
			elements: []Element{flow(21), flow(21), flow(21)},
			want:     []seg{{end: HourMS, content: 63 * minute, overflow: 3 * minute, elements: []int{0, 1, 2}, holder: 2}},
			status:   StatusComplete,
		},
		{
			name:     "fixed element splits the hour; shortfall counts from its earliest start",
			elements: []Element{flow(14), flow(14), fixed(30, 5), flow(12), flow(12)},
			want: []seg{
				{end: 30 * minute, content: 28 * minute, shortfall: minute, elements: []int{0, 1}, holder: 1, fixedStart: new(29 * minute)},
				{start: 34 * minute, end: HourMS, content: 24 * minute, shortfall: 2 * minute, elements: []int{3, 4}, holder: 4},
			},
			status: StatusIncomplete,
		},
		{
			name:     "fixed element absorbs a small gap within its tolerance",
			elements: []Element{flow(29), withFiller(fixed(30, 5), FillerNone), flow(26)},
			want: []seg{
				{end: 30 * minute, content: 29 * minute, elements: []int{0}, holder: 0, fixedStart: new(29 * minute)},
				{start: 34 * minute, end: HourMS, content: 26 * minute, elements: []int{2}, holder: 2},
			},
			status: StatusComplete,
		},
		{
			name:     "start and end elements go to the first and last segments",
			elements: []Element{flow(10), placed(PlacementEnd, 3), fixed(30, 5), placed(PlacementStart, 2), flow(10)},
			want: []seg{
				{end: 30 * minute, content: 12 * minute, shortfall: 17 * minute, elements: []int{3, 0}, holder: 0, fixedStart: new(29 * minute)},
				{start: 34 * minute, end: HourMS, content: 13 * minute, shortfall: 13 * minute, elements: []int{4, 1}, holder: 1},
			},
			status: StatusIncomplete,
		},
		{
			name:     "empty segments hold their filler on the fixed element",
			elements: []Element{withFiller(fixed(30, 5), FillerAsk)},
			want: []seg{
				{end: 30 * minute, shortfall: 29 * minute, holder: 0, fixedStart: new(29 * minute)},
				{start: 34 * minute, end: HourMS, shortfall: 26 * minute, holder: 0},
			},
			status: StatusComplete,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			arr, err := Arrange(tt.elements, nil)
			require.NoError(t, err)
			require.Len(t, arr.Layout.Segments, len(tt.want))
			total := 0
			for k, w := range tt.want {
				s := arr.Layout.Segments[k]
				require.Equal(t, k, s.Index)
				require.Equal(t, w.start, s.StartMS, "segment %d start", k)
				require.Equal(t, w.end, s.EndMS, "segment %d end", k)
				require.Equal(t, w.content, s.ContentMS, "segment %d content", k)
				require.Equal(t, w.shortfall, s.ShortfallMS, "segment %d shortfall", k)
				require.Equal(t, w.overflow, s.OverflowMS, "segment %d overflow", k)
				require.Equal(t, w.elements, s.Elements, "segment %d elements", k)
				require.Equal(t, w.holder, s.Holder, "segment %d holder", k)
				require.Equal(t, w.fixedStart, s.FixedStartMS, "segment %d fixed start", k)
				total += w.shortfall
			}
			require.Equal(t, total, arr.Layout.ShortfallMS)
			require.Equal(t, tt.status, arr.Layout.Status(arr.Elements))
		})
	}
}

func TestArrangeRefusals(t *testing.T) {
	t.Parallel()
	noPlaylists := flow(10)
	noPlaylists.PlaylistIDs = nil
	noMinute := fixed(30, 5)
	noMinute.FixedMinute = nil
	playlistNoSource := flow(10)
	playlistNoSource.Filler = Filler{Mode: FillerPlaylist}

	tests := []struct {
		name     string
		elements []Element
		existing map[uint64]bool
		field    string
		rule     validation.Rule
	}{
		{"no elements", nil, nil, "elements", validation.Required},
		{"element without playlists", []Element{flow(10), noPlaylists}, nil, "elements[1].playlist_ids", validation.Required},
		{"fixed without minute", []Element{noMinute}, nil, "elements[0].fixed_minute", validation.Required},
		{"filler playlist without source", []Element{playlistNoSource}, nil, "elements[0].filler_source_id", validation.Required},
		{"fixed window before the hour", []Element{fixed(0, 5)}, nil, "elements[0].fixed_minute", validation.DoesNotFit},
		{"fixed element past the hour", []Element{fixed(58, 4)}, nil, "elements[0].nominal_duration_ms", validation.DoesNotFit},
		{"unknown element ID", []Element{withID(flow(10), 9)}, map[uint64]bool{1: true}, "elements[0].id", validation.UnknownID},
		{"material before a fixed element does not fit", []Element{flow(20), flow(12), fixed(30, 5)}, nil, "elements[2].fixed_minute", validation.DoesNotFit},
		{"start elements do not fit before a fixed element", []Element{flow(1), placed(PlacementStart, 7), fixed(5, 5)}, nil, "elements[1].nominal_duration_ms", validation.DoesNotFit},
		{"end elements do not fit the hour", []Element{fixed(30, 5), placed(PlacementEnd, 20), placed(PlacementEnd, 7)}, nil, "elements[2].nominal_duration_ms", validation.DoesNotFit},
		{"two new fixed windows overlap", []Element{fixed(30, 2), fixed(31, 2)}, nil, "elements[1].fixed_minute", validation.Overlap},
		{"two existing fixed windows overlap", []Element{withID(fixed(30, 2), 1), withID(fixed(31, 2), 2)}, map[uint64]bool{1: true, 2: true}, "elements[1].fixed_minute", validation.Overlap},
		{"fixed elements out of minute order", []Element{fixed(40, 2), fixed(20, 2)}, nil, "elements[1].fixed_minute", validation.Order},
		{"fixed element runs into the next one", []Element{fixed(10, 25), fixed(30, 5)}, nil, "elements[1].fixed_minute", validation.DoesNotFit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Arrange(tt.elements, tt.existing)
			var verr *validation.Error
			require.True(t, errors.As(err, &verr), "%v", err)
			require.Contains(t, verr.Fields, validation.FieldError{Field: tt.field, Rule: tt.rule, Message: fieldMessage(verr, tt.field)}, "%v", verr)
		})
	}
}

// fieldMessage returns the message of field in err, so the refusal tests
// match field and rule without pinning the English text.
func fieldMessage(err *validation.Error, field string) string {
	for _, f := range err.Fields {
		if f.Field == field {
			return f.Message
		}
	}
	return ""
}

func TestArrangeReplacesFixed(t *testing.T) {
	t.Parallel()
	existing := withID(fixed(30, 2), 7)
	arr, err := Arrange([]Element{withID(flow(28), 3), existing, fixed(31, 2), flow(26)}, map[uint64]bool{3: true, 7: true})
	require.NoError(t, err)
	require.Equal(t, []Element{existing}, arr.Replaced)
	require.Len(t, arr.Elements, 3)
	require.Equal(t, 31, *arr.Elements[1].FixedMinute)
	require.Len(t, arr.Layout.Segments, 2)
}

func TestTouchingWindowsDoNotOverlap(t *testing.T) {
	t.Parallel()
	_, err := Arrange([]Element{fixed(30, 1), fixed(32, 1)}, nil)
	require.NoError(t, err)
}

func TestApplyChoices(t *testing.T) {
	t.Parallel()
	elements := []Element{fixed(30, 5)}
	l, err := LayoutOf(elements)
	require.NoError(t, err)
	require.Len(t, l.Uncovered(elements), 2)

	// Both empty segments are held by the fixed element.
	err = applyChoices(elements, l, []SegmentChoice{
		{Segment: 0, Filler: Filler{Mode: FillerRandom}},
		{Segment: 1, Filler: Filler{Mode: FillerAsk}},
	})
	var verr *validation.Error
	require.True(t, errors.As(err, &verr))
	require.Equal(t, "segments[1].segment", verr.Fields[0].Field)

	elements = []Element{fixed(30, 5)}
	require.NoError(t, applyChoices(elements, l, []SegmentChoice{
		{Segment: 0, Filler: Filler{Mode: FillerRandom}},
		{Segment: 1, Filler: Filler{Mode: FillerRandom}},
	}))
	require.Empty(t, l.Uncovered(elements))
	require.Equal(t, StatusComplete, l.Status(elements))

	err = applyChoices(elements, l, []SegmentChoice{{Segment: 2, Filler: Filler{Mode: FillerRandom}}})
	require.True(t, errors.As(err, &verr))
	require.Equal(t, validation.Range, verr.Fields[0].Rule)
}
