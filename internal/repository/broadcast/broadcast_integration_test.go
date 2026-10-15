//go:build integration

package broadcast_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/broadcast"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	broadcastrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/broadcast"
)

var t0 = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

func TestGetAndSave(t *testing.T) {
	t.Parallel()
	d := dbtest.Migrated(t)
	s := broadcastrepo.New(d.Pool)
	ctx := t.Context()

	_, found, err := s.Get(ctx)
	require.NoError(t, err)
	require.False(t, found, "an empty table: unknown")

	reason := "mount not found"
	down := broadcast.Record{State: broadcast.StateUnreachable, Since: t0, Reason: &reason, UpdatedAt: t0.Add(time.Second)}
	require.NoError(t, s.Save(ctx, down))
	got, found, err := s.Get(ctx)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, down, got)

	up := broadcast.Record{State: broadcast.StateConnected, Since: t0.Add(time.Minute), UpdatedAt: t0.Add(time.Minute)}
	require.NoError(t, s.Save(ctx, up))
	got, _, err = s.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, up, got, "upsert of the one row, reason cleared")
	var n int
	require.NoError(t, d.Pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_state`).Scan(&n))
	require.Equal(t, 1, n)
}

func TestSaveInTransactionRollsBack(t *testing.T) {
	t.Parallel()
	d := dbtest.Migrated(t)
	s := broadcastrepo.New(d.Pool)
	ctx := t.Context()

	errRollback := errors.New("rollback")
	err := d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		require.NoError(t, s.WithTx(tx).Save(ctx, broadcast.Record{State: broadcast.StateConnected, Since: t0, UpdatedAt: t0}))
		return errRollback
	})
	require.ErrorIs(t, err, errRollback)
	_, found, err := s.Get(ctx)
	require.NoError(t, err)
	require.False(t, found)
}

// countingQuerier counts the statements that write broadcast_state.
type countingQuerier struct {
	db.Querier
	mu     sync.Mutex
	writes int
	reads  int
}

func (c *countingQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c.mu.Lock()
	if strings.Contains(sql, "INSERT INTO broadcast_state") {
		c.writes++
	}
	c.mu.Unlock()
	return c.Querier.Exec(ctx, sql, args...)
}

func (c *countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.Querier.QueryRow(ctx, sql, args...)
}

// A steady stream writes nothing after the first transition; a restart
// with the stream playing keeps since and writes nothing (§7 of the plan).
func TestSteadyStreamWritesOnlyChanges(t *testing.T) {
	t.Parallel()
	d := dbtest.Migrated(t)
	q := &countingQuerier{Querier: d.Pool}
	now := t0
	tr := broadcast.NewTracker(d, broadcastrepo.New(q), nopPublisher{}, slog.New(slog.DiscardHandler),
		func() time.Time { return now })
	ctx := t.Context()

	tr.OnOutput(ctx, true, now)
	require.Equal(t, 1, q.writes)
	for i := range 100 {
		now = t0.Add(time.Duration(i+1) * time.Second)
		tr.OnHeartbeat(ctx, false, now)
		if i%5 == 0 {
			tr.OnMountCheck(ctx, true, now)
		}
	}
	tr.OnOutput(ctx, true, now)
	require.Equal(t, 1, q.writes, "no writes while the state does not change")

	// Restart: a new tracker reads the row once and keeps since.
	q2 := &countingQuerier{Querier: d.Pool}
	restarted := broadcast.NewTracker(d, broadcastrepo.New(q2), nopPublisher{}, slog.New(slog.DiscardHandler),
		func() time.Time { return now })
	for range 10 {
		now = now.Add(5 * time.Second)
		restarted.OnHeartbeat(ctx, false, now)
		restarted.OnMountCheck(ctx, true, now)
	}
	st := restarted.Status()
	require.Equal(t, broadcast.StateConnected, st.State)
	require.Equal(t, t0, *st.Since)
	require.Zero(t, q2.writes)
	require.Equal(t, 1, q2.reads)
}

// The outage and the state are written in one transaction: a failure
// rolls both back.
func TestOutageTransaction(t *testing.T) {
	t.Parallel()
	d := dbtest.Migrated(t)
	s := broadcastrepo.New(d.Pool)
	tr := broadcast.NewTracker(d, s, nopPublisher{}, slog.New(slog.DiscardHandler), func() time.Time { return t0 })
	ctx := t.Context()
	openOutages := func() int {
		var n int
		require.NoError(t, d.Pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_outages WHERE ended_at IS NULL`).Scan(&n))
		return n
	}

	err := tr.OutageOpened(ctx, func(ctx context.Context, tx db.Tx) (broadcast.Outage, error) {
		var id uint64
		err := tx.Pgx().QueryRow(ctx, `INSERT INTO broadcast_outages (started_at, detected_by, component)
VALUES ($1, 'icecast', 'icecast') RETURNING id`, t0).Scan(&id)
		return broadcast.Outage{ID: id, StartedAt: t0, Reason: "icecast"}, err
	})
	require.NoError(t, err)
	got, found, err := s.Get(ctx)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, broadcast.StateUnreachable, got.State)
	require.Equal(t, 1, openOutages())

	errAfter := errors.New("failed after the close")
	err = tr.OutageClosed(ctx, t0.Add(time.Minute), func(ctx context.Context, tx db.Tx) error {
		if _, execErr := tx.Pgx().Exec(ctx, `UPDATE broadcast_outages SET ended_at = $1`, t0.Add(time.Minute)); execErr != nil {
			return execErr
		}
		return errAfter
	})
	require.ErrorIs(t, err, errAfter)
	require.Equal(t, 1, openOutages(), "the close is rolled back")
	got, _, err = s.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, broadcast.StateUnreachable, got.State)
	require.Equal(t, broadcast.StateUnreachable, tr.Status().State)

	tr.OnMountCheck(ctx, true, t0.Add(2*time.Minute))
	err = tr.OutageClosed(ctx, t0.Add(2*time.Minute), func(ctx context.Context, tx db.Tx) error {
		_, execErr := tx.Pgx().Exec(ctx, `UPDATE broadcast_outages SET ended_at = $1`, t0.Add(2*time.Minute))
		return execErr
	})
	require.NoError(t, err)
	require.Zero(t, openOutages())
	got, _, err = s.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, broadcast.Record{State: broadcast.StateConnected, Since: t0.Add(2 * time.Minute), UpdatedAt: t0}, got)
}

type nopPublisher struct{}

func (nopPublisher) Publish(any) {}
