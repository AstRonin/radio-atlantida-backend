//go:build integration

package rotation_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

// startedLead is the lead of a partial build (rotation.startedHourLead).
const startedLead = 10 * time.Second

// startedStart is the hour on air at fixtureNow.
var startedStart = fixtureNow.Truncate(time.Hour)

func planHourCount(t *testing.T, d *db.DB) int {
	t.Helper()
	var n int
	require.NoError(t, d.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM plan_hours WHERE hour_start_utc = $1`, startedStart).Scan(&n))
	return n
}

// requireRemainderOnly requires one stored hour with items only in what was
// left of the hour at fixtureNow.
func requireRemainderOnly(t *testing.T, d *db.DB, plan []rotation.PlannedHour) {
	t.Helper()
	require.Equal(t, 1, planHourCount(t, d))
	require.Len(t, plan, 1)
	require.NotEmpty(t, plan[0].Items)
	for _, it := range plan[0].Items {
		require.False(t, it.Start.Before(fixtureNow.Add(startedLead)), "item at %s is before the remainder", it.Start)
		require.True(t, it.Start.Before(startedStart.Add(time.Hour)), "item at %s is after the hour", it.Start)
	}
}

// A clock assigned to the hour on air gives the hour a plan for its
// remainder, once.
func TestAssignIntoAStartedHourBuildsItsRemainder(t *testing.T) {
	d := dbtest.Migrated(t)
	seedSixMonthFixture(t, d)
	// fixtureNow is Thursday 04:30 in the station's time. The hour starts with
	// no clock on the grid.
	_, err := d.Pool.Exec(t.Context(), `DELETE FROM grid_slots WHERE weekday = 4 AND hour = 4`)
	require.NoError(t, err)
	svc, _ := fixtureServices(t, d)
	ctx := t.Context()
	span := rotation.Range{From: startedStart, To: startedStart.Add(time.Hour)}

	_, err = svc.RunJob(ctx)
	require.NoError(t, err)
	require.Zero(t, planHourCount(t, d), "off the grid: nothing to build")

	_, err = d.Pool.Exec(ctx, `INSERT INTO grid_slots (weekday, hour, clock_id, valid_from)
SELECT 4, 4, id, date '2026-10-15' FROM clocks WHERE name = 'Day'`)
	require.NoError(t, err)
	_, err = svc.RunJob(ctx)
	require.NoError(t, err)

	plan, err := svc.Plan(ctx, span)
	require.NoError(t, err)
	requireRemainderOnly(t, d, plan)

	_, err = svc.RunJob(ctx)
	require.NoError(t, err)
	again, err := svc.Plan(ctx, span)
	require.NoError(t, err)
	require.Equal(t, plan[0].BuiltAt, again[0].BuiltAt, "built once")
	require.Equal(t, plan[0].Items, again[0].Items)
}

// Concurrent syncs build the hour once, under the plan lock.
func TestConcurrentSyncsBuildAStartedHourOnce(t *testing.T) {
	d := dbtest.Migrated(t)
	seedSixMonthFixture(t, d)
	svc, _ := fixtureServices(t, d)
	ctx := t.Context()

	var g errgroup.Group
	for range 4 {
		g.Go(func() error {
			_, err := svc.RunJob(ctx)
			return err
		})
	}
	require.NoError(t, g.Wait())

	plan, err := svc.Plan(ctx, rotation.Range{From: startedStart, To: startedStart.Add(time.Hour)})
	require.NoError(t, err)
	requireRemainderOnly(t, d, plan)
	starts := map[time.Time]bool{}
	for _, it := range plan[0].Items {
		require.False(t, starts[it.Start], "one build wrote the items")
		starts[it.Start] = true
	}
}
