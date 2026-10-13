package playrules

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

func TestParseTimeOfDay(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]TimeOfDay{"00:00": 0, "06:30": 390, "23:59": 1439} {
		got, err := ParseTimeOfDay(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
		require.Equal(t, in, got.String())
	}
	for _, bad := range []string{"", "6:30", "24:00", "12:60", "12:00:00", "noon"} {
		_, err := ParseTimeOfDay(bad)
		require.Error(t, err, bad)
	}
}

func tod(s string) *TimeOfDay {
	t, err := ParseTimeOfDay(s)
	if err != nil {
		panic(err)
	}
	return &t
}

func TestValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    RuleInput
		field string // "-" when valid
		rule  validation.Rule
	}{
		{"window", RuleInput{Name: "Morning", WindowFrom: tod("06:00"), WindowTo: tod("08:00")}, "-", ""},
		{"window across midnight", RuleInput{Name: "Night", WindowFrom: tod("22:00"), WindowTo: tod("02:00")}, "-", ""},
		{"weekdays alone", RuleInput{Name: "Mondays", Weekdays: []int{1}}, "-", ""},
		{"interval alone", RuleInput{Name: "Spaced", MinIntervalMinutes: new(10)}, "-", ""},
		{"per hour alone", RuleInput{Name: "Once", MaxPerHour: new(1)}, "-", ""},
		{"blank name", RuleInput{Name: "  ", MaxPerHour: new(1)}, "name", validation.Required},
		{"no limit", RuleInput{Name: "Nothing"}, "", validation.Required},
		{"only from", RuleInput{Name: "Half", WindowFrom: tod("06:00")}, "window_to", validation.Required},
		{"only to", RuleInput{Name: "Half", WindowTo: tod("06:00")}, "window_from", validation.Required},
		{"empty window", RuleInput{Name: "Empty", WindowFrom: tod("06:00"), WindowTo: tod("06:00")}, "window_to", validation.Invalid},
		{"empty weekdays", RuleInput{Name: "None", Weekdays: []int{}}, "weekdays", validation.Required},
		{"weekday 0", RuleInput{Name: "Bad", Weekdays: []int{0, 1}}, "weekdays", validation.Range},
		{"weekday 8", RuleInput{Name: "Bad", Weekdays: []int{8}}, "weekdays", validation.Range},
		{"weekday twice", RuleInput{Name: "Bad", Weekdays: []int{2, 2}}, "weekdays", validation.Invalid},
		{"interval 0", RuleInput{Name: "Bad", MinIntervalMinutes: new(0)}, "min_interval_minutes", validation.Range},
		{"per hour 0", RuleInput{Name: "Bad", MaxPerHour: new(0)}, "max_per_hour", validation.Range},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Validate(tt.in)
			if tt.field == "-" {
				require.NoError(t, err)
				return
			}
			var ve *validation.Error
			require.ErrorAs(t, err, &ve)
			require.Equal(t, []validation.FieldError{{Field: tt.field, Rule: tt.rule, Message: ve.Fields[0].Message}}, ve.Fields)
		})
	}
}

func TestValidateNormalises(t *testing.T) {
	t.Parallel()
	in := []int{7, 1, 3}
	out, err := Validate(RuleInput{Name: " Weekend ", Weekdays: in, Notes: " note "})
	require.NoError(t, err)
	require.Equal(t, "Weekend", out.Name)
	require.Equal(t, "note", out.Notes)
	require.Equal(t, []int{1, 3, 7}, out.Weekdays)
	require.Equal(t, []int{7, 1, 3}, in, "the caller's slice is not changed")
}
