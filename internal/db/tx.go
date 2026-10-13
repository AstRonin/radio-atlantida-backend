package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
)

// TxBeginner starts a pgx transaction. *pgxpool.Pool and pgx.Tx (for a
// savepoint) implement it.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// WithTx runs fn in a pgx transaction (READ COMMITTED). The transaction is
// committed when fn returns nil and rolled back when it returns an error or
// panics. fn must not make network calls (§1.2 of the init plan).
func WithTx(ctx context.Context, db TxBeginner, fn func(pgx.Tx) error) error {
	if err := pgx.BeginFunc(ctx, db, fn); err != nil {
		return fmt.Errorf("tx: %w", err)
	}
	return nil
}

// WithGormTx runs fn in a GORM transaction bound to ctx, with the same
// commit and rollback rules as WithTx. Use the tx passed to fn, not the
// outer handle, for every statement inside.
func WithGormTx(ctx context.Context, db *gorm.DB, fn func(tx *gorm.DB) error) error {
	if err := db.WithContext(ctx).Transaction(fn); err != nil {
		return fmt.Errorf("tx: %w", err)
	}
	return nil
}
