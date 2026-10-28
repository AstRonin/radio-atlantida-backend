//go:build integration

package library_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// The two lookups behind the track detail's in_use (and behind FileInUse)
// answer for each kind of use: a clock insert, a planned item, an aired item,
// and none.
func TestTrackUsesLookupsForInUse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	clock := f.id(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	unused, inClock, planned, aired := f.track("Unused", nil), f.track("Clock", nil), f.track("Planned", nil), f.track("Aired", nil)
	f.exec(`INSERT INTO clock_inserts (clock_id, offset_ms, track_id) VALUES ($1, 0, $2)`, clock, inClock)
	h := time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)
	f.hour(clock, h)
	f.item(clock, planned, h.Add(10*time.Minute), "planned")
	f.item(clock, aired, h.Add(20*time.Minute), "aired")

	users, err := f.tracks.Users(ctx, unused, library.MaxUsers)
	require.NoError(t, err)
	require.Empty(t, users)
	hours, err := f.tracks.PlannedHours(ctx, unused)
	require.NoError(t, err)
	require.Nil(t, hours)

	users, err = f.tracks.Users(ctx, inClock, library.MaxUsers)
	require.NoError(t, err)
	require.Equal(t, []library.Ref{{Kind: library.KindClock, ID: clock, Name: "Morning"}}, users)

	hours, err = f.tracks.PlannedHours(ctx, planned)
	require.NoError(t, err)
	require.Equal(t, &library.HourRange{From: h, To: h.Add(time.Hour)}, hours)

	users, err = f.tracks.Users(ctx, aired, library.MaxUsers)
	require.NoError(t, err)
	require.Empty(t, users, "an aired item is not a use")
	hours, err = f.tracks.PlannedHours(ctx, aired)
	require.NoError(t, err)
	require.Nil(t, hours, "an aired item is not a use")
}

// MissedHours answers for each resolution of a missed item: an open one
// counts; a rescheduled one (its replacement is a plan item) and a dismissed
// one do not.
func TestTrackMissedLookupForInUse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	start := time.Date(2026, 10, 17, 6, 10, 0, 0, time.UTC)
	outage := f.id(`INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component)
VALUES ($1, $2, 'icecast', 'icecast') RETURNING id`, start.Add(-time.Hour), start.Add(time.Hour))
	open, rescheduled, dismissed, none := f.track("Open", nil), f.track("Rescheduled", nil), f.track("Dismissed", nil), f.track("None", nil)
	missed := func(track uint64, resolution string) {
		t.Helper()
		resolvedAt := "NULL"
		if resolution != "open" {
			resolvedAt = "now()"
		}
		resolvedItem := "NULL"
		if resolution == "rescheduled" {
			resolvedItem = "1" // no FK on resolved_item_id; the check needs it set
		}
		f.exec(`INSERT INTO play_log (track_id, status, planned_start_utc, planned_in_ms, planned_out_ms, truncated, source,
		        outage_id, resolution, resolved_at, resolved_item_id)
		        VALUES ($1, 'missed', $2, 0, 200000, false, 'auto', $3, $4, `+resolvedAt+`, `+resolvedItem+`)`,
			track, start, outage, resolution)
	}
	missed(open, "open")
	missed(rescheduled, "rescheduled")
	missed(dismissed, "dismissed")

	tests := []struct {
		name  string
		track uint64
		want  *library.HourRange
	}{
		{"open missed item", open, &library.HourRange{From: start.Truncate(time.Hour), To: start.Truncate(time.Hour).Add(time.Hour)}},
		{"rescheduled missed item", rescheduled, nil},
		{"dismissed missed item", dismissed, nil},
		{"no missed item", none, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hours, err := f.tracks.MissedHours(ctx, tt.track)
			require.NoError(t, err)
			require.Equal(t, tt.want, hours)
		})
	}
}
