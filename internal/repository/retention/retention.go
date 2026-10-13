// Package retention deletes expired rows for the retention rules. It
// implements retention.Store of the domain package with the same name;
// import it as retentionrepo next to the domain.
package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

const (
	// batchSize caps the rows of one DELETE, so locks stay short.
	batchSize = 5000
	// batchTimeout bounds one DELETE batch.
	batchTimeout = 30 * time.Second
)

// Store is the retention repository.
type Store struct {
	q         db.Querier
	batchSize int
}

// New returns a repository on the pool.
func New(q db.Querier) *Store { return &Store{q: q, batchSize: batchSize} }

// WithTx returns a copy of the repository bound to tx.
func (s *Store) WithTx(tx db.Tx) *Store { return &Store{q: tx.Pgx(), batchSize: s.batchSize} }

// deleteInBatches runs a DELETE of at most batchSize rows (parameters
// $1 = cutoff, $2 = batch size) until a batch deletes fewer rows. Each batch
// has its own deadline and commits on its own.
func (s *Store) deleteInBatches(ctx context.Context, what, query string, cutoff time.Time) (int64, error) {
	var total int64
	for {
		bctx, cancel := context.WithTimeout(ctx, batchTimeout)
		tag, err := s.q.Exec(bctx, query, cutoff.UTC(), s.batchSize)
		cancel()
		if err != nil {
			return total, fmt.Errorf("delete %s: %w", what, err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(s.batchSize) {
			return total, nil
		}
	}
}
