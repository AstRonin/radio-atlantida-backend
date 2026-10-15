// Package broadcast is the pgx store of internal/broadcast: the one row of
// the stored broadcast state (§5.1).
package broadcast

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/AstRonin/radio-atlantida-backend/internal/broadcast"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

// Store implements broadcast.Store.
type Store struct {
	q db.Querier
}

// New returns the store.
func New(q db.Querier) *Store { return &Store{q: q} }

// WithTx returns the store bound to tx.
func (s *Store) WithTx(tx db.Tx) broadcast.Store { return &Store{q: tx.Pgx()} }

// Get returns the stored state; false when the table is empty.
func (s *Store) Get(ctx context.Context) (broadcast.Record, bool, error) {
	var (
		r     broadcast.Record
		state string
	)
	err := s.q.QueryRow(ctx, `SELECT state, since, reason, updated_at FROM broadcast_state WHERE id = 1`).
		Scan(&state, &r.Since, &r.Reason, &r.UpdatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return broadcast.Record{}, false, nil
	case err != nil:
		return broadcast.Record{}, false, fmt.Errorf("select broadcast state: %w", err)
	}
	r.State = broadcast.State(state)
	r.Since, r.UpdatedAt = r.Since.UTC(), r.UpdatedAt.UTC()
	return r, true, nil
}

// Save stores the state in row 1.
func (s *Store) Save(ctx context.Context, r broadcast.Record) error {
	_, err := s.q.Exec(ctx, `INSERT INTO broadcast_state (id, state, since, reason, updated_at)
VALUES (1, $1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE SET state = EXCLUDED.state, since = EXCLUDED.since, reason = EXCLUDED.reason,
    updated_at = EXCLUDED.updated_at`,
		string(r.State), r.Since.UTC(), r.Reason, r.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("upsert broadcast state: %w", err)
	}
	return nil
}
