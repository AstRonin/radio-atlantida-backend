// Package jobs runs background jobs in the backend process: a small
// ticker-based scheduler and the jobs themselves (retention).
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

	"github.com/jackc/pgx/v5/pgxpool"
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
	Run     operations.Func
}

// Operations runs a function as an operation in the calling goroutine.
type Operations interface {
	Do(ctx context.Context, kind operations.Kind, component logging.Component, fn operations.Func) (int64, error)
}

// Scheduler runs jobs on their intervals until its context ends.
type Scheduler struct {
	pool *pgxpool.Pool
	ops  Operations
	log  *slog.Logger // root logger
	jobs []Job
}

// NewScheduler returns a scheduler for jobs.
func NewScheduler(pool *pgxpool.Pool, ops Operations, log *slog.Logger, jobs ...Job) *Scheduler {
	return &Scheduler{pool: pool, ops: ops, log: log, jobs: jobs}
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
	rctx, cancel := context.WithTimeout(ctx, j.Timeout)
	defer cancel()

	err := withAdvisoryLock(rctx, s.pool, lockKey(j.Kind), func() error {
		// The outcome of the job is stored, logged, and published by Do;
		// an error here means the operation itself could not be stored.
		_, err := s.ops.Do(rctx, j.Kind, j.Component, j.Run)
		return err
	})
	switch {
	case errors.Is(err, errLocked):
		log.Info("job skipped: another run holds the lock", "kind", string(j.Kind))
	case err != nil && ctx.Err() == nil:
		log.Error("job run failed", "kind", string(j.Kind), logging.KeyError, err)
	}
}

var errLocked = errors.New("advisory lock held by another session")

// lockKey derives the advisory lock key of a job kind.
func lockKey(kind operations.Kind) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("jobs/" + kind))
	return int64(h.Sum64()) //nolint:gosec // G115: a hash; wrapping to negative keys is fine
}

// withAdvisoryLock runs fn while holding a session-level advisory lock on a
// dedicated connection, or returns errLocked if another session holds it.
// The connection is held for the whole run, so fn should use other
// connections from the pool.
func withAdvisoryLock(ctx context.Context, pool *pgxpool.Pool, key int64, fn func() error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for lock: %w", err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		return fmt.Errorf("take advisory lock: %w", err)
	}
	if !locked {
		return errLocked
	}
	defer func() {
		// Unlock even when ctx is cancelled; a failed unlock is released
		// when the connection closes.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
			// Drop the connection instead; the lock goes with it.
			_ = conn.Conn().Close(uctx)
		}
	}()
	return fn()
}
