//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
)

// storedCounts reads what a simulation must not change: the built hours,
// the scheduled items, the one-time inserts, and the newest build time.
func (a *libraryAPI) storedCounts() [4]int64 {
	a.t.Helper()
	var out [4]int64
	var built time.Time
	require.NoError(a.t, a.db.Pool.QueryRow(a.t.Context(), `
SELECT (SELECT count(*) FROM plan_hours), (SELECT count(*) FROM scheduled_items),
       (SELECT count(*) FROM hour_inserts), COALESCE((SELECT max(built_at) FROM plan_hours), 'epoch')`).
		Scan(&out[0], &out[1], &out[2], &built))
	out[3] = built.UnixNano()
	return out
}

// TestRotationSimulate builds the two grid hours of tomorrow in the plan and
// in a simulation: both give the same items, so the simulation threads the
// plays of the first hour into the second as the planner does. Nothing is
// stored.
func TestRotationSimulate(t *testing.T) {
	f := newClockFixture(t)
	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", f.musicClock("Day"), &saved)
	date, weekday, start := tomorrowNoon(t)
	var assigned dto.GridAssignResult
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": saved.Clock.ID, "weekdays": []int{weekday}, "hours": []int{12, 13}, "valid_from": date, "valid_to": date,
	}, &assigned)
	f.waitOperation(assigned.BuildOperationID)

	planned := f.plan()
	require.Len(t, planned.Items, 2)
	stored := f.storedCounts()

	const path = "/api/rotation/simulate"
	query := fmt.Sprintf("%s?from=%s&to=%s", path, date, date)

	// The default: the summary, no items.
	var sim dto.TrackSimulation
	f.ok(http.StatusOK, http.MethodGet, query, nil, &sim)
	require.Equal(t, date, sim.From)
	require.Equal(t, date, sim.To)
	require.Equal(t, 2, sim.HoursBuilt)
	require.Zero(t, sim.HoursSkipped)
	require.Len(t, sim.Hours, 2)
	require.Equal(t, start, sim.Hours[0].HourStartUTC)
	require.Equal(t, saved.Clock.ID, sim.Hours[0].Clock.ID)
	require.Nil(t, sim.Hours[0].Items, "items only with items=true")
	require.Positive(t, sim.Hours[0].ItemCount)
	require.Equal(t, sim.Items, sim.Hours[0].ItemCount+sim.Hours[1].ItemCount)
	require.Zero(t, sim.HistoryPlays, "nothing played before the range")
	require.Equal(t, 4, sim.LibraryTracks, "the snapshot: the songs of the clock's playlist; the jingle is in no playlist of the clock")
	require.Positive(t, sim.Queries)
	require.GreaterOrEqual(t, sim.ElapsedMS, int64(0))

	// With items: the same items as the plan, hour by hour.
	var withItems dto.TrackSimulation
	f.ok(http.StatusOK, http.MethodGet, query+"&items=true", nil, &withItems)
	require.Len(t, withItems.Hours, len(planned.Items))
	for i, h := range withItems.Hours {
		want := planned.Items[i].Items
		require.Equal(t, planned.Items[i].HourStartUTC, h.HourStartUTC)
		require.Len(t, h.Items, len(want), "hour %d", i)
		require.Equal(t, len(want), h.ItemCount)
		for j, it := range h.Items {
			require.Equal(t, want[j].TrackID, it.TrackID, "hour %d item %d", i, j)
			require.Equal(t, want[j].PlannedStartUTC, it.StartUTC, "hour %d item %d", i, j)
			require.Equal(t, want[j].PlannedInMS, it.PlannedInMS, "hour %d item %d", i, j)
			require.Equal(t, want[j].PlannedOutMS, it.PlannedOutMS, "hour %d item %d", i, j)
			require.Equal(t, want[j].Source, it.Source, "hour %d item %d", i, j)
		}
	}
	require.Equal(t, stored, f.storedCounts(), "the simulation stores nothing")

	// Ranges: to before from, more than 31 days, and bad dates are refused
	// per field.
	f.fieldError("query.to", errcodes.Range, http.MethodGet, path+"?from=2026-11-02&to=2026-11-01", nil)
	f.fieldError("query.to", errcodes.Range, http.MethodGet, path+"?from=2026-11-01&to=2026-12-02", nil)
	f.fieldError("query.from", errcodes.Format, http.MethodGet, path+"?from=2026-13-01&to=2026-12-01", nil)
	// Exactly 31 days is allowed.
	f.ok(http.StatusOK, http.MethodGet, path+"?from=2026-11-01&to=2026-12-01", nil, &sim)
	require.Zero(t, sim.HoursBuilt, "no grid hour in the empty range")
}
