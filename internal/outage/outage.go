// Package outage records broadcast interruptions and what they kept off the
// air (§7.1 of the design; decisions in docs/phase-10-plan.md §2).
//
// The Detector watches three signals (the Icecast mount, Liquidsoap's
// callbacks, the control link), opens an outage when one fails and closes
// it when all are up again. When an outage closes, the plan items of its
// window that never started are moved to the play log as missed. Items that
// did not air while the stream was fine are dropped, like tracks removed by
// overflow. Liquidsoap's playout journal decides what aired: before
// marking or dropping, the journal of the window is replayed, so a start
// whose callback was lost is recorded. After the backend itself was down,
// the startup reconcile replays the journal and records the downtime as an
// outage when planned items did not air.
//
// The Service serves the played-tracks list, the missed items (list and
// dismiss; rescheduling is in internal/rotation), and the outage periods.
//
// Persistence goes through internal/repository/outage.
package outage

import (
	"context"
	"errors"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

// Errors of the missed items.
var (
	ErrMissedNotFound = errors.New("missed item not found")
	// ErrMissedResolved: the missed item was already rescheduled or
	// dismissed.
	ErrMissedResolved = errors.New("missed item is already resolved")
	// ErrOutageOpen: another outage is open (uq_broadcast_outages_open).
	ErrOutageOpen = errors.New("an outage is already open")
)

// Component is the part of the chain that failed
// (ck_broadcast_outages_component).
type Component string

// Components.
const (
	ComponentIcecast    Component = "icecast"
	ComponentLiquidsoap Component = "liquidsoap"
	ComponentBackend    Component = "backend"
	ComponentNetwork    Component = "network"
)

// DetectedBy is the signal that opened an outage
// (ck_broadcast_outages_detected_by).
type DetectedBy string

// Signals.
const (
	DetectedByIcecast            DetectedBy = "icecast"
	DetectedByLiquidsoapCallback DetectedBy = "liquidsoap_callback"
	DetectedByLiquidsoapControl  DetectedBy = "liquidsoap_control"
	DetectedByBackend            DetectedBy = "backend"
)

// Outage is a broadcast interruption.
type Outage struct {
	ID        uint64
	StartedAt time.Time
	// EndedAt is nil while the outage is open.
	EndedAt    *time.Time
	DetectedBy DetectedBy
	Component  Component
	Reason     *string
	// ReconciledAt is when its missed items were marked.
	ReconciledAt *time.Time
	// MissedCount is the number of items marked missed (lists only).
	MissedCount int
}

// NewOutage is an outage to record.
type NewOutage struct {
	StartedAt  time.Time
	DetectedBy DetectedBy
	Component  Component
	Reason     string
}

// Status of a play log row.
type Status string

// Statuses (ck_play_log_status).
const (
	StatusAired     Status = "aired"
	StatusTruncated Status = "truncated"
	StatusMissed    Status = "missed"
)

// Resolution of a missed item.
type Resolution string

// Resolutions (ck_play_log_resolution).
const (
	ResolutionOpen        Resolution = "open"
	ResolutionRescheduled Resolution = "rescheduled"
	ResolutionDismissed   Resolution = "dismissed"
)

// Entry is a row of the played-tracks list: an aired, truncated, or missed
// item.
type Entry struct {
	ID         uint64
	Status     Status
	Source     string
	TrackID    uint64
	Title      string
	ArtistName *string
	// PlannedStart is nil for emergency tracks, which were never planned.
	PlannedStart *time.Time
	// StartedAt and ActualDurationMS are nil for missed items.
	StartedAt *time.Time
	// PlannedInMS and PlannedOutMS are the planned cut in the file; the
	// planned length is their difference.
	PlannedInMS      int
	PlannedOutMS     *int
	ActualDurationMS *int
	ClockID          *uint64
	ClockName        *string
	// Outage is set for missed items.
	Outage         *EntryOutage
	Resolution     *Resolution
	ResolvedAt     *time.Time
	ResolvedItemID *uint64
	// SortTime is when the row aired, or was planned to (the list order).
	SortTime time.Time
}

// EntryOutage is the outage of a missed item.
type EntryOutage struct {
	ID        uint64
	Component Component
	Reason    *string
}

// HistoryFilter selects the played-tracks list: rows whose SortTime is in
// [From, To), newest first, after the cursor.
type HistoryFilter struct {
	From, To time.Time
	Status   *Status
	Limit    int
	After    *Cursor
}

// Cursor is the keyset position of the last row of a page.
type Cursor struct {
	SortTime time.Time
	ID       uint64
}

// Store persists outages and resolves the plan items they affect
// (internal/repository/outage).
type Store interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) Store
	// LockPlan takes the transaction-level advisory lock of plan writes,
	// the one the planner and the pusher hold.
	LockPlan(ctx context.Context) error

	// OpenOutage returns the open outage; false if there is none.
	OpenOutage(ctx context.Context) (Outage, bool, error)
	// CreateOutage records an outage, open when endedAt is nil.
	// ErrOutageOpen when it is open and another one is.
	CreateOutage(ctx context.Context, o NewOutage, endedAt *time.Time) (Outage, error)
	// CloseOutage sets the end of an open outage.
	CloseOutage(ctx context.Context, id uint64, endedAt time.Time) error
	// Unmarked returns the closed outages whose missed items are not
	// marked yet, oldest first.
	Unmarked(ctx context.Context) ([]Outage, error)
	// MarkMissed moves the planned and queued items of the outage window
	// that start before before to the play log as missed (§4.1) and returns
	// how many.
	MarkMissed(ctx context.Context, o Outage, before time.Time) (int, error)
	// SetReconciled records that the outage's missed items are marked.
	SetReconciled(ctx context.Context, id uint64, at time.Time) error

	// OldestUnaired returns the planned start of the oldest planned or
	// queued item that starts before before; false if there is none.
	OldestUnaired(ctx context.Context, before time.Time) (time.Time, bool, error)
	// CountUnaired counts the planned and queued items that start in
	// [from, to).
	CountUnaired(ctx context.Context, from, to time.Time) (int, error)
	// DropStale deletes the planned and queued items that start before
	// before and overlap no open or unmarked outage, releases their pool
	// rows, and returns how many.
	DropStale(ctx context.Context, before time.Time) (int, error)
	// ItemsDue reports whether a planned or queued item plays within
	// [from, to).
	ItemsDue(ctx context.Context, from, to time.Time) (bool, error)

	// Heartbeat returns when the backend was last alive; false before the
	// first heartbeat.
	Heartbeat(ctx context.Context) (time.Time, bool, error)
	// SetHeartbeat records that the backend is alive at at.
	SetHeartbeat(ctx context.Context, at time.Time) error

	// History returns a page of the played-tracks list.
	History(ctx context.Context, f HistoryFilter) ([]Entry, error)
	// Missed returns the missed items with the resolution, newest planned
	// first, at most limit.
	Missed(ctx context.Context, r Resolution, limit int) ([]Entry, error)
	// Dismiss resolves an open missed item as dismissed.
	// ErrMissedNotFound, ErrMissedResolved.
	Dismiss(ctx context.Context, id uint64, at time.Time) (Entry, error)
	// Outages returns the outages that overlap [from, to), newest first,
	// with their missed counts.
	Outages(ctx context.Context, from, to time.Time) ([]Outage, error)
}

// TxRunner runs fn in one transaction (*db.DB).
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error
}

// Publisher sends an event to the panels without blocking.
type Publisher interface {
	Publish(event any)
}
