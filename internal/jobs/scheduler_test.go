package jobs

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

// fakeLocker grants the lock unless held is set.
type fakeLocker struct {
	held bool
	keys []int64
}

func (f *fakeLocker) WithLock(_ context.Context, key int64, fn func() error) error {
	f.keys = append(f.keys, key)
	if f.held {
		return ErrLocked
	}
	return fn()
}

type countingOps struct{ runs int }

func (c *countingOps) Do(context.Context, operations.Kind, logging.Component, operations.Func) (uint64, error) {
	c.runs++
	return 1, nil
}

func TestRunOnceTakesTheJobLock(t *testing.T) {
	t.Parallel()
	job := Job{Kind: operations.KindRetention, Component: logging.ComponentRetention, Timeout: time.Second}
	tests := []struct {
		name     string
		held     bool
		wantRuns int
	}{
		{"lock free: the job runs", false, 1},
		{"lock held: the run is skipped", true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			locks, ops := &fakeLocker{held: tt.held}, &countingOps{}

			NewScheduler(locks, ops, slog.New(slog.DiscardHandler), job).runOnce(t.Context(), job)

			require.Equal(t, tt.wantRuns, ops.runs)
			require.Equal(t, []int64{lockKey(job.Kind)}, locks.keys)
		})
	}
}

func TestLockKeyIsStable(t *testing.T) {
	t.Parallel()
	// The key is shared by every process of every release that may run at
	// once during an update: it must never change for a kind.
	require.Equal(t, int64(-8726217943897523684), lockKey(operations.KindRetention))
	require.NotEqual(t, lockKey(operations.KindRetention), lockKey("other"))
}

// A job with a due check runs only when it is due, and creates no operation
// when it is not.
func TestRunOnceSkipsAJobThatIsNotDue(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		due      bool
		err      error
		wantRuns int
	}{
		{"due: the job runs", true, nil, 1},
		{"not due: no operation", false, nil, 0},
		{"check failed: no operation", true, context.DeadlineExceeded, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			job := TrackPlanFull(func(context.Context) (string, error) { return "", nil },
				func(context.Context) (bool, error) { return tt.due, tt.err }, time.Minute)
			locks, ops := &fakeLocker{}, &countingOps{}

			NewScheduler(locks, ops, slog.New(slog.DiscardHandler), job).runOnce(t.Context(), job)

			require.Equal(t, tt.wantRuns, ops.runs)
		})
	}
}
