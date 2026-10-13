//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
)

func TestSessionSettings(t *testing.T) {
	url := dbtest.StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	d, err := db.Open(ctx, url, dbtest.Logger(t))
	require.NoError(t, err)
	t.Cleanup(d.Close)
	require.NoError(t, d.Pool.Ping(ctx))

	const q = `SELECT current_setting('TimeZone'), current_setting('statement_timeout'),
	                  current_setting('idle_in_transaction_session_timeout')`
	var tz, stmt, idle string
	require.NoError(t, d.Pool.QueryRow(ctx, q).Scan(&tz, &stmt, &idle))
	require.Equal(t, []string{"UTC", "30s", "1min"}, []string{tz, stmt, idle})

	// GORM runs on the same pool, so it has the same session settings.
	var gormTZ string
	require.NoError(t, d.Gorm.WithContext(ctx).Raw(`SELECT current_setting('TimeZone')`).Scan(&gormTZ).Error)
	require.Equal(t, "UTC", gormTZ)

	mdb, err := db.OpenMigrationDB(url)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, mdb.Close()) })
	var lock string
	require.NoError(t, mdb.QueryRowContext(ctx,
		`SELECT current_setting('TimeZone'), current_setting('statement_timeout'),
		        current_setting('lock_timeout')`).Scan(&tz, &stmt, &lock))
	require.Equal(t, []string{"UTC", "0", "10s"}, []string{tz, stmt, lock})
}

func TestWithTx(t *testing.T) {
	url := dbtest.StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, url, dbtest.Logger(t))
	require.NoError(t, err)
	t.Cleanup(d.Close)
	_, err = d.Pool.Exec(ctx, `CREATE TABLE t (v int NOT NULL)`)
	require.NoError(t, err)

	errBoom := errors.New("boom")
	count := func() int {
		var n int
		require.NoError(t, d.Pool.QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n))
		return n
	}

	require.NoError(t, db.WithTx(ctx, d.Pool, func(tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, `INSERT INTO t VALUES (1)`)
		return execErr
	}))
	require.Equal(t, 1, count(), "pgx commit")

	err = db.WithTx(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, execErr := tx.Exec(ctx, `INSERT INTO t VALUES (2)`); execErr != nil {
			return execErr
		}
		return errBoom
	})
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, 1, count(), "pgx rollback")

	require.NoError(t, db.WithGormTx(ctx, d.Gorm, func(tx *gorm.DB) error {
		return tx.Exec(`INSERT INTO t VALUES (3)`).Error
	}))
	require.Equal(t, 2, count(), "gorm commit")

	err = db.WithGormTx(ctx, d.Gorm, func(tx *gorm.DB) error {
		if execErr := tx.Exec(`INSERT INTO t VALUES (4)`).Error; execErr != nil {
			return execErr
		}
		return errBoom
	})
	require.ErrorIs(t, err, errBoom)
	require.Equal(t, 2, count(), "gorm rollback")
}
