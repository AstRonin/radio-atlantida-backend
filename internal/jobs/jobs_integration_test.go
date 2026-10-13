//go:build integration

package jobs

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

type nopPublisher struct{}

func (nopPublisher) Publish(any) {}

var now = time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC)

func TestRetention(t *testing.T) {
	d := dbtest.Migrated(t)
	ctx := t.Context()
	pool := d.Pool

	var trackID, outageID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO tracks (title, file_path, type_id)
		VALUES ('Song', 'a.mp3', (SELECT id FROM playlist_types WHERE code = 'music'))
		RETURNING id`).Scan(&trackID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component)
		VALUES ($1, $1, 'icecast', 'icecast') RETURNING id`, now.Add(-30*24*time.Hour)).Scan(&outageID))

	old := now.Add(-11 * 24 * time.Hour)
	recent := now.Add(-9 * 24 * time.Hour)
	insertAired := func(status string, startedAt time.Time) int64 {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO play_log (track_id, status, planned_start_utc, started_at, actual_duration_ms, truncated)
			VALUES ($1, $2, $3, $3, 1000, $2 = 'truncated') RETURNING id`,
			trackID, status, startedAt).Scan(&id))
		return id
	}
	insertMissed := func(resolution string, resolvedAt *time.Time) int64 {
		var id int64
		var resolvedItem *int64
		if resolution == "rescheduled" {
			one := int64(1)
			resolvedItem = &one
		}
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO play_log (track_id, status, planned_start_utc, outage_id, resolution, resolved_at, resolved_item_id)
			VALUES ($1, 'missed', $2, $3, $4, $5, $6) RETURNING id`,
			trackID, old.Add(-24*time.Hour), outageID, resolution, resolvedAt, resolvedItem).Scan(&id))
		return id
	}

	keep := []int64{
		insertAired("aired", recent),
		insertMissed("open", nil), // open missed items are never deleted
		insertMissed("dismissed", &recent),
	}
	drop := []int64{
		insertAired("aired", old),
		insertAired("truncated", old),
		insertMissed("dismissed", &old),
		insertMissed("rescheduled", &old),
	}

	tracker := operations.NewTracker(pool, nopPublisher{}, dbtest.Logger(t), func() time.Time { return now })
	oldOp, err := tracker.Start(ctx, operations.KindRetention)
	require.NoError(t, err)
	require.NoError(t, tracker.Complete(ctx, oldOp, "ok"))
	_, err = pool.Exec(ctx, `UPDATE operations SET started_at = $1, finished_at = $1 WHERE id = $2`, old, oldOp)
	require.NoError(t, err)

	summary, err := NewRetention(pool, tracker, func() time.Time { return now }).Run(ctx)
	require.NoError(t, err)
	require.Equal(t, "deleted 4 play log rows and 1 operations", summary)

	rows, err := pool.Query(ctx, `SELECT id FROM play_log ORDER BY id`)
	require.NoError(t, err)
	var left []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		left = append(left, id)
	}
	require.NoError(t, rows.Err())
	require.ElementsMatch(t, keep, left)
	require.NotContains(t, left, drop[0])
}

// blockingOps holds a run open until release is closed.
type blockingOps struct {
	runs    atomic.Int32
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingOps) Do(ctx context.Context, _ operations.Kind, _ logging.Component, _ operations.Func) (int64, error) {
	b.runs.Add(1)
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return 1, nil
}

func TestSingletonRunsDoNotOverlap(t *testing.T) {
	d := dbtest.Migrated(t)
	// A second pool is a second process.
	other, err := pgxpool.NewWithConfig(t.Context(), d.Pool.Config())
	require.NoError(t, err)
	t.Cleanup(other.Close)

	ops := &blockingOps{started: make(chan struct{}), release: make(chan struct{})}
	job := Job{Kind: operations.KindRetention, Component: logging.ComponentRetention, Timeout: 10 * time.Second}
	first := NewScheduler(d.Pool, ops, slog.New(slog.DiscardHandler), job)
	second := NewScheduler(other, ops, slog.New(slog.DiscardHandler), job)

	done := make(chan struct{})
	go func() {
		defer close(done)
		first.runOnce(t.Context(), job)
	}()
	<-ops.started

	second.runOnce(t.Context(), job) // lock held: skipped
	require.EqualValues(t, 1, ops.runs.Load())

	close(ops.release)
	<-done
	second.runOnce(t.Context(), job) // lock free again
	require.EqualValues(t, 2, ops.runs.Load())
}

func TestSchedulerRunsOnIntervalAndStops(t *testing.T) {
	d := dbtest.Migrated(t)
	var runs atomic.Int32
	tracker := operations.NewTracker(d.Pool, nopPublisher{}, dbtest.Logger(t), time.Now)
	job := Job{
		Kind: operations.KindRetention, Component: logging.ComponentRetention,
		FirstRun: time.Millisecond, Interval: 20 * time.Millisecond, Timeout: 5 * time.Second,
		Run: func(context.Context) (string, error) {
			runs.Add(1)
			return "ok", nil
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- NewScheduler(d.Pool, tracker, dbtest.Logger(t), job).Run(ctx) }()

	require.Eventually(t, func() bool { return runs.Load() >= 3 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop")
	}

	ops, err := tracker.List(t.Context(), operations.ListFilter{Limit: 100})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(ops), 3)
	for _, op := range ops {
		require.Equal(t, operations.StatusCompleted, op.Status)
	}
}
