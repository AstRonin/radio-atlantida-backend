package playrules

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAllowsAt(t *testing.T) {
	t.Parallel()
	at := func(s string) TimeOfDay {
		v, err := ParseTimeOfDay(s)
		require.NoError(t, err)
		return v
	}
	window := func(from, to string, days ...int) RuleInput {
		r := RuleInput{WindowFrom: new(at(from)), WindowTo: new(at(to))}
		if days != nil {
			r.Weekdays = days
		}
		return r
	}

	tests := []struct {
		name string
		rule RuleInput
		day  int
		t    string
		want bool
	}{
		{"no window or weekdays", RuleInput{MaxPerHour: new(1)}, 3, "04:00", true},
		{"inside window", window("06:00", "08:00"), 1, "07:59", true},
		{"window start included", window("06:00", "08:00"), 1, "06:00", true},
		{"window end excluded", window("06:00", "08:00"), 1, "08:00", false},
		{"before window", window("06:00", "08:00"), 1, "05:59", false},
		{"weekday listed", window("06:00", "08:00", 1, 2), 2, "07:00", true},
		{"weekday not listed", window("06:00", "08:00", 1, 2), 3, "07:00", false},
		{"weekdays only", RuleInput{Weekdays: []int{6, 7}}, 7, "12:00", true},
		{"weekdays only, other day", RuleInput{Weekdays: []int{6, 7}}, 5, "12:00", false},
		{"crossing midnight, evening part", window("22:00", "02:00", 5), 5, "23:00", true},
		{"crossing midnight, morning part next day", window("22:00", "02:00", 5), 6, "01:00", true},
		{"crossing midnight, morning part of the start day", window("22:00", "02:00", 5), 5, "01:00", false},
		{"crossing midnight, Sunday into Monday", window("22:00", "02:00", 7), 1, "00:30", true},
		{"crossing midnight, outside", window("22:00", "02:00"), 3, "12:00", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.rule.AllowsAt(tt.day, at(tt.t)))
		})
	}
}
