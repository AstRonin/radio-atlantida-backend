package rotation

import (
	"context"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

// Store persists the plan and the one-time inserts
// (internal/repository/rotation).
type Store interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) Store
	// LockPlan takes the transaction-level advisory lock that serializes
	// writes of the plan.
	LockPlan(ctx context.Context) error
	// Library returns the tracks with a duration that the scope selects, in
	// ID order, without titles, and every play rule.
	Library(ctx context.Context, scope LibraryScope) (Library, error)
	// TracksOf returns the tracks with a duration among ids, with their titles
	// and artists (for output).
	TracksOf(ctx context.Context, ids []uint64) ([]Track, error)
	// History returns the plays in [from, to): aired and truncated in the
	// play log, planned and queued in the plan, except the planned items
	// of the hour that starts at hourStart.
	History(ctx context.Context, from, to, hourStart time.Time) ([]Play, error)
	// Window returns the plays in [from, to) like History, but keeps the
	// planned items of every hour (flagged Planned); the runs of builds
	// exclude the hours they build (window).
	Window(ctx context.Context, from, to time.Time) ([]Play, error)
	// ClaimNightly takes the nightly claim of a station local date
	// (YYYY-MM-DD); true once per date, even across processes and restarts.
	ClaimNightly(ctx context.Context, day string) (bool, error)
	// InsertsRange returns the one-time inserts of the hours that start in
	// [from, to), in hour and ID order.
	InsertsRange(ctx context.Context, from, to time.Time) ([]Insert, error)
	// QueuedHours returns the starts of the built hours in [from, to) that
	// have a queued item: their items are in Liquidsoap's queue.
	QueuedHours(ctx context.Context, from, to time.Time) ([]time.Time, error)
	// PlanHours returns the built hours that start in [from, to).
	PlanHours(ctx context.Context, from, to time.Time) ([]PlanHour, error)
	// PlanHour returns one built hour; false if there is none.
	PlanHour(ctx context.Context, start time.Time) (PlanHour, bool, error)
	// ReplaceHour replaces the planned items of the hour with items and
	// stores h.
	ReplaceHour(ctx context.Context, h PlanHour, items []Item) error
	// ClearHour deletes the planned items of the hour and its record.
	ClearHour(ctx context.Context, start time.Time) error
	// Plan returns the built hours that start in [from, to) with their
	// items and one-time inserts.
	Plan(ctx context.Context, from, to time.Time) ([]PlannedHour, error)

	// Missed returns a missed item; outage.ErrMissedNotFound.
	Missed(ctx context.Context, id uint64) (MissedItem, error)
	// LockMissed returns a missed item locked for update;
	// outage.ErrMissedNotFound.
	LockMissed(ctx context.Context, id uint64) (MissedItem, error)
	// Inserts returns the one-time inserts of the hour, in ID order.
	Inserts(ctx context.Context, hourStart time.Time) ([]Insert, error)
	// Insert returns one one-time insert; ErrInsertNotFound.
	Insert(ctx context.Context, id uint64) (Insert, error)
	// CreateInsert stores a one-time insert and returns its ID.
	CreateInsert(ctx context.Context, ins Insert) (uint64, error)
	// UpdateInsert sets the target start and the tolerance of a one-time
	// insert; ErrInsertNotFound.
	UpdateInsert(ctx context.Context, ins Insert) error
	// PlanItem returns one planned item and its hour; ErrPlanItemNotFound.
	PlanItem(ctx context.Context, id uint64) (PlanItemRef, error)
	// DeleteInsert deletes a one-time insert.
	DeleteInsert(ctx context.Context, id uint64) error
	// ReopenMissed sets a rescheduled missed item open again.
	ReopenMissed(ctx context.Context, missedID uint64) error
	// LinkRescheduled resolves a missed item as rescheduled to the plan
	// row that airs it again (keeping the first resolved_at).
	LinkRescheduled(ctx context.Context, missedID uint64, at time.Time) error
}

// ClockSource reads a clock (*clocks.Service).
type ClockSource interface {
	Get(ctx context.Context, id uint64) (clocks.Clock, error)
}

// GridSource resolves the grid to hours (*grid.Service).
type GridSource interface {
	Hours(ctx context.Context, from, to time.Time) ([]grid.ScheduledHour, error)
}

// Operations starts and finishes operations (*operations.Tracker).
type Operations interface {
	Start(ctx context.Context, kind operations.Kind) (uint64, error)
	Execute(ctx context.Context, id uint64, component logging.Component, fn operations.Func) error
	// Checkpoint stores the progress of the operation that runs ctx.
	Checkpoint(ctx context.Context, kind operations.Kind, done, total int) error
}

// Publisher sends an event to the panels without blocking.
type Publisher interface {
	Publish(event any)
}

// TxRunner runs fn in one transaction (*db.DB).
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error
}
