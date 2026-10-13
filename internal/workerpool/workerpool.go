// Package workerpool runs tasks on a fixed number of worker goroutines.
//
// Work that may arrive in bursts (asynchronous operations, per-file media
// work) goes through a pool instead of one goroutine per task, so the number
// of goroutines and the memory they hold stay bounded: at most Workers tasks
// run at once and at most Queue more wait.
package workerpool

import (
	"context"
	"errors"
	"sync"
)

// ErrClosed is returned by Submit after Close.
var ErrClosed = errors.New("worker pool closed")

// Pool is a fixed set of workers reading tasks from a bounded queue.
//
// The pool owns its workers: they start in New and stop in Close, after the
// queue has drained. A task must respect its own context; the pool does not
// cancel tasks. A panicking task crashes the process like any goroutine, so
// tasks that run foreign code recover themselves (see operations.Tracker).
type Pool struct {
	tasks chan func()
	wg    sync.WaitGroup

	// mu guards closed and the send on tasks against a concurrent Close.
	mu     sync.RWMutex
	closed bool
}

// New starts workers goroutines with a queue of queue waiting tasks. It
// panics if workers < 1 or queue < 0 (a wiring error at startup).
func New(workers, queue int) *Pool {
	if workers < 1 || queue < 0 {
		panic("workerpool: workers must be >= 1 and queue >= 0")
	}
	p := &Pool{tasks: make(chan func(), queue)}
	for range workers {
		p.wg.Go(func() {
			for task := range p.tasks {
				task()
			}
		})
	}
	return p
}

// Submit queues task. When the queue is full it waits for a free slot until
// ctx ends and then returns ctx.Err(). After Close it returns ErrClosed.
func (p *Pool) Submit(ctx context.Context, task func()) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return ErrClosed
	}
	select {
	case p.tasks <- task:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting tasks, runs the queued ones, and waits until every
// worker has stopped. It is safe to call more than once.
func (p *Pool) Close() {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.tasks)
	}
	p.mu.Unlock()
	p.wg.Wait()
}

// ForEach runs fn for every item on at most workers goroutines and returns
// the first error. The first error cancels the context passed to the other
// calls, and items not yet started are skipped. Use it for batches (scan a
// directory, probe files) instead of one goroutine per item.
func ForEach[T any](ctx context.Context, workers int, items []T, fn func(context.Context, T) error) error {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	next := make(chan T)
	var wg sync.WaitGroup
	for range min(workers, len(items)) {
		wg.Go(func() {
			for item := range next {
				if ctx.Err() != nil {
					continue // cancelled: drain without running
				}
				if err := fn(ctx, item); err != nil {
					cancel(err)
				}
			}
		})
	}

feed:
	for _, item := range items {
		select {
		case next <- item:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()

	// The first error of fn, or the cancellation of the parent context.
	return context.Cause(ctx)
}
