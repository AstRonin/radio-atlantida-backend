//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

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

func openWithTable(t *testing.T) (*db.DB, context.Context) {
	t.Helper()
	url := dbtest.StartPostgres(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	d, err := db.Open(ctx, url, dbtest.Logger(t))
	require.NoError(t, err)
	t.Cleanup(d.Close)
	_, err = d.Pool.Exec(ctx, `
		CREATE TABLE t (
			v int NOT NULL,
			CONSTRAINT uq_t_v UNIQUE (v),
			CONSTRAINT ck_t_v_positive CHECK (v > 0)
		)`)
	require.NoError(t, err)
	return d, ctx
}

func TestWithTx(t *testing.T) {
	d, ctx := openWithTable(t)
	errBoom := errors.New("boom")
	values := func() []int {
		rows, err := d.Pool.Query(ctx, `SELECT v FROM t ORDER BY v`)
		require.NoError(t, err)
		vs, err := pgx.CollectRows(rows, pgx.RowTo[int])
		require.NoError(t, err)
		return vs
	}
	// insertBoth writes a through pgx and b through GORM in tx.
	insertBoth := func(ctx context.Context, tx db.Tx, a, b int) error {
		if _, err := tx.Pgx().Exec(ctx, `INSERT INTO t VALUES ($1)`, a); err != nil {
			return err
		}
		return tx.Gorm().Exec(`INSERT INTO t VALUES (?)`, b).Error
	}

	t.Run("pgx and GORM commit together and see each other", func(t *testing.T) {
		require.NoError(t, d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
			if err := insertBoth(ctx, tx, 1, 2); err != nil {
				return err
			}
			var n int
			require.NoError(t, tx.Gorm().Raw(`SELECT count(*) FROM t`).Scan(&n).Error)
			require.Equal(t, 2, n, "GORM sees the pgx write")
			require.NoError(t, tx.Pgx().QueryRow(ctx, `SELECT count(*) FROM t`).Scan(&n))
			require.Equal(t, 2, n, "pgx sees the GORM write")
			// Outside the transaction nothing is visible yet.
			require.Empty(t, values())
			return nil
		}))
		require.Equal(t, []int{1, 2}, values())
	})

	t.Run("an error rolls back both", func(t *testing.T) {
		err := d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
			if err := insertBoth(ctx, tx, 3, 4); err != nil {
				return err
			}
			return errBoom
		})
		require.ErrorIs(t, err, errBoom)
		require.Equal(t, []int{1, 2}, values())
	})

	t.Run("a panic rolls back and propagates", func(t *testing.T) {
		require.PanicsWithValue(t, "kaboom", func() {
			_ = d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
				if err := insertBoth(ctx, tx, 5, 6); err != nil {
					return err
				}
				panic("kaboom")
			})
		})
		require.Equal(t, []int{1, 2}, values())
	})

	t.Run("a failed statement aborts the transaction", func(t *testing.T) {
		err := d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
			_, _ = tx.Pgx().Exec(ctx, `INSERT INTO t VALUES (1)`) // duplicate, ignored
			return tx.Gorm().Exec(`INSERT INTO t VALUES (7)`).Error
		})
		require.Error(t, err)
		require.Equal(t, []int{1, 2}, values())
	})

	t.Run("a cancelled context rolls back without racing pgx", func(t *testing.T) {
		// database/sql must not roll back from its own goroutine while pgx
		// runs on the connection (run with -race).
		for range 20 {
			cctx, cancel := context.WithCancel(ctx)
			err := d.WithTx(cctx, func(ctx context.Context, tx db.Tx) error {
				if _, err := tx.Pgx().Exec(ctx, `INSERT INTO t VALUES (8)`); err != nil {
					return err
				}
				cancel()
				for range 5 {
					if _, err := tx.Pgx().Exec(ctx, `SELECT pg_sleep(0.001)`); err != nil {
						return err
					}
				}
				return nil
			})
			require.ErrorIs(t, err, context.Canceled)
			cancel()
		}
		require.Equal(t, []int{1, 2}, values())
	})

	t.Run("connections go back to the pool idle", func(t *testing.T) {
		require.Zero(t, d.Pool.Stat().AcquiredConns())
		var open int
		require.NoError(t, d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE state LIKE 'idle in transaction%'`).Scan(&open))
		require.Zero(t, open)
	})
}

func TestConstraintError(t *testing.T) {
	d, ctx := openWithTable(t)
	_, err := d.Pool.Exec(ctx, `INSERT INTO t VALUES (1)`)
	require.NoError(t, err)

	tests := []struct {
		name string
		err  func() error
		want db.ConstraintError
	}{
		{"pgx unique", func() error {
			_, err := d.Pool.Exec(ctx, `INSERT INTO t VALUES (1)`)
			return err
		}, db.ConstraintError{Kind: db.Unique, Constraint: "uq_t_v", Table: "t"}},
		{"GORM check", func() error {
			return d.Gorm.WithContext(ctx).Exec(`INSERT INTO t VALUES (0)`).Error
		}, db.ConstraintError{Kind: db.Check, Constraint: "ck_t_v_positive", Table: "t"}},
		{"inside WithTx", func() error {
			return d.WithTx(ctx, func(_ context.Context, tx db.Tx) error {
				return tx.Gorm().Exec(`INSERT INTO t VALUES (1)`).Error
			})
		}, db.ConstraintError{Kind: db.Unique, Constraint: "uq_t_v", Table: "t"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ce, ok := db.AsConstraintError(tt.err())
			require.True(t, ok)
			require.Equal(t, tt.want.Kind, ce.Kind)
			require.Equal(t, tt.want.Constraint, ce.Constraint)
			require.Equal(t, tt.want.Table, ce.Table)
		})
	}
}
