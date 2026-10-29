//go:build integration

package rotation_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	rotationrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/rotation"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

// measurePlaylists is the number of playlists of the measured library; the
// tracks are spread evenly over them, and the two clocks use 12 of them each.
const measurePlaylists = 60

// seedLargeLibrary fills a fresh database with n tracks in measurePlaylists
// playlists, one playlist each, by 20 tracks per artist, and the six-month grid
// of two clocks that use 12 of the playlists each (overlapping on 4).
func seedLargeLibrary(t *testing.T, d *db.DB, n int) {
	t.Helper()
	ctx := t.Context()
	pool := d.Pool
	var music uint64
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM playlist_types WHERE code = 'music'`).Scan(&music))
	_, err := pool.Exec(ctx, `INSERT INTO playlists (name, type_id) SELECT 'Playlist ' || g, $1 FROM generate_series(1, $2) g`,
		music, measurePlaylists)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO playlist_play_rules (playlist_id, rule_id)
SELECT p.id, r.id FROM playlists p, play_rules r WHERE r.name = 'Music rotation'`)
	require.NoError(t, err)

	artists := n / 20
	_, err = pool.Exec(ctx, `INSERT INTO artists (name) SELECT 'Artist ' || g FROM generate_series(1, $1) g`, artists)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
WITH ar AS (SELECT array_agg(id ORDER BY id) AS ids FROM artists),
     pl AS (SELECT array_agg(id ORDER BY id) AS ids FROM playlists)
INSERT INTO tracks (title, artist_id, duration_ms, file_path, type_id)
SELECT 'Track ' || g, ar.ids[g % $1 + 1], 120000 + ((g::bigint * 7919) % 150000)::int, 'music/t-' || g || '.mp3', $2
FROM generate_series(1, $3) g, ar`, artists, music, n)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
WITH pl AS (SELECT array_agg(id ORDER BY id) AS ids FROM playlists)
INSERT INTO track_playlists (track_id, playlist_id)
SELECT g, pl.ids[g % $1 + 1] FROM generate_series(1, $2) g, pl`, measurePlaylists, n)
	require.NoError(t, err)

	var day, night uint64
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO clocks (name) VALUES ('Day') RETURNING id`).Scan(&day))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO clocks (name) VALUES ('Night') RETURNING id`).Scan(&night))
	_, err = pool.Exec(ctx, `
INSERT INTO clock_playlists (clock_id, playlist_id, position)
SELECT $1, p.id, row_number() OVER (ORDER BY p.id) - 1
FROM (SELECT id FROM playlists ORDER BY id LIMIT 12) p`, day)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO clock_playlists (clock_id, playlist_id, position)
SELECT $1, p.id, row_number() OVER (ORDER BY p.id) - 1
FROM (SELECT id FROM playlists ORDER BY id OFFSET 8 LIMIT 12) p`, night)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
INSERT INTO grid_slots (weekday, hour, clock_id, valid_from)
SELECT wd, h, CASE WHEN h < 12 THEN $1::bigint ELSE $2::bigint END, date '2026-10-15'
FROM generate_series(1, 7) wd, generate_series(0, 23) h`, day, night)
	require.NoError(t, err)
}

// TestMeasureLargeLibrary measures the library snapshot and the passes on a
// large library. It is skipped unless ATLANTIDA_MEASURE_TRACKS is set (for
// example 800000), because it writes that many rows to its own database.
func TestMeasureLargeLibrary(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv("ATLANTIDA_MEASURE_TRACKS"))
	if err != nil || n <= 0 {
		t.Skip("set ATLANTIDA_MEASURE_TRACKS to measure the large library")
	}
	d := dbtest.Migrated(t)
	began := time.Now()
	seedLargeLibrary(t, d, n)
	t.Logf("measurement: seeded %d tracks in %s", n, time.Since(began))

	ctx := t.Context()
	var ids []uint64
	rows, err := d.Pool.Query(ctx, `SELECT id FROM playlists ORDER BY id LIMIT 20`)
	require.NoError(t, err)
	for rows.Next() {
		var id uint64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	scope := rotation.LibraryScope{Playlists: ids}

	store := rotationrepo.New(d.Pool)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	readStart := time.Now()
	lib, err := store.Library(ctx, scope)
	readElapsed := time.Since(readStart)
	require.NoError(t, err)
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	t.Logf("measurement: snapshot tracks=%d read=%s live_heap_after_mb=%.1f live_heap_delta_mb=%.1f",
		len(lib.Tracks), readElapsed, float64(after.HeapAlloc)/(1<<20), float64(after.HeapAlloc-before.HeapAlloc)/(1<<20))
	runtime.KeepAlive(lib)

	svc, tracker := fixtureServices(t, d)
	began = time.Now()
	short, err := svc.RunJob(ctx)
	require.NoError(t, err)
	t.Logf("measurement: short pass on an empty plan (31 days): %s, elapsed %s", short, time.Since(began))

	began = time.Now()
	idle, err := svc.RunJob(ctx)
	require.NoError(t, err)
	t.Logf("measurement: short pass with nothing to build: %s, elapsed %s", idle, time.Since(began))

	began = time.Now()
	var summary string
	opID, err := tracker.Do(ctx, operations.KindTrackPlanFull, logging.ComponentRotation, func(ctx context.Context) (string, error) {
		s, passErr := svc.FullPass(ctx, rotation.TriggerButton)
		summary = s
		return s, passErr
	})
	require.NoError(t, err)
	op, err := tracker.Get(ctx, opID)
	require.NoError(t, err)
	t.Logf("measurement: full pass (183 days) %s, elapsed %s, status %s", summary, time.Since(began), op.Status)
	require.Equal(t, operations.StatusCompleted, op.Status, fmt.Sprint(op.ErrorMessage))
}
