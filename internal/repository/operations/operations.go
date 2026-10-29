// Package operations stores asynchronous operations in the operations table
// (§6.5 of the design). It implements operations.Store of the domain package
// with the same name; import it as operationsrepo next to the domain.
package operations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

// Store is the operations repository.
type Store struct {
	q db.Querier
}

// New returns a repository on the pool.
func New(q db.Querier) *Store { return &Store{q: q} }

// WithTx returns a copy of the repository bound to tx.
func (s *Store) WithTx(tx db.Tx) *Store { return &Store{q: tx.Pgx()} }

type row struct {
	ID            uint64     `db:"id"`
	Kind          string     `db:"kind"`
	Status        string     `db:"status"`
	StartedAt     time.Time  `db:"started_at"`
	FinishedAt    *time.Time `db:"finished_at"`
	ErrorMessage  *string    `db:"error_message"`
	ResultSummary *string    `db:"result_summary"`
	ProgressDone  *int32     `db:"progress_done"`
	ProgressTotal *int32     `db:"progress_total"`
}

func (r row) operation() operations.Operation {
	return operations.Operation{
		ID:            r.ID,
		Kind:          r.Kind,
		Status:        operations.Status(r.Status),
		StartedAt:     r.StartedAt,
		FinishedAt:    r.FinishedAt,
		ErrorMessage:  r.ErrorMessage,
		ResultSummary: r.ResultSummary,
		ProgressDone:  intPtr(r.ProgressDone),
		ProgressTotal: intPtr(r.ProgressTotal),
	}
}

func intPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	return new(int(*v))
}

const selectColumns = `SELECT id, kind, status, started_at, finished_at, error_message, result_summary,
       progress_done, progress_total FROM operations`

// Progress stores the progress of a running operation.
func (s *Store) Progress(ctx context.Context, id uint64, done, total int) error {
	if _, err := s.q.Exec(ctx, `
		UPDATE operations SET progress_done = $2, progress_total = $3
		WHERE id = $1 AND status = 'running'`,
		id, done, total,
	); err != nil {
		return fmt.Errorf("update progress of operation %d: %w", id, err)
	}
	return nil
}

// Insert stores a running operation and returns its ID.
func (s *Store) Insert(ctx context.Context, kind operations.Kind, startedAt time.Time) (uint64, error) {
	var id uint64
	err := s.q.QueryRow(ctx,
		`INSERT INTO operations (kind, status, started_at) VALUES ($1, 'running', $2) RETURNING id`,
		string(kind), startedAt.UTC(),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert operation: %w", err)
	}
	return id, nil
}

// Finish moves a running operation to its final state and returns its kind.
// It returns operations.ErrNotRunning if the operation is finished already or
// does not exist: the status condition makes the terminal state final.
func (s *Store) Finish(ctx context.Context, f operations.Finish) (operations.Kind, error) {
	var kind string
	err := s.q.QueryRow(ctx, `
		UPDATE operations
		SET status = $2, finished_at = $3, error_message = $4, result_summary = $5
		WHERE id = $1 AND status = 'running'
		RETURNING kind`,
		f.ID, string(f.Status), f.FinishedAt.UTC(), f.ErrorMessage, f.ResultSummary,
	).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", operations.ErrNotRunning
	}
	if err != nil {
		return "", fmt.Errorf("update operation %d: %w", f.ID, err)
	}
	return operations.Kind(kind), nil
}

// FailRunning fails every running operation with message and returns the
// number of rows changed.
func (s *Store) FailRunning(ctx context.Context, at time.Time, message string) (int64, error) {
	tag, err := s.q.Exec(ctx, `
		UPDATE operations
		SET status = 'failed', finished_at = $1, error_message = $2
		WHERE status = 'running'`,
		at.UTC(), message,
	)
	if err != nil {
		return 0, fmt.Errorf("fail running operations: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Get returns one operation, or operations.ErrNotFound.
func (s *Store) Get(ctx context.Context, id uint64) (operations.Operation, error) {
	rows, err := s.q.Query(ctx, selectColumns+` WHERE id = $1`, id)
	if err != nil {
		return operations.Operation{}, fmt.Errorf("select operation %d: %w", id, err)
	}
	op, err := db.CollectOne(rows, row.operation, operations.ErrNotFound)
	if err != nil {
		return operations.Operation{}, fmt.Errorf("select operation %d: %w", id, err)
	}
	return op, nil
}

// List returns operations in ID order. The table holds at most the retention
// period of rows, so it has no index besides the primary key.
func (s *Store) List(ctx context.Context, f operations.ListFilter) ([]operations.Operation, error) {
	rows, err := s.q.Query(ctx, selectColumns+`
		WHERE id > $1 AND (started_at >= $2 OR finished_at >= $2)
		ORDER BY id
		LIMIT $3`,
		f.AfterID, f.Since.UTC(), f.Limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select operations: %w", err)
	}
	ops, err := db.CollectAll(rows, row.operation)
	if err != nil {
		return nil, fmt.Errorf("select operations: %w", err)
	}
	return ops, nil
}
