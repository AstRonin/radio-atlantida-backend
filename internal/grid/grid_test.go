package grid

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func dayPtr(s string) *time.Time { return new(day(s)) }

func TestRemainders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		from          string
		to            *time.Time
		nFrom         string
		nTo           *time.Time
		before, after *DateRange
	}{
		{
			name: "covered completely", from: "2026-10-20", to: dayPtr("2026-10-25"),
			nFrom: "2026-10-20", nTo: dayPtr("2026-10-31"),
		},
		{
			name: "open slot, open new range from the same day", from: "2026-10-20",
			nFrom: "2026-10-20",
		},
		{
			name: "keeps the dates before", from: "2026-10-01", to: dayPtr("2026-10-25"),
			nFrom: "2026-10-20", nTo: dayPtr("2026-10-31"),
			before: &DateRange{From: day("2026-10-01"), To: dayPtr("2026-10-19")},
		},
		{
			name: "keeps the dates after", from: "2026-10-20", to: dayPtr("2026-11-30"),
			nFrom: "2026-10-15", nTo: dayPtr("2026-10-31"),
			after: &DateRange{From: day("2026-11-01"), To: dayPtr("2026-11-30")},
		},
		{
			name: "open slot split around a closed range", from: "2026-10-01",
			nFrom: "2026-10-20", nTo: dayPtr("2026-10-20"),
			before: &DateRange{From: day("2026-10-01"), To: dayPtr("2026-10-19")},
			after:  &DateRange{From: day("2026-10-21")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := Remainders(day(tt.from), tt.to, day(tt.nFrom), tt.nTo)
			require.Equal(t, tt.before, r.Before)
			require.Equal(t, tt.after, r.After)
		})
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()
	yerevan, err := time.LoadLocation("Asia/Yerevan")
	require.NoError(t, err)
	// 2026-10-19 is a Monday.
	slots := []Slot{
		{ID: 1, Weekday: 1, Hour: 8, Clock: ClockRef{ID: 10}, ValidFrom: day("2026-10-01")},
		{ID: 2, Weekday: 2, Hour: 0, Clock: ClockRef{ID: 11}, ValidFrom: day("2026-10-21"), ValidTo: dayPtr("2026-10-31")},
		{ID: 3, Weekday: 2, Hour: 0, Clock: ClockRef{ID: 12}, ValidFrom: day("2026-10-01"), ValidTo: dayPtr("2026-10-20")},
	}
	got := resolve(slots, day("2026-10-19"), day("2026-10-20"), yerevan)
	require.Equal(t, []ScheduledHour{
		{Date: day("2026-10-19"), Weekday: 1, Hour: 8, StartUTC: time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC), SlotID: 1, Clock: ClockRef{ID: 10}},
		{Date: day("2026-10-20"), Weekday: 2, Hour: 0, StartUTC: time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC), SlotID: 3, Clock: ClockRef{ID: 12}},
	}, got)
}

func TestResolveSkipsDSTGap(t *testing.T) {
	t.Parallel()
	berlin, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)
	// 2026-03-29 (Sunday): 02:00 does not exist in Berlin.
	slots := []Slot{{ID: 1, Weekday: 7, Hour: 2, ValidFrom: day("2026-03-01")}}
	require.Empty(t, resolve(slots, day("2026-03-29"), day("2026-03-29"), berlin))
	require.Len(t, resolve(slots, day("2026-03-22"), day("2026-03-22"), berlin), 1)
}

func TestValidate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 21, 0, 0, 0, time.UTC) // 2026-10-15 01:00 in Yerevan
	yerevan, err := time.LoadLocation("Asia/Yerevan")
	require.NoError(t, err)
	s := &Service{now: func() time.Time { return now }}
	set := Settings{Location: yerevan, HorizonDays: 30}

	tests := []struct {
		name  string
		in    AssignInput
		field string
	}{
		{"today in station time is allowed", AssignInput{Weekdays: []int{1}, Hours: []int{8}, ValidFrom: day("2026-10-15")}, ""},
		{"yesterday in station time", AssignInput{Weekdays: []int{1}, Hours: []int{8}, ValidFrom: day("2026-10-14")}, "valid_from"},
		{"past the horizon", AssignInput{Weekdays: []int{1}, Hours: []int{8}, ValidFrom: day("2026-11-15")}, "valid_from"},
		{"end before start", AssignInput{Weekdays: []int{1}, Hours: []int{8}, ValidFrom: day("2026-10-20"), ValidTo: dayPtr("2026-10-19")}, "valid_to"},
		{"no weekdays", AssignInput{Hours: []int{8}, ValidFrom: day("2026-10-20")}, "weekdays"},
		{"hour out of range", AssignInput{Weekdays: []int{1}, Hours: []int{24}, ValidFrom: day("2026-10-20")}, "hours"},
		{"weekday twice", AssignInput{Weekdays: []int{3, 3}, Hours: []int{8}, ValidFrom: day("2026-10-20")}, "weekdays"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.validate(tt.in, set)
			if tt.field == "" {
				require.NoError(t, err)
				return
			}
			var verr *validation.Error
			require.True(t, errors.As(err, &verr), "%v", err)
			require.Equal(t, tt.field, verr.Fields[0].Field)
		})
	}
}

func TestValidateWeekdaysFallInRange(t *testing.T) {
	t.Parallel()
	// Fixed clock: 2026-10-15 01:00 in Yerevan. 2026-10-19 is a Monday, so
	// 2026-10-29 is a Thursday and 2026-10-20 to 2026-10-26 is a full week.
	now := time.Date(2026, 10, 14, 21, 0, 0, 0, time.UTC)
	yerevan, err := time.LoadLocation("Asia/Yerevan")
	require.NoError(t, err)
	s := &Service{now: func() time.Time { return now }}
	set := Settings{Location: yerevan, HorizonDays: 30}

	tests := []struct {
		name     string
		weekdays []int
		from     string
		to       *time.Time
		refused  bool
	}{
		{name: "one day on a non-matching weekday is refused", weekdays: []int{1}, from: "2026-10-29", to: dayPtr("2026-10-29"), refused: true},
		{name: "one day on the matching weekday is accepted", weekdays: []int{4}, from: "2026-10-29", to: dayPtr("2026-10-29")},
		{name: "a week covers every weekday", weekdays: []int{1}, from: "2026-10-20", to: dayPtr("2026-10-26")},
		{name: "open-ended range is accepted", weekdays: []int{1}, from: "2026-10-29"},
		{name: "partial match is accepted", weekdays: []int{1, 4}, from: "2026-10-29", to: dayPtr("2026-10-29")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := AssignInput{Weekdays: tt.weekdays, Hours: []int{8}, ValidFrom: day(tt.from), ValidTo: tt.to}
			_, err := s.validate(in, set)
			if !tt.refused {
				require.NoError(t, err)
				return
			}
			var verr *validation.Error
			require.True(t, errors.As(err, &verr), "%v", err)
			require.Len(t, verr.Fields, 1)
			require.Equal(t, "weekdays", verr.Fields[0].Field)
			require.Equal(t, validation.Invalid, verr.Fields[0].Rule)
			require.Equal(t, "none of the chosen weekdays falls between valid_from and valid_to", verr.Fields[0].Message)
		})
	}
}
