//go:build integration

package migrations_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/migrations"
)

func TestUpCreatesSchemaAndSeeds(t *testing.T) {
	url := dbtest.StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	require.NoError(t, migrations.Up(ctx, url, dbtest.Logger(t)))
	sqlDB, err := db.OpenMigrationDB(url)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

	var tables []string
	rows, err := sqlDB.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
		ORDER BY table_name`)
	require.NoError(t, err)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []string{
		"artists", "backend_heartbeat", "broadcast_outages", "broadcast_state", "clock_inserts", "clock_playlists",
		"clocks", "grid_slots", "hour_inserts", "operations", "plan_hours",
		"play_log", "play_rules", "playlist_play_rules", "playlist_types", "playlists",
		"rotation_nightly_passes", "scheduled_items", "settings", "track_play_rules",
		"track_playlists", "track_waveforms", "tracks",
	}, tables)

	var types string
	require.NoError(t, sqlDB.QueryRowContext(ctx, `
		SELECT string_agg(code, ',' ORDER BY code) FROM playlist_types WHERE is_system`).Scan(&types))
	require.Equal(t, "jingle,music,news,segment", types)

	// Separation became the play rule "Music rotation" (migration 0013).
	var rules string
	require.NoError(t, sqlDB.QueryRowContext(ctx, `
		SELECT string_agg(name || ':' || min_interval_minutes || ':' || artist_min_interval_minutes, ',')
		FROM play_rules`).Scan(&rules))
	require.Equal(t, "Music rotation:4320:4320", rules)

	version := func() int {
		var v int
		var dirty bool
		require.NoError(t, sqlDB.QueryRowContext(ctx,
			`SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty))
		require.False(t, dirty)
		return v
	}
	applied := version()
	require.Positive(t, applied)

	// A second run is a no-op.
	require.NoError(t, migrations.Up(ctx, url, dbtest.Logger(t)))
	require.Equal(t, applied, version())
	var n int
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM playlist_types`).Scan(&n))
	require.Equal(t, 4, n, "seeds are not inserted twice")
}

func TestUpRefusesDirtyDatabase(t *testing.T) {
	d := dbtest.Migrated(t)
	ctx := t.Context()
	_, err := d.Pool.Exec(ctx, `UPDATE schema_migrations SET dirty = true`)
	require.NoError(t, err)

	err = up(t, d)

	require.ErrorIs(t, err, migrations.ErrDirty)
	require.ErrorContains(t, err, "update.bat restore")
}

// After a rollback to an earlier release, the database may be newer than the
// binary. The compose stack runs `migrate up` before `backend`, so this must
// succeed or the rollback would never start (§9.2, step 11).
func TestUpAcceptsNewerDatabase(t *testing.T) {
	d := dbtest.Migrated(t)
	_, err := d.Pool.Exec(t.Context(), `UPDATE schema_migrations SET version = 9999`)
	require.NoError(t, err)

	require.NoError(t, up(t, d))
}

func up(t *testing.T, d *db.DB) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	return migrations.Up(ctx, d.Pool.Config().ConnString(), dbtest.Logger(t))
}

// fixture is a small library, a clock in the grid, an outage, and one aired
// play_log row. Each constraint case runs on top of it in a transaction that
// is rolled back.
const fixture = `
INSERT INTO artists (name) VALUES ('Artist');
INSERT INTO playlists (name, type_id) SELECT 'Pop', id FROM playlist_types WHERE code = 'music';
INSERT INTO tracks (title, artist_id, duration_ms, file_path, type_id)
	SELECT 'Song', a.id, 180000, 'music/song.mp3', pt.id
	FROM artists a, playlist_types pt WHERE pt.code = 'music';
INSERT INTO track_playlists (track_id, playlist_id) SELECT t.id, p.id FROM tracks t, playlists p;
INSERT INTO play_rules (name, max_per_hour) VALUES ('Once per hour', 1);
INSERT INTO track_play_rules (track_id, rule_id) SELECT t.id, r.id FROM tracks t, play_rules r;
INSERT INTO clocks (name) VALUES ('Morning');
INSERT INTO clock_playlists (clock_id, playlist_id, position) SELECT c.id, p.id, 0 FROM clocks c, playlists p;
INSERT INTO clock_inserts (clock_id, offset_ms, track_id) SELECT c.id, 1800000, t.id FROM clocks c, tracks t;
INSERT INTO grid_slots (weekday, hour, clock_id, valid_from) SELECT 1, 7, id, '2026-10-12' FROM clocks;
INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component)
	VALUES ('2026-10-13T06:00:00Z', '2026-10-13T06:05:00Z', 'icecast', 'icecast');
INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, started_at,
                      planned_out_ms, actual_duration_ms, clock_id)
	SELECT t.id, t.artist_id, 'aired', '2026-10-13T05:00:00Z', '2026-10-13T05:00:00Z', 180000, 180000, c.id
	FROM tracks t, clocks c;
`

func TestConstraints(t *testing.T) {
	d := dbtest.Migrated(t)

	// Subqueries for fixture ids.
	const (
		musicType = `(SELECT id FROM playlist_types WHERE code = 'music')`
		track     = `(SELECT id FROM tracks WHERE file_path = 'music/song.mp3')`
		clock     = `(SELECT id FROM clocks WHERE name = 'Morning')`
		outage    = `(SELECT id FROM broadcast_outages)`
	)
	insertTrack := func(path string) string {
		return `INSERT INTO tracks (title, file_path, type_id) VALUES ('T', '` + path + `', ` + musicType + `)`
	}

	tests := []struct {
		name string
		sql  string
		// want is the violated constraint, or "" when the statement must succeed.
		want string
	}{
		// Playlist types.
		{"type code must be a slug", `INSERT INTO playlist_types (code, name) VALUES ('Bad Code', 'Bad')`, "ck_playlist_types_code_slug"},
		{"type code is unique", `INSERT INTO playlist_types (code, name) VALUES ('music', 'Music 2')`, "uq_playlist_types_code"},
		{"custom type", `INSERT INTO playlist_types (code, name) VALUES ('promo', 'Promo')`, ""},
		{"type used by a playlist cannot be deleted", `DELETE FROM playlist_types WHERE code = 'music'`, "fk_playlists_type_id"},

		// Track paths stay under MEDIA_ROOT.
		{"relative path", insertTrack("dir/a..b.mp3"), ""},
		{"absolute path", insertTrack("/etc/passwd"), "ck_tracks_file_path_relative"},
		{"parent directory", insertTrack("../x.mp3"), "ck_tracks_file_path_relative"},
		{"parent directory inside", insertTrack("a/../../x.mp3"), "ck_tracks_file_path_relative"},
		{"windows drive", insertTrack("C:/x.mp3"), "ck_tracks_file_path_relative"},
		{"backslash", insertTrack(`a\b.mp3`), "ck_tracks_file_path_relative"},
		{"file path is unique", insertTrack("music/song.mp3"), "uq_tracks_file_path"},

		// History blocks deletes (track, artist, clock).
		{"track in play log cannot be deleted", `DELETE FROM track_play_rules; DELETE FROM tracks`, "fk_play_log_track_id"},
		{"clock in grid cannot be deleted", `DELETE FROM clocks`, "fk_grid_slots_clock_id"},
		{"clock in play log cannot be deleted", `DELETE FROM grid_slots; DELETE FROM clocks`, "fk_play_log_clock_id"},

		// Play rules.
		{"rule needs a constraint", `INSERT INTO play_rules (name) VALUES ('Empty')`, "ck_play_rules_any_constraint"},
		{"window both or none", `INSERT INTO play_rules (name, window_from) VALUES ('W', '06:00')`, "ck_play_rules_window_both"},
		{"window crossing midnight", `INSERT INTO play_rules (name, window_from, window_to, weekdays) VALUES ('Night', '22:00', '02:00', '{5,6}')`, ""},
		{"empty window", `INSERT INTO play_rules (name, window_from, window_to) VALUES ('W', '06:00', '06:00')`, "ck_play_rules_window_not_empty"},
		{"weekday out of range", `INSERT INTO play_rules (name, weekdays) VALUES ('W', '{0}')`, "ck_play_rules_weekdays"},
		{"weekdays not empty", `INSERT INTO play_rules (name, weekdays) VALUES ('W', '{}')`, "ck_play_rules_weekdays"},
		{"rule in use cannot be deleted", `DELETE FROM play_rules WHERE name = 'Once per hour'`, "fk_track_play_rules_rule_id"},
		{"artist interval alone", `INSERT INTO play_rules (name, artist_min_interval_minutes) VALUES ('A', 120)`, ""},
		{"artist interval positive", `INSERT INTO play_rules (name, artist_min_interval_minutes) VALUES ('A', 0)`, "ck_play_rules_artist_min_interval_minutes_positive"},

		// Clocks and elements.
		{"clock is 60 minutes", `INSERT INTO clocks (name, length_seconds) VALUES ('Half', 1800)`, "ck_clocks_length_seconds"},
		{"playlist rule", `UPDATE playlists SET clock_rule = 'often'`, "ck_playlists_clock_rule"},
		{"spread needs a count", `UPDATE playlists SET clock_rule = 'spread'`, "ck_playlists_clock_count"},
		{"count only for spread", `UPDATE playlists SET clock_count = 2`, "ck_playlists_clock_count"},
		{"spread 12", `UPDATE playlists SET clock_rule = 'spread', clock_count = 12`, ""},
		{"spread 13", `UPDATE playlists SET clock_rule = 'spread', clock_count = 13`, "ck_playlists_clock_count"},
		{"clock playlist twice", `INSERT INTO clock_playlists (clock_id, playlist_id, position) SELECT ` + clock + `, id, 1 FROM playlists`, "pk_clock_playlists"},
		{"clock playlist position unique", `
			INSERT INTO playlists (name, type_id) VALUES ('Jingles', ` + musicType + `);
			INSERT INTO clock_playlists (clock_id, playlist_id, position) SELECT ` + clock + `, id, 0 FROM playlists WHERE name = 'Jingles'`, "uq_clock_playlists_clock_id_position"},
		{"clock playlist spread override needs a count", `UPDATE clock_playlists SET clock_rule = 'spread'`, "ck_clock_playlists_clock_count"},
		{"clock playlist count without override", `UPDATE clock_playlists SET clock_count = 2`, "ck_clock_playlists_clock_count"},
		{"clock playlist weight", `UPDATE clock_playlists SET fill_weight = 101`, "ck_clock_playlists_fill_weight"},
		{"playlist in a clock cannot be deleted", `DELETE FROM playlists`, "fk_clock_playlists_playlist_id"},
		{"insert offset whole seconds", `UPDATE clock_inserts SET offset_ms = 1500`, "ck_clock_inserts_offset_ms"},
		{"insert offset inside the hour", `UPDATE clock_inserts SET offset_ms = 3600000`, "ck_clock_inserts_offset_ms"},
		{"insert tolerance", `UPDATE clock_inserts SET tolerance_ms = 600001`, "ck_clock_inserts_tolerance_ms"},
		{"insert with both sources", `UPDATE clock_inserts SET playlist_id = (SELECT id FROM playlists)`, "ck_clock_inserts_source"},
		{"insert offset unique", `INSERT INTO clock_inserts (clock_id, offset_ms, track_id) SELECT clock_id, offset_ms, track_id FROM clock_inserts`, "uq_clock_inserts_clock_id_offset_ms"},
		{"track in a clock insert cannot be deleted", `DELETE FROM play_log; DELETE FROM track_play_rules; DELETE FROM tracks`, "fk_clock_inserts_track_id"},
		{"one-time insert offset", `INSERT INTO hour_inserts (hour_start_utc, track_id, placement, offset_ms) VALUES (now(), ` + track + `, 'fixed', 1500)`, "ck_hour_inserts_offset_ms"},
		{"one-time start has no offset", `INSERT INTO hour_inserts (hour_start_utc, track_id, placement, offset_ms) VALUES (now(), ` + track + `, 'start', 0)`, "ck_hour_inserts_offset_ms"},
		{"one-time fixed insert", `INSERT INTO hour_inserts (hour_start_utc, track_id, placement, offset_ms, tolerance_ms) VALUES (now(), ` + track + `, 'fixed', 1780000, 20000)`, ""},

		// Grid.
		{"weekday 1 to 7", `INSERT INTO grid_slots (weekday, hour, clock_id, valid_from) VALUES (8, 0, ` + clock + `, '2026-10-12')`, "ck_grid_slots_weekday"},
		// The fixture slot is Monday 07:00 from 2026-10-12, open-ended.
		{"overlapping slot", `INSERT INTO grid_slots (weekday, hour, clock_id, valid_from, valid_to) VALUES (1, 7, ` + clock + `, '2026-10-20', '2026-10-27')`, "ex_grid_slots_weekday_hour_valid"},
		{"same hour, earlier dates", `INSERT INTO grid_slots (weekday, hour, clock_id, valid_from, valid_to) VALUES (1, 7, ` + clock + `, '2026-10-01', '2026-10-11')`, ""},
		{"ranges touching on one day overlap", `INSERT INTO grid_slots (weekday, hour, clock_id, valid_from, valid_to) VALUES (1, 7, ` + clock + `, '2026-10-01', '2026-10-12')`, "ex_grid_slots_weekday_hour_valid"},
		{"same weekday, other hour", `INSERT INTO grid_slots (weekday, hour, clock_id, valid_from) VALUES (1, 8, ` + clock + `, '2026-10-12')`, ""},
		{"valid range", `INSERT INTO grid_slots (weekday, hour, clock_id, valid_from, valid_to) VALUES (1, 0, ` + clock + `, '2026-10-12', '2026-10-11')`, "ck_grid_slots_valid_range"},

		// Outages: at most one open.
		{"one open outage", `INSERT INTO broadcast_outages (started_at, detected_by, component) VALUES (now(), 'icecast', 'icecast')`, ""},
		{"two open outages", `INSERT INTO broadcast_outages (started_at, detected_by, component) VALUES (now(), 'icecast', 'icecast'), (now(), 'backend', 'backend')`, "uq_broadcast_outages_open"},

		// Broadcast state: one row, known states only.
		{"broadcast state row", `INSERT INTO broadcast_state (id, state, since, updated_at) VALUES (1, 'connected', now(), now())`, ""},
		{"broadcast state second row", `INSERT INTO broadcast_state (id, state, since, updated_at) VALUES (2, 'connected', now(), now())`, "ck_broadcast_state_one_row"},
		{"broadcast state unknown is not stored", `INSERT INTO broadcast_state (id, state, since, updated_at) VALUES (1, 'unknown', now(), now())`, "ck_broadcast_state_state"},
		{"broadcast state other value", `INSERT INTO broadcast_state (id, state, since, updated_at) VALUES (1, 'disconnected', now(), now())`, "ck_broadcast_state_state"},

		// Play log.
		{"missed needs an outage", `INSERT INTO play_log (track_id, status, planned_start_utc, resolution) VALUES (` + track + `, 'missed', now(), 'open')`, "ck_play_log_missed_fields"},
		{"open missed item", `INSERT INTO play_log (track_id, status, planned_start_utc, outage_id, resolution) VALUES (` + track + `, 'missed', now(), ` + outage + `, 'open')`, ""},
		{"dismissed needs resolved_at", `INSERT INTO play_log (track_id, status, planned_start_utc, outage_id, resolution) VALUES (` + track + `, 'missed', now(), ` + outage + `, 'dismissed')`, "ck_play_log_resolved"},
		{"aired needs started_at", `INSERT INTO play_log (track_id, status, planned_start_utc) VALUES (` + track + `, 'aired', now())`, "ck_play_log_aired_fields"},
		{"truncated flag matches status", `INSERT INTO play_log (track_id, status, planned_start_utc, started_at, actual_duration_ms, truncated) VALUES (` + track + `, 'aired', now(), now(), 1000, true)`, "ck_play_log_truncated"},
		{"emergency play without plan", `INSERT INTO play_log (track_id, status, source, started_at, actual_duration_ms) VALUES (` + track + `, 'aired', 'emergency', now(), 1000)`, ""},

		// Track plan.
		{"reschedule_of only on reschedule items", `
			INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, reschedule_of_id)
			VALUES (` + clock + `, ` + track + `, now(), 1000, 'fill', (SELECT id FROM play_log LIMIT 1))`, "ck_scheduled_items_reschedule_of"},
		{"insert source", `
			INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, clock_insert_id)
			VALUES (` + clock + `, ` + track + `, now(), 1000, 'insert', (SELECT id FROM clock_inserts))`, ""},
		{"general source", `
			INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source)
			VALUES (` + clock + `, ` + track + `, now(), 1000, 'general')`, ""},
		{"filler source is gone", `
			INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source)
			VALUES (` + clock + `, ` + track + `, now(), 1000, 'filler_random')`, "ck_scheduled_items_source"},
		{"out point after in point", `
			INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_in_ms, planned_out_ms, source)
			VALUES (` + clock + `, ` + track + `, now(), 1000, 1000, 'fill')`, "ck_scheduled_items_planned_out_ms"},

		// Settings and operations.
		{"setting key format", `INSERT INTO settings (key, value) VALUES ('Bad Key', '1')`, "ck_settings_key"},
		{"setting", `INSERT INTO settings (key, value) VALUES ('liquidsoap.fade_out_ms', '2500')`, ""},
		{"failed operation needs a message", `INSERT INTO operations (kind, status, finished_at) VALUES ('import', 'failed', now())`, "ck_operations_failed_message"},
		{"finished operation has finished_at", `INSERT INTO operations (kind, status) VALUES ('import', 'completed')`, "ck_operations_finished"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			err := inRolledBackTx(ctx, d, func(tx db.Querier) error {
				if _, err := tx.Exec(ctx, fixture); err != nil {
					t.Fatalf("fixture: %v", err)
				}
				_, err := tx.Exec(ctx, tt.sql)
				return err
			})

			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			require.Equal(t, tt.want, pgErr.ConstraintName, pgErr.Message)
		})
	}
}

var errRollback = errors.New("rollback")

// inRolledBackTx runs fn in a transaction that is always rolled back, and
// returns fn's error.
func inRolledBackTx(ctx context.Context, d *db.DB, fn func(db.Querier) error) error {
	var fnErr error
	err := d.WithTx(ctx, func(_ context.Context, tx db.Tx) error {
		fnErr = fn(tx.Pgx())
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		return err
	}
	return fnErr
}
