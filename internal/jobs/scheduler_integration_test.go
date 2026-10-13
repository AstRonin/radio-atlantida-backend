//go:build integration

package jobs_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/jobs"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	jobsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/jobs"
	operationsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"
)

type nopPublisher struct{}

func (nopPublisher) Publish(any) {}

func TestSchedulerRunsOnIntervalAndStops(t *testing.T) {
	d := dbtest.Migrated(t)
	var runs atomic.Int32
	tracker := operations.NewTracker(operationsrepo.New(d.Pool), nopPublisher{}, dbtest.Logger(t), time.Now)
	job := jobs.Job{
		Kind: operations.KindRetention, Component: logging.ComponentRetention,
		FirstRun: time.Millisecond, Interval: 20 * time.Millisecond, Timeout: 5 * time.Second,
		Run: func(context.Context) (string, error) {
			runs.Add(1)
			return "ok", nil
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- jobs.NewScheduler(jobsrepo.NewLocks(d.Pool), tracker, dbtest.Logger(t), job).Run(ctx)
	}()

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
