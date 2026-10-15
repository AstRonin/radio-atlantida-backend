// Package broadcast derives whether the stream reaches Icecast, and since
// when (docs/phase-16-plan.md §2). The Tracker combines Liquidsoap's output
// reports, the outage detector's open outage, its Icecast mount checks, and
// Liquidsoap's heartbeat into one state. The moment of its last change is
// stored, so the panel's timer survives a restart of the backend.
//
// Persistence goes through internal/repository/broadcast.
package broadcast

import (
	"context"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

// State is the derived broadcast state.
type State string

// States. Unknown is never stored (ck_broadcast_state_state).
const (
	// StateConnected: Liquidsoap streams to Icecast and no outage is open.
	StateConnected State = "connected"
	// StateReconnecting: the output is lost and no outage is open yet;
	// Liquidsoap retries on its own.
	StateReconnecting State = "reconnecting"
	// StateUnreachable: a broadcast outage is open; listeners hear nothing.
	StateUnreachable State = "unreachable"
	// StateUnknown: no evidence yet after a start, or Liquidsoap's heartbeat
	// is silent.
	StateUnknown State = "unknown"
)

// Record is the stored state: the row of broadcast_state.
type Record struct {
	State State
	// Since is the moment of the last state change (UTC).
	Since time.Time
	// Reason is the open outage's reason or component (unreachable only).
	Reason    *string
	UpdatedAt time.Time
}

// Outage is the open outage as the Tracker needs it.
type Outage struct {
	ID        uint64
	StartedAt time.Time
	// Reason is the outage's reason, or its component when it has none.
	Reason string
}

// Status is the current state for the panel.
type Status struct {
	State State
	// Since is nil while the state is unknown.
	Since  *time.Time
	Reason *string
	// OutageID is the open outage (unreachable only).
	OutageID *uint64
	// MountOK is the result of the last Icecast mount check; nil before the
	// first.
	MountOK     *bool
	LastCheckAt *time.Time
}

// Store persists the state (internal/repository/broadcast).
type Store interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) Store
	// Get returns the stored state; false when none is stored.
	Get(ctx context.Context) (Record, bool, error)
	// Save stores the state (upsert of the one row).
	Save(ctx context.Context, r Record) error
}

// TxRunner runs fn in one transaction (*db.DB).
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error
}

// Publisher sends an event to the panels without blocking.
type Publisher interface {
	Publish(event any)
}
