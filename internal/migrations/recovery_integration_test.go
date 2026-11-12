//go:build integration

package migrations_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/migrations"
)

// migrationFS returns the embedded migrations up to and including upTo
// (all when upTo is 0) plus extra files.
func migrationFS(t *testing.T, upTo int, extra map[string]string) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.up.sql")
	require.NoError(t, err)
	for i, name := range names {
		if upTo > 0 && i+1 > upTo {
			break
		}
		body, err := migrations.Files.ReadFile(name)
		require.NoError(t, err)
		m[name] = &fstest.MapFile{Data: body}
	}
	for name, body := range extra {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return m
}

// latest is the number of embedded migrations.
func latest(t *testing.T) int {
	t.Helper()
	names, err := fs.Glob(migrations.Files, "*.up.sql")
	require.NoError(t, err)
	return len(names)
}

func openSQL(t *testing.T, url string) *sql.DB {
	t.Helper()
	sqlDB, err := db.OpenMigrationDB(url)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	return sqlDB
}

// schemaVersion returns golang-migrate's version and dirty flag; -1 when no
// version is recorded.
func schemaVersion(ctx context.Context, t *testing.T, sqlDB *sql.DB) (int, bool) {
	t.Helper()
	v, dirty := -1, false
	err := sqlDB.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty)
	if err == sql.ErrNoRows {
		return -1, false
	}
	require.NoError(t, err)
	return v, dirty
}

func exists(ctx context.Context, t *testing.T, sqlDB *sql.DB, query string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT EXISTS (`+query+`)`).Scan(&ok))
	return ok
}

func runUp(t *testing.T, fsys fs.FS, url string, log *slog.Logger) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	return migrations.UpFS(ctx, fsys, url, log)
}

func bufferLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(logging.NewHandler(buf, slog.LevelInfo))
}

// A failing transactional file is rolled back by PostgreSQL; the runner
// resets the version, so the database is clean at the previous version.
// Files applied earlier in the same run stay applied.
func TestUpRollsBackFailedMigration(t *testing.T) {
	url := dbtest.StartPostgres(t)
	ctx := t.Context()
	n := latest(t)
	fsys := migrationFS(t, 0, map[string]string{
		fmt.Sprintf("%04d_ok.up.sql", n+1): "CREATE TABLE t_ok (id int);",
		fmt.Sprintf("%04d_fail.up.sql", n+2): "-- failing test migration\n" +
			"CREATE TABLE t_fail (id int);\nALTER TABLE tracks ADD COLUMN x int;\nSELECT 1/0;\n",
	})
	var logs bytes.Buffer

	err := runUp(t, fsys, url, bufferLogger(&logs))

	require.ErrorIs(t, err, migrations.ErrRolledBack)
	require.NotErrorIs(t, err, migrations.ErrDirty)
	require.ErrorContains(t, err, "22012")
	require.NotContains(t, err.Error(), "ALTER TABLE", "the migration SQL is cut to its first line")
	sqlDB := openSQL(t, url)
	v, dirty := schemaVersion(ctx, t, sqlDB)
	require.Equal(t, n+1, v)
	require.False(t, dirty)
	require.True(t, exists(ctx, t, sqlDB, `SELECT 1 FROM information_schema.tables WHERE table_name = 't_ok'`))
	require.False(t, exists(ctx, t, sqlDB, `SELECT 1 FROM information_schema.tables WHERE table_name = 't_fail'`))
	require.False(t, exists(ctx, t, sqlDB, `SELECT 1 FROM information_schema.columns WHERE table_name = 'tracks' AND column_name = 'x'`))
	require.Contains(t, logs.String(), `"msg":"migration rolled back"`)
	require.Contains(t, logs.String(), fmt.Sprintf(`"version_failed":%d,"schema_version":%d,"sqlstate":"22012"`, n+2, n+1))
	require.NotContains(t, logs.String(), "ALTER TABLE")

	// The next run tries again from the clean version.
	require.ErrorIs(t, runUp(t, fsys, url, dbtest.Logger(t)), migrations.ErrRolledBack)
	v, dirty = schemaVersion(ctx, t, sqlDB)
	require.Equal(t, n+1, v)
	require.False(t, dirty)
}

// A failing first migration leaves no version at all.
func TestUpRollsBackFailedFirstMigration(t *testing.T) {
	url := dbtest.StartPostgres(t)
	fsys := fstest.MapFS{"0001_fail.up.sql": {Data: []byte("CREATE TABLE t (id int);\nSELECT 1/0;")}}

	err := runUp(t, fsys, url, dbtest.Logger(t))

	require.ErrorIs(t, err, migrations.ErrRolledBack)
	sqlDB := openSQL(t, url)
	v, _ := schemaVersion(t.Context(), t, sqlDB)
	require.Equal(t, -1, v)
	require.False(t, exists(t.Context(), t, sqlDB, `SELECT 1 FROM information_schema.tables WHERE table_name = 't'`))
}

// A migration that waits for a lock gives up (lock_timeout) and is rolled
// back like any other failure.
func TestUpRollsBackLockTimeout(t *testing.T) {
	d := dbtest.Migrated(t)
	url := d.Pool.Config().ConnString()
	ctx := t.Context()
	n := latest(t)

	tx, err := d.Pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
	_, err = tx.Exec(ctx, `LOCK TABLE tracks IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	// The connection's lock_timeout is 10 s; the test file shortens it.
	fsys := migrationFS(t, 0, map[string]string{
		fmt.Sprintf("%04d_lock.up.sql", n+1): "CREATE TABLE t_before (id int);\nSET lock_timeout = '1s';\nALTER TABLE tracks ADD COLUMN y int;",
	})
	err = runUp(t, fsys, url, dbtest.Logger(t))

	require.ErrorIs(t, err, migrations.ErrRolledBack)
	require.ErrorContains(t, err, "55P03")
	require.NoError(t, tx.Rollback(ctx))
	sqlDB := openSQL(t, url)
	v, dirty := schemaVersion(ctx, t, sqlDB)
	require.Equal(t, n, v)
	require.False(t, dirty)
	require.False(t, exists(ctx, t, sqlDB, `SELECT 1 FROM information_schema.tables WHERE table_name = 't_before'`))
}

// A marked non-transactional file that fails may be half applied: the
// database stays dirty.
func TestUpLeavesFailedNonTransactionalMigrationDirty(t *testing.T) {
	url := dbtest.StartPostgres(t)
	n := latest(t)
	fsys := migrationFS(t, 0, map[string]string{
		fmt.Sprintf("%04d_concurrently.up.sql", n+1): migrations.NonTransactionalMarker +
			"\nCREATE INDEX CONCURRENTLY ix_missing ON no_such_table (id);\n",
	})
	var logs bytes.Buffer

	err := runUp(t, fsys, url, bufferLogger(&logs))

	require.ErrorIs(t, err, migrations.ErrDirty)
	require.ErrorContains(t, err, "deploy tool restores the pre-update dump")
	v, dirty := schemaVersion(t.Context(), t, openSQL(t, url))
	require.Equal(t, n+1, v)
	require.True(t, dirty)
	require.Contains(t, logs.String(), `"msg":"database dirty"`)

	// A dirty database is refused as it is.
	require.ErrorIs(t, runUp(t, fsys, url, dbtest.Logger(t)), migrations.ErrDirty)
}

// A marked file that succeeds is applied like any other.
func TestUpAppliesNonTransactionalMigration(t *testing.T) {
	url := dbtest.StartPostgres(t)
	n := latest(t)
	fsys := migrationFS(t, 0, map[string]string{
		fmt.Sprintf("%04d_concurrently.up.sql", n+1): migrations.NonTransactionalMarker +
			"\nCREATE INDEX CONCURRENTLY ix_tracks_title_test ON tracks (title);\n",
	})

	require.NoError(t, runUp(t, fsys, url, dbtest.Logger(t)))

	v, dirty := schemaVersion(t.Context(), t, openSQL(t, url))
	require.Equal(t, n+1, v)
	require.False(t, dirty)
}

func TestCompat(t *testing.T) {
	url := dbtest.StartPostgres(t)
	ctx := t.Context()
	compat := func() migrations.Status {
		t.Helper()
		st, err := migrations.Compat(ctx, url)
		require.NoError(t, err)
		return st
	}

	// Empty database: no version, the baseline.
	require.Equal(t, migrations.Status{MinAppVersion: "1.0.0"}, compat())

	// Before migration 0022 the table is missing: the baseline.
	require.NoError(t, runUp(t, migrationFS(t, 21, nil), url, dbtest.Logger(t)))
	require.Equal(t, migrations.Status{SchemaVersion: 21, MinAppVersion: "1.0.0"}, compat())

	require.NoError(t, runUp(t, migrationFS(t, 0, nil), url, dbtest.Logger(t)))
	st := compat()
	require.Equal(t, migrations.Status{SchemaVersion: uint(latest(t)), MinAppVersion: "1.0.0"}, st)
	require.True(t, st.Compatible("1.0.0"))

	sqlDB := openSQL(t, url)
	_, err := sqlDB.ExecContext(ctx, `UPDATE schema_compat SET min_app_version = '1.4.0', set_by_migration = 99 WHERE id = 1`)
	require.NoError(t, err)
	st = compat()
	require.Equal(t, "1.4.0", st.MinAppVersion)
	require.False(t, st.Compatible("1.3.9"))
	require.True(t, st.Compatible("1.4.0"))
	require.True(t, st.Compatible("dev"))

	_, err = sqlDB.ExecContext(ctx, `UPDATE schema_migrations SET dirty = true`)
	require.NoError(t, err)
	require.True(t, compat().Dirty)

	// The table enforces one row and a release version.
	_, err = sqlDB.ExecContext(ctx, `UPDATE schema_compat SET min_app_version = 'dev'`)
	require.ErrorContains(t, err, "ck_schema_compat_min_app_version")
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO schema_compat (id, min_app_version, set_by_migration) VALUES (2, '1.0.0', 1)`)
	require.ErrorContains(t, err, "ck_schema_compat_one_row")
}
