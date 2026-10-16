package rotation

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

func TestCheckPlacement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		ins    Insert
		length int
		field  string // "-" when valid
		rule   validation.Rule
	}{
		{"fixed", Insert{Placement: PlacementFixed, OffsetMS: new(30 * mn), ToleranceMS: mn}, 5 * mn, "-", ""},
		{"start", Insert{Placement: PlacementStart, ToleranceMS: mn}, 5 * mn, "-", ""},
		{"end", Insert{Placement: PlacementEnd}, 5 * mn, "-", ""},
		{"fixed fits from the earliest start", Insert{Placement: PlacementFixed, OffsetMS: new(56 * mn), ToleranceMS: mn}, 5 * mn, "-", ""},
		{"fixed without time", Insert{Placement: PlacementFixed}, mn, "offset_ms", validation.Required},
		{"start with time", Insert{Placement: PlacementStart, OffsetMS: new(0)}, mn, "offset_ms", validation.Invalid},
		{"not whole seconds", Insert{Placement: PlacementFixed, OffsetMS: new(1500)}, mn, "offset_ms", validation.Format},
		{"past 59:59", Insert{Placement: PlacementFixed, OffsetMS: new(60 * mn)}, mn, "offset_ms", validation.Range},
		{"runs past the hour", Insert{Placement: PlacementFixed, OffsetMS: new(58 * mn), ToleranceMS: 0}, 3 * mn, "offset_ms", validation.DoesNotFit},
		{"longer than the hour", Insert{Placement: PlacementEnd}, 61 * mn, "placement", validation.DoesNotFit},
		{"tolerance", Insert{Placement: PlacementStart, ToleranceMS: 601 * sec}, mn, "tolerance_ms", validation.Range},
		{"placement", Insert{Placement: "middle"}, mn, "placement", validation.Invalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkPlacement(tt.ins, tt.length)
			if tt.field == "-" {
				require.NoError(t, err)
				return
			}
			var ve *validation.Error
			require.ErrorAs(t, err, &ve)
			require.Equal(t, tt.field, ve.Fields[0].Field, ve.Error())
			require.Equal(t, tt.rule, ve.Fields[0].Rule, ve.Error())
		})
	}
}

func TestInsertErrorReason(t *testing.T) {
	t.Parallel()
	err := error(&InsertError{Reason: ReasonBlocked, Message: "the track breaks a play rule at 10:30:00"})
	var r interface{ FailureReason() string }
	require.ErrorAs(t, err, &r)
	require.Equal(t, "insert_blocked", r.FailureReason())
	require.Contains(t, err.Error(), "10:30:00")
}
