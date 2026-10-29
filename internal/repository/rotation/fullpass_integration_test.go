//go:build integration

package rotation_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	clocksrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/clocks"
	gridrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/grid"
	operationsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"
	rotationrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/rotation"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
	"github.com/AstRonin/radio-atlantida-backend/internal/workerpool"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// The fixture: six months of grid (183 days, two clocks alternating by hour),
// a library of 2000 tracks by 800 artists, all in one music playlist with the
// station's rotation rule. The plan starts empty.
const (
	fixtureTracks  = 2000
	fixtureArtists = 800
	fixtureDays    = 183
)

// fixtureNow is the instant of the pass. It is not on the hour, so the clock
// moving on by the real time of the test does not move the range across an
// hour boundary.
var fixtureNow = time.Date(2026, 10, 15, 0, 30, 0, 0, time.UTC)

func seedSixMonthFixture(t *testing.T, d *db.DB) {
	t.Helper()
	ctx := t.Context()
	pool := d.Pool
	var music, jingle uint64
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM playlist_types WHERE code = 'music'`).Scan(&music))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM playlist_types WHERE code = 'jingle'`).Scan(&jingle))
	var playlist uint64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO playlists (name, type_id) VALUES ('Music', $1) RETURNING id`, music).Scan(&playlist))

	_, err := pool.Exec(ctx, `INSERT INTO artists (name) SELECT 'Artist ' || g FROM generate_series(1, $1) g`, fixtureArtists)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO tracks (title, artist_id, duration_ms, file_path, type_id)
SELECT 'Track ' || g,
       (SELECT id FROM artists WHERE name = 'Artist ' || (g % $1 + 1)),
       150000 + (g % 90) * 1000,
       'music/track-' || g || '.mp3',
       $2
FROM generate_series(1, $3) g`, fixtureArtists, music, fixtureTracks)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO track_playlists (track_id, playlist_id) SELECT id, $1 FROM tracks`, playlist)
	require.NoError(t, err)

	var day, night uint64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO clocks (name) VALUES ('Day') RETURNING id`).Scan(&day))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO clocks (name) VALUES ('Night') RETURNING id`).Scan(&night))
	for _, c := range []uint64{day, night} {
		_, err = pool.Exec(ctx, `INSERT INTO clock_playlists (clock_id, playlist_id, position) VALUES ($1, $2, 0)`, c, playlist)
		require.NoError(t, err)
	}
	_, err = pool.Exec(ctx, `
INSERT INTO grid_slots (weekday, hour, clock_id, valid_from)
SELECT wd, h, CASE WHEN h < 12 THEN $1::bigint ELSE $2::bigint END, date '2026-10-15'
FROM generate_series(1, 7) wd, generate_series(0, 23) h`, day, night)
	require.NoError(t, err)
}

// fixtureServices wires the services of the pass to the database. Its clock
// is fixtureNow plus the real time since the call, so the durations in the
// log lines are real. Its log lines go to the test output.
func fixtureServices(t *testing.T, d *db.DB) (*rotation.Service, *operations.Tracker) {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Yerevan")
	require.NoError(t, err)
	started := time.Now()
	clock := func() time.Time { return fixtureNow.Add(time.Since(started)) }
	log := slog.New(logging.NewHandler(os.Stdout, slog.LevelInfo))
	gridSvc := grid.NewService(d, gridrepo.New(d), func(context.Context) (grid.Settings, error) {
		return grid.Settings{Location: loc, HorizonDays: fixtureDays}, nil
	}, clock)
	clocksSvc := clocks.NewService(d, clocksrepo.New(d), clocksrepo.NewCheck(d.Pool), gridSvc,
		func(context.Context) (clocks.Settings, error) {
			return clocks.Settings{FixedToleranceMS: 60000, GridHorizonDays: fixtureDays}, nil
		}, clock)
	hub := ws.NewHub(log)
	pool := workerpool.New(1, 4)
	t.Cleanup(pool.Close)
	tracker := operations.NewTracker(operationsrepo.New(d.Pool), hub, pool, log, clock)
	set := rotation.PlanSettings{
		Location: loc, PlanHorizonDays: 31, GridHorizonDays: fixtureDays, FullPassEnabled: true,
		FullPassTime: "03:00", FullBatchHours: 168, FullSlowMS: 60000, FullHeapWarnMB: 512,
		Freeze: 5 * time.Minute, DefaultFixedToleranceMS: 60000, Build: rotation.Settings{MinGapMS: 10000},
	}
	svc := rotation.NewService(d, rotationrepo.New(d.Pool), clocksSvc, gridSvc,
		func(context.Context) (rotation.PlanSettings, error) { return set, nil },
		tracker, hub, log, clock)
	return svc, tracker
}

// TestFullPassSixMonths runs the full pass on the six-month fixture: it
// completes, stores the plan, reports the counts, and a second pass with no
// change builds nothing. Its log lines (duration, memory, batches, queries)
// go to the test output, which is the measurement of the change.
func TestFullPassSixMonths(t *testing.T) {
	d := dbtest.Migrated(t)
	seedSixMonthFixture(t, d)
	svc, tracker := fixtureServices(t, d)
	ctx := t.Context()

	hours, err := grid.NewService(d, gridrepo.New(d), func(context.Context) (grid.Settings, error) {
		loc, _ := time.LoadLocation("Asia/Yerevan")
		return grid.Settings{Location: loc, HorizonDays: fixtureDays}, nil
	}, func() time.Time { return fixtureNow }).Hours(ctx, fixtureNow.Add(time.Nanosecond), fixtureNow.AddDate(0, 0, fixtureDays))
	require.NoError(t, err)

	run := func(trigger rotation.Trigger) (string, uint64, time.Duration) {
		var summary string
		began := time.Now()
		opID, runErr := tracker.Do(ctx, operations.KindTrackPlanFull, logging.ComponentRotation,
			func(ctx context.Context) (string, error) {
				s, passErr := svc.FullPass(ctx, trigger)
				summary = s
				return s, passErr
			})
		require.NoError(t, runErr)
		return summary, opID, time.Since(began)
	}

	summary, id, elapsed := run(rotation.TriggerButton)
	t.Logf("measurement: first pass %s, elapsed %s, operation %d: %s", summary, elapsed, id, summary)
	op, err := tracker.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, operations.StatusCompleted, op.Status, op.ErrorMessage)
	require.NotNil(t, op.ProgressTotal)
	require.Equal(t, len(hours), *op.ProgressTotal, "progress counts the grid hours")
	require.Equal(t, len(hours), *op.ProgressDone)

	var planned, items int
	require.NoError(t, d.Pool.QueryRow(ctx, `SELECT count(*) FROM plan_hours`).Scan(&planned))
	require.NoError(t, d.Pool.QueryRow(ctx, `SELECT count(*) FROM scheduled_items WHERE status = 'planned'`).Scan(&items))
	require.Equal(t, len(hours), planned, "every grid hour of the range is stored")
	require.Positive(t, items)
	require.Contains(t, summary, fmt.Sprintf("built %d hours, skipped 0", len(hours)))

	again, _, elapsed2 := run(rotation.TriggerNightly)
	t.Logf("measurement: second pass %s, elapsed %s", again, elapsed2)
	require.Contains(t, again, "built 0 hours")
	require.Contains(t, again, fmt.Sprintf("skipped %d", len(hours)))

	// The simulation of the longest allowed range reads the stored plan as
	// history; its timeout is checked against this.
	from := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	began := time.Now()
	sim, err := svc.Simulate(ctx, from, from.AddDate(0, 0, rotation.MaxSimulateDays-1), false)
	require.NoError(t, err)
	t.Logf("measurement: simulation of %d days: %d hours, %d items, elapsed %s, queries %d",
		rotation.MaxSimulateDays, len(sim.Hours), sim.Items, time.Since(began), sim.Queries)
}
