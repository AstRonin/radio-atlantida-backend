// Package jobs runs background jobs in the backend process: a small
// ticker-based scheduler and the schedules of the jobs. It holds no business
// rules; each job runs a function of its domain package (retention).
//
// Every run is an asynchronous operation (§6.5): it has an operations row
// and ends with a <kind>.completed or <kind>.failed event. A job is a
// singleton: a run takes a PostgreSQL advisory lock, so two runs never
// overlap, even across processes.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

// Job is a recurring background job.
type Job struct {
	Kind      operations.Kind
	Component logging.Component
	// FirstRun is the delay after startup before the first run.
	FirstRun time.Duration
	// Interval is the time between the start of one run and the next.
	Interval time.Duration
	// Timeout bounds one run.
	Timeout time.Duration
	// Due, if set, is asked before each run: a run that is not due is
	// skipped without an operation. A job that runs once a day checks its
	// time here, not with a long interval.
	Due func(ctx context.Context) (bool, error)
	Run operations.Func
}

// Operations runs a function as an operation in the calling goroutine.
type Operations interface {
	Do(ctx context.Context, kind operations.Kind, component logging.Component, fn operations.Func) (uint64, error)
}

// ErrLocked reports that another session holds the lock of a job.
var ErrLocked = errors.New("advisory lock held by another session")

// Locker runs fn while holding a PostgreSQL session-level advisory lock on
// key (internal/repository/jobs). It returns ErrLocked without running fn if
// another session holds the lock.
type Locker interface {
	WithLock(ctx context.Context, key int64, fn func() error) error
}

// Scheduler runs jobs on their intervals until its context ends.
type Scheduler struct {
	locks Locker
	ops   Operations
	log   *slog.Logger // root logger
	jobs  []Job
}

// NewScheduler returns a scheduler for jobs.
func NewScheduler(locks Locker, ops Operations, log *slog.Logger, jobs ...Job) *Scheduler {
	return &Scheduler{locks: locks, ops: ops, log: log, jobs: jobs}
}

// Run starts one goroutine per job and returns when ctx is cancelled and
// every run in progress has stopped.
func (s *Scheduler) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	for _, j := range s.jobs {
		g.Go(func() error {
			s.loop(gctx, j)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	return nil
}

func (s *Scheduler) loop(ctx context.Context, j Job) {
	timer := time.NewTimer(j.FirstRun)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.runOnce(ctx, j)
		timer.Reset(j.Interval)
	}
}

// runOnce runs the job under its advisory lock. Each run gets its own
// context with the job timeout, derived from the service context, so
// shutdown stops it.
func (s *Scheduler) runOnce(ctx context.Context, j Job) {
	log := logging.For(s.log, j.Component)
	if j.Due != nil {
		due, err := j.Due(ctx)
		if err != nil {
			log.Error("job due check failed", "kind", string(j.Kind), logging.KeyError, err)
			return
		}
		if !due {
			return
		}
	}
	rctx, cancel := context.WithTimeout(ctx, j.Timeout)
	defer cancel()

	err := s.locks.WithLock(rctx, lockKey(j.Kind), func() error {
		// The outcome of the job is stored, logged, and published by Do;
		// an error here means the operation itself could not be stored.
		_, err := s.ops.Do(rctx, j.Kind, j.Component, j.Run)
		return err
	})
	switch {
	case errors.Is(err, ErrLocked):
		log.Info("job skipped: another run holds the lock", "kind", string(j.Kind))
	case err != nil && ctx.Err() == nil:
		log.Error("job run failed", "kind", string(j.Kind), logging.KeyError, err)
	}
}

// lockKey derives the advisory lock key of a job kind.
func lockKey(kind operations.Kind) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("jobs/" + kind))
	return int64(h.Sum64()) //nolint:gosec // G115: a hash; wrapping to negative keys is fine
}
