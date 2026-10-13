package workerpool_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/workerpool"
)

func TestPoolRunsEveryTaskWithBoundedConcurrency(t *testing.T) {
	t.Parallel()
	const workers, tasks = 3, 50
	p := workerpool.New(workers, 5)

	var running, peak, done atomic.Int32
	for range tasks {
		require.NoError(t, p.Submit(t.Context(), func() {
			n := running.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			running.Add(-1)
			done.Add(1)
		}))
	}
	p.Close()

	require.EqualValues(t, tasks, done.Load(), "Close runs the queued tasks")
	require.LessOrEqual(t, peak.Load(), int32(workers))
}

func TestSubmitWaitsForQueueSlotUntilContextEnds(t *testing.T) {
	t.Parallel()
	p := workerpool.New(1, 0)
	t.Cleanup(p.Close)

	release := make(chan struct{})
	started := make(chan struct{})
	require.NoError(t, p.Submit(t.Context(), func() {
		close(started)
		<-release
	}))
	<-started

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := p.Submit(ctx, func() {})
	require.ErrorIs(t, err, context.DeadlineExceeded, "the only worker is busy and there is no queue")
	close(release)
}

func TestSubmitAfterClose(t *testing.T) {
	t.Parallel()
	p := workerpool.New(1, 1)
	p.Close()
	p.Close() // idempotent

	require.ErrorIs(t, p.Submit(t.Context(), func() {}), workerpool.ErrClosed)
}

func TestNewPanicsOnBadSizes(t *testing.T) {
	t.Parallel()
	require.Panics(t, func() { workerpool.New(0, 1) })
	require.Panics(t, func() { workerpool.New(1, -1) })
}

func TestForEach(t *testing.T) {
	t.Parallel()
	errBoom := errors.New("boom")
	items := make([]int, 100)
	for i := range items {
		items[i] = i
	}

	tests := []struct {
		name    string
		workers int
		items   []int
		fail    int // item that fails; -1 for none
		wantErr error
	}{
		{"all succeed", 4, items, -1, nil},
		{"no items", 4, nil, -1, nil},
		{"zero workers means one", 0, items[:5], -1, nil},
		{"first error is returned", 4, items, 10, errBoom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var running, peak, done atomic.Int32
			err := workerpool.ForEach(t.Context(), tt.workers, tt.items, func(_ context.Context, i int) error {
				n := running.Add(1)
				defer running.Add(-1)
				for {
					old := peak.Load()
					if n <= old || peak.CompareAndSwap(old, n) {
						break
					}
				}
				if i == tt.fail {
					return errBoom
				}
				done.Add(1)
				return nil
			})
			require.ErrorIs(t, err, tt.wantErr)
			if tt.wantErr == nil {
				require.EqualValues(t, len(tt.items), done.Load())
			}
			require.LessOrEqual(t, int(peak.Load()), max(tt.workers, 1))
		})
	}
}

func TestForEachStopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var calls atomic.Int32
	err := workerpool.ForEach(ctx, 2, []int{1, 2, 3}, func(context.Context, int) error {
		calls.Add(1)
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, calls.Load())
}
