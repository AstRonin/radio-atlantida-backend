// Package operations tracks asynchronous operations (§6.5 of the design).
//
// Every operation has a row in the operations table, which is the source of
// truth for its state, and ends with exactly one terminal WebSocket event:
// <kind>.completed or <kind>.failed. The event is sent only after the final
// state is stored, so a panel that missed it reads the same outcome through
// the REST API.
package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/workerpool"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// Kind names a kind of operation. Its events are <kind>.completed,
// <kind>.failed, and <kind>.progress.
type Kind string

// Operation kinds. Event types are part of the API contract: add a kind
// here, never rename one that is released.
const (
	// KindRetention is the nightly cleanup of the play log and of old
	// operations.
	KindRetention Kind = "retention"
)

// Kinds lists every operation kind (for the OpenAPI event type enums).
func Kinds() []Kind { return []Kind{KindRetention} }

// CompletedEvent is the type of the success event.
func (k Kind) CompletedEvent() string { return string(k) + ".completed" }

// FailedEvent is the type of the failure event.
func (k Kind) FailedEvent() string { return string(k) + ".failed" }

// ProgressEvent is the type of the progress event.
func (k Kind) ProgressEvent() string { return string(k) + ".progress" }

// Status is the state of an operation.
type Status string

// Operation states (ck_operations_status).
const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// Operation is the stored state of one operation.
type Operation struct {
	ID            int64
	Kind          string
	Status        Status
	StartedAt     time.Time
	FinishedAt    *time.Time
	ErrorMessage  *string
	ResultSummary *string
}

// Errors returned by the tracker.
var (
	ErrNotFound   = errors.New("operation not found")
	ErrNotRunning = errors.New("operation is not running")
)

// Func is the work of an operation. It returns a short result summary.
type Func func(ctx context.Context) (summary string, err error)

// Publisher sends an event to the connected panels without blocking.
type Publisher interface {
	Publish(event any)
}

// Store persists operations (internal/repository/operations).
type Store interface {
	// Insert stores a running operation and returns its ID.
	Insert(ctx context.Context, kind Kind, startedAt time.Time) (int64, error)
	// Finish moves a running operation to its final state and returns its
	// kind, or ErrNotRunning if it is not running.
	Finish(ctx context.Context, f Finish) (Kind, error)
	// FailRunning fails every running operation with message and returns
	// the number of rows changed.
	FailRunning(ctx context.Context, at time.Time, message string) (int64, error)
	// Get returns one operation or ErrNotFound.
	Get(ctx context.Context, id int64) (Operation, error)
	// List returns operations in ID order.
	List(ctx context.Context, f ListFilter) ([]Operation, error)
}

// Finish is the final state of an operation.
type Finish struct {
	ID            int64
	Status        Status
	FinishedAt    time.Time
	ErrorMessage  *string
	ResultSummary *string
}

const (
	// dbTimeout bounds each statement of the tracker.
	dbTimeout = 5 * time.Second
	// maxErrorMessage keeps stored and published error messages short.
	maxErrorMessage = 2000
	// interruptedMessage is stored for operations cut off by a restart.
	interruptedMessage = "interrupted: the backend stopped before the operation finished"
)

// Tracker starts, finishes, and reads operations.
//
// Operations started by Run execute on pool, so a burst of requests never
// starts more goroutines than the pool has workers. Tasks submitted by Run
// are counted in wg; Wait blocks until they end.
type Tracker struct {
	store Store
	pub   Publisher
	pool  *workerpool.Pool
	log   *slog.Logger // root logger; failures are logged with their component
	now   func() time.Time

	wg sync.WaitGroup
}

// NewTracker returns a tracker. pool runs the operations started by Run (the
// caller owns it and closes it on shutdown); log is the root logger; now is
// the clock.
func NewTracker(store Store, pub Publisher, pool *workerpool.Pool, log *slog.Logger, now func() time.Time) *Tracker {
	return &Tracker{store: store, pub: pub, pool: pool, log: log, now: now}
}

// Start stores a new running operation and returns its ID.
func (t *Tracker) Start(ctx context.Context, kind Kind) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	id, err := t.store.Insert(ctx, kind, t.now().UTC())
	if err != nil {
		return 0, fmt.Errorf("start %s operation: %w", kind, err)
	}
	return id, nil
}

// Complete stores the success of a running operation and sends
// <kind>.completed.
func (t *Tracker) Complete(ctx context.Context, id int64, summary string) error {
	at := t.now().UTC()
	kind, err := t.finish(ctx, id, StatusCompleted, at, nil, &summary)
	if err != nil {
		return err
	}
	t.pub.Publish(ws.OperationCompletedEvent{
		Type:        kind.CompletedEvent(),
		Time:        at,
		OperationID: id,
		Data:        ws.OperationCompletedData{Kind: string(kind), Summary: summary},
	})
	return nil
}

// Fail stores the failure of a running operation, logs cause at ERROR with
// component (which also raises an error alert in the panel), and sends
// <kind>.failed.
func (t *Tracker) Fail(ctx context.Context, id int64, component logging.Component, cause error) error {
	at := t.now().UTC()
	msg := truncate(cause.Error(), maxErrorMessage)
	kind, err := t.finish(ctx, id, StatusFailed, at, &msg, nil)
	if err != nil {
		return err
	}

	attrs := []any{"operation_id", id, "kind", string(kind), logging.KeyError, cause}
	if p, ok := errors.AsType[*panicError](cause); ok {
		attrs = append(attrs, "stack", p.stack)
	}
	logging.For(t.log, component).ErrorContext(ctx, "operation failed", attrs...)

	t.pub.Publish(ws.OperationFailedEvent{
		Type:        kind.FailedEvent(),
		Time:        at,
		OperationID: id,
		Data:        ws.OperationFailedData{Kind: string(kind), Component: string(component), Error: msg},
	})
	return nil
}

// Progress sends <kind>.progress. It is not stored.
func (t *Tracker) Progress(id int64, kind Kind, done, total int, message string) {
	t.pub.Publish(ws.OperationProgressEvent{
		Type:        kind.ProgressEvent(),
		Time:        t.now().UTC(),
		OperationID: id,
		Data:        ws.OperationProgressData{Kind: string(kind), Done: done, Total: total, Message: message},
	})
}

func (t *Tracker) finish(ctx context.Context, id int64, status Status, at time.Time, errMsg, summary *string) (Kind, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	kind, err := t.store.Finish(ctx, Finish{
		ID: id, Status: status, FinishedAt: at, ErrorMessage: errMsg, ResultSummary: summary,
	})
	if err != nil {
		return "", fmt.Errorf("finish operation %d: %w", id, err)
	}
	return kind, nil
}

// Run starts an operation and queues fn on the worker pool. It returns the
// operation ID as soon as the row is stored and fn is queued; when the queue
// is full it waits for a free slot.
//
// ctx must be the service context, never a request context: the work
// outlives the request that started it (§6.5). Copy request values such as
// the request ID into it explicitly. A panic in fn fails the operation, and
// so does a pool that refuses fn (shutdown): the row never stays running.
func (t *Tracker) Run(ctx context.Context, kind Kind, component logging.Component, fn Func) (int64, error) {
	id, err := t.Start(ctx, kind)
	if err != nil {
		return 0, err
	}
	t.wg.Add(1)
	err = t.pool.Submit(ctx, func() {
		defer t.wg.Done()
		// execute logs a failure to store the outcome; nobody else waits
		// for it.
		_ = t.execute(ctx, id, component, fn)
	})
	if err != nil {
		t.wg.Done()
		// The outcome is the operation's failure, reported like any other.
		_ = t.execute(ctx, id, component, func(context.Context) (string, error) {
			return "", fmt.Errorf("queue operation: %w", err)
		})
	}
	return id, nil
}

// Do runs fn as an operation in the calling goroutine (scheduled jobs). The
// outcome of fn is stored, logged, and published; the returned error reports
// only a failure to store the operation.
func (t *Tracker) Do(ctx context.Context, kind Kind, component logging.Component, fn Func) (int64, error) {
	id, err := t.Start(ctx, kind)
	if err != nil {
		return 0, err
	}
	return id, t.execute(ctx, id, component, fn)
}

// Wait blocks until every operation started by Run has finished.
func (t *Tracker) Wait() { t.wg.Wait() }

func (t *Tracker) execute(ctx context.Context, id int64, component logging.Component, fn Func) error {
	summary, err := call(ctx, fn)

	// The final write must happen even when ctx is cancelled (shutdown), so
	// the row does not stay running.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dbTimeout)
	defer cancel()
	if err != nil {
		err = t.Fail(fctx, id, component, err)
	} else {
		err = t.Complete(fctx, id, summary)
	}
	if err != nil {
		logging.For(t.log, logging.ComponentDB).ErrorContext(fctx, "store operation result failed",
			"operation_id", id, logging.KeyError, err)
	}
	return err
}

// panicError is the failure of an operation whose function panicked.
type panicError struct {
	value any
	stack string
}

func (p *panicError) Error() string { return fmt.Sprintf("panic: %v", p.value) }

func call(ctx context.Context, fn Func) (summary string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = &panicError{value: rec, stack: string(debug.Stack())}
		}
	}()
	return fn(ctx)
}

// FailInterrupted marks every running operation as failed. Operations run
// inside this process only, so at startup a running row was cut off by a
// restart. It returns the number of rows changed.
func (t *Tracker) FailInterrupted(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	n, err := t.store.FailRunning(ctx, t.now().UTC(), interruptedMessage)
	if err != nil {
		return 0, fmt.Errorf("fail interrupted operations: %w", err)
	}
	return n, nil
}

// Get returns one operation, or ErrNotFound.
func (t *Tracker) Get(ctx context.Context, id int64) (Operation, error) {
	op, err := t.store.Get(ctx, id)
	if err != nil {
		return Operation{}, fmt.Errorf("get operation %d: %w", id, err)
	}
	return op, nil
}

// ListFilter selects operations for a panel that catches up after a
// reconnect.
type ListFilter struct {
	// Since keeps operations started or finished at or after it. Zero keeps
	// all.
	Since time.Time
	// AfterID continues a previous page: only IDs greater than it.
	AfterID int64
	// Limit is the page size.
	Limit int
}

// List returns operations in ID order.
func (t *Tracker) List(ctx context.Context, f ListFilter) ([]Operation, error) {
	ops, err := t.store.List(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("list operations: %w", err)
	}
	return ops, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
