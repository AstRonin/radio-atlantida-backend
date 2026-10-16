package rotation

import (
	"context"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

const (
	// rebuildQueue bounds the clock_build operations waiting for the
	// planner. One manager saves clocks by hand, and the planner merges
	// everything queued into one sync, so a short queue never fills in
	// practice; a full one makes the request wait (Rebuild).
	rebuildQueue = 32
	// rebuildTimeout bounds one sync of the planner: at most a plan horizon
	// of hours, each in its own short transaction.
	rebuildTimeout = 5 * time.Minute
)

// rebuild is a queued operation of the planner: a clock_build, or an
// insert operation (reschedule, hour_insert) when insert is set.
type rebuild struct {
	opID uint64
	// force rebuilds the hours in the range even when they are current.
	force *Range
	// insert adds or removes a one-time insert of one hour.
	insert *insertOp
}

// Rebuild starts a clock_build operation that syncs the plan after a clock
// or grid change (§7, "Grid and clock changes"), and returns its ID at once.
// The planner goroutine (Run) does the work; its outcome is the operation's
// terminal event. ctx is the request context: it only bounds the wait for a
// queue slot.
func (s *Service) Rebuild(ctx context.Context) (uint64, error) {
	return s.enqueue(ctx, nil)
}

// RebuildRange starts a clock_build operation that rebuilds every future
// hour in the range, current or not.
func (s *Service) RebuildRange(ctx context.Context, r Range) (uint64, error) {
	if err := checkRange(r); err != nil {
		return 0, err
	}
	return s.enqueue(ctx, &r)
}

func (s *Service) enqueue(ctx context.Context, force *Range) (uint64, error) {
	id, err := s.ops.Start(ctx, operations.KindClockBuild)
	if err != nil {
		return 0, err
	}
	s.enqueueOp(ctx, rebuild{opID: id, force: force})
	return id, nil
}

// enqueueOp queues a started operation for the planner. If ctx ends first,
// the operation fails like any other, so its row does not stay running.
func (s *Service) enqueueOp(ctx context.Context, r rebuild) {
	select {
	case s.queue <- r:
	case <-ctx.Done():
		_ = s.ops.Execute(ctx, r.opID, logging.ComponentRotation, func(context.Context) (string, error) {
			return "", fmt.Errorf("queue operation: %w", ctx.Err())
		})
	}
}

// Run is the planner: it runs the queued operations until ctx
// is cancelled. Everything queued when a sync starts is merged into it, and
// each operation gets the outcome of that sync. It is the only consumer of
// the queue; serve owns it in its errgroup. Operations still queued at
// shutdown fail.
func (s *Service) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			s.failQueued(ctx)
			return nil
		case r := <-s.queue:
			s.runBatch(ctx, append([]rebuild{r}, s.drain()...))
		}
	}
}

// drain takes what is queued now, without waiting.
func (s *Service) drain() []rebuild {
	var out []rebuild
	for {
		select {
		case r := <-s.queue:
			out = append(out, r)
		default:
			return out
		}
	}
}

// runBatch runs the insert operations of a batch one by one, then the
// clock builds as one sync.
func (s *Service) runBatch(ctx context.Context, batch []rebuild) {
	var force []Range
	var builds []rebuild
	for _, r := range batch {
		if r.insert != nil {
			rctx, cancel := context.WithTimeout(ctx, hourTimeout+dbTimeout)
			// Execute stores, logs, and publishes the outcome; an error
			// here means the outcome itself could not be stored, and it is
			// logged.
			_ = s.ops.Execute(rctx, r.opID, logging.ComponentRotation, func(ctx context.Context) (string, error) {
				return s.runInsert(ctx, *r.insert)
			})
			cancel()
			continue
		}
		builds = append(builds, r)
		if r.force != nil {
			force = append(force, *r.force)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, rebuildTimeout)
	defer cancel()
	var summary string
	var syncErr error
	for i, r := range builds {
		fn := func(ctx context.Context) (string, error) {
			if i == 0 {
				res, err := s.Sync(ctx, force)
				summary, syncErr = res.String(), err
			}
			return summary, syncErr
		}
		_ = s.ops.Execute(rctx, r.opID, logging.ComponentRotation, fn)
	}
}

// failQueued fails the operations still queued at shutdown.
func (s *Service) failQueued(ctx context.Context) {
	for _, r := range s.drain() {
		_ = s.ops.Execute(ctx, r.opID, logging.ComponentRotation, func(context.Context) (string, error) {
			return "", fmt.Errorf("rebuild cancelled: %w", ctx.Err())
		})
	}
}
