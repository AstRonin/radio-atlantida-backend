// Package jobs holds the advisory locks that keep scheduled jobs singleton.
// It implements jobs.Locker of the domain package with the same name; import
// it as jobsrepo next to the domain.
package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstRonin/radio-atlantida-backend/internal/jobs"
)

// unlockTimeout bounds the unlock, which runs even after the run's context
// is cancelled.
const unlockTimeout = 5 * time.Second

// Locks takes session-level advisory locks.
//
// It has no WithTx: a session lock belongs to a connection, not to a
// transaction, and it must outlive the transactions of the run it guards.
type Locks struct {
	pool *pgxpool.Pool
}

// NewLocks returns the lock repository on pool.
func NewLocks(pool *pgxpool.Pool) *Locks { return &Locks{pool: pool} }

// WithLock runs fn while holding the advisory lock on key, on a dedicated
// connection, or returns jobs.ErrLocked if another session holds it. The
// connection is held for the whole run, so fn uses other connections from
// the pool.
func (l *Locks) WithLock(ctx context.Context, key int64, fn func() error) error {
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for lock: %w", err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		return fmt.Errorf("take advisory lock: %w", err)
	}
	if !locked {
		return jobs.ErrLocked
	}
	defer func() {
		// Unlock even when ctx is cancelled.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
			// Drop the connection instead; the lock goes with it.
			_ = conn.Conn().Close(uctx)
		}
	}()
	return fn()
}
