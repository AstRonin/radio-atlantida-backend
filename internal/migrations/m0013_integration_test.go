//go:build integration

package migrations_test

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/migrations"
)

// before0013 is the data of a station before phase 23: a clock with
// elements, a filler pool, a reminder, a grid slot, a built hour with plan
// items, a missed item rescheduled into it, and the aired history.
const before0013 = `
INSERT INTO artists (name) VALUES ('Artist');
INSERT INTO playlists (name, type_id) SELECT 'Pop', id FROM playlist_types WHERE code = 'music';
INSERT INTO playlists (name, type_id) SELECT 'Jingles', id FROM playlist_types WHERE code = 'jingle';
INSERT INTO tracks (title, artist_id, duration_ms, file_path, type_id)
	SELECT 'Song', a.id, 180000, 'music/song.mp3', pt.id FROM artists a, playlist_types pt WHERE pt.code = 'music';
INSERT INTO track_playlists (track_id, playlist_id) SELECT t.id, p.id FROM tracks t, playlists p WHERE p.name = 'Pop';
UPDATE separation_rules SET min_interval_hours = 48 WHERE scope = 'artist';
INSERT INTO clocks (name, status) VALUES ('Morning', 'complete');
INSERT INTO clock_elements (clock_id, position, nominal_duration_ms) SELECT id, 0, 180000 FROM clocks;
INSERT INTO clock_element_playlists (clock_element_id, playlist_id)
	SELECT e.id, p.id FROM clock_elements e, playlists p WHERE p.name = 'Pop';
INSERT INTO clock_filler_pool (clock_id, track_id, position) SELECT c.id, t.id, 0 FROM clocks c, tracks t;
INSERT INTO filler_requests (clock_id, hour_start_utc, reason, deadline_at, mode)
	SELECT id, '2026-10-14T05:00:00Z', 'short_clock', '2026-10-14T03:00:00Z', 'ask' FROM clocks;
INSERT INTO grid_slots (weekday, hour, clock_id, valid_from) SELECT 3, 9, id, '2026-10-12' FROM clocks;
INSERT INTO plan_hours (hour_start_utc, clock_id, clock_updated_at, grid_slot_id, seed, built_at, filler_request_id)
	SELECT '2026-10-14T05:00:00Z', c.id, c.updated_at, g.id, 1, now(), r.id FROM clocks c, grid_slots g, filler_requests r;
INSERT INTO scheduled_items (grid_slot_id, clock_id, track_id, planned_start_utc, planned_out_ms, source, clock_element_id, filler_pool_id)
	SELECT g.id, c.id, t.id, '2026-10-14T05:03:00Z', 180000, 'filler_pool', NULL, p.id
	FROM grid_slots g, clocks c, tracks t, clock_filler_pool p;
INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component)
	VALUES ('2026-10-13T06:00:00Z', '2026-10-13T06:05:00Z', 'icecast', 'icecast');
INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, started_at, planned_out_ms, actual_duration_ms, clock_id, grid_slot_id)
	SELECT t.id, t.artist_id, 'aired', '2026-10-13T05:00:00Z', '2026-10-13T05:00:00Z', 180000, 180000, c.id, g.id
	FROM tracks t, clocks c, grid_slots g;
INSERT INTO play_log (track_id, status, planned_start_utc, planned_out_ms, clock_id, outage_id, resolution)
	SELECT t.id, 'missed', '2026-10-13T06:01:00Z', 180000, c.id, o.id, 'open' FROM tracks t, clocks c, broadcast_outages o;
INSERT INTO hour_inserts (hour_start_utc, track_id, missed_id, placement)
	SELECT '2026-10-14T05:00:00Z', track_id, id, 'start' FROM play_log WHERE status = 'missed';
INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, reschedule_of_id)
	SELECT c.id, l.track_id, '2026-10-14T05:00:00Z', 180000, 'reschedule', l.id FROM clocks c, play_log l WHERE l.status = 'missed';
UPDATE play_log SET resolution = 'rescheduled', resolved_at = now(),
	resolved_item_id = (SELECT id FROM scheduled_items WHERE source = 'reschedule')
	WHERE status = 'missed';
INSERT INTO settings (key, value) VALUES
	('filler.fallback', '{"mode":"random"}'), ('rotation.min_filler_gap_ms', '5000'),
	('rotation.default_duration_tolerance_ms', '20000'), ('liquidsoap.fade_out_ms', '2500');
`

// Migration 0013 deletes the clocks, the grid, and the plan, keeps the play
// log (a rescheduled missed item is open again), turns the separation rules
// into "Music rotation" on the music playlists, and moves the settings
// (docs/phase-23-plan.md §3.1, §4).
func TestMigration0013(t *testing.T) {
	url := dbtest.StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	sqlDB, err := db.OpenMigrationDB(url)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

	for v := 1; v <= 12; v++ {
		apply(ctx, t, sqlDB, v)
	}
	_, err = sqlDB.ExecContext(ctx, before0013)
	require.NoError(t, err, "fixture")
	apply(ctx, t, sqlDB, 13)

	count := func(q string) int {
		t.Helper()
		var n int
		require.NoError(t, sqlDB.QueryRowContext(ctx, q).Scan(&n))
		return n
	}
	for _, table := range []string{"clocks", "grid_slots", "plan_hours", "scheduled_items", "hour_inserts"} {
		require.Zero(t, count(`SELECT count(*) FROM `+table), table)
	}
	for _, table := range []string{"separation_rules", "clock_elements", "clock_element_playlists", "clock_filler_pool", "filler_requests"} {
		require.Zero(t, count(fmt.Sprintf(`SELECT count(*) FROM information_schema.tables WHERE table_name = '%s'`, table)), table)
	}
	require.Equal(t, 2, count(`SELECT count(*) FROM play_log WHERE clock_id IS NULL AND grid_slot_id IS NULL`), "the play log keeps its rows")
	require.Equal(t, 1, count(`SELECT count(*) FROM play_log
		WHERE status = 'missed' AND resolution = 'open' AND resolved_at IS NULL AND resolved_item_id IS NULL`), "open again")

	var name string
	var track, artist int
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT name, min_interval_minutes, artist_min_interval_minutes FROM play_rules`).
		Scan(&name, &track, &artist))
	require.Equal(t, []any{"Music rotation", 72 * 60, 48 * 60}, []any{name, track, artist})
	require.Equal(t, 1, count(`SELECT count(*) FROM playlist_play_rules pr JOIN playlists p ON p.id = pr.playlist_id WHERE p.name = 'Pop'`),
		"attached to the music playlist")
	require.Zero(t, count(`SELECT count(*) FROM playlist_play_rules pr JOIN playlists p ON p.id = pr.playlist_id WHERE p.name = 'Jingles'`))
	require.Equal(t, 2, count(`SELECT count(*) FROM playlists WHERE clock_rule = 'fill' AND clock_count IS NULL`))

	var keys string
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT string_agg(key, ',' ORDER BY key) FROM settings`).Scan(&keys))
	require.Equal(t, "liquidsoap.fade_out_ms,rotation.min_gap_ms", keys)
}

// apply runs one embedded migration file.
func apply(ctx context.Context, t *testing.T, sqlDB *sql.DB, version int) {
	t.Helper()
	matches, err := fs.Glob(migrations.Files, fmt.Sprintf("%04d_*.up.sql", version))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	body, err := migrations.Files.ReadFile(matches[0])
	require.NoError(t, err)
	_, err = sqlDB.ExecContext(ctx, string(body))
	require.NoError(t, err, matches[0])
}
