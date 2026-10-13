package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/gorm"
)

// Querier runs pgx statements. The pool, a pgx.Tx, and the connection of a
// Tx implement it, so a repository runs the same code inside and outside a
// transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, src pgx.CopyFromSource) (int64, error)
}

// Tx is one open transaction, usable through pgx and GORM at once: both
// handles run on the same connection, so a pgx write is visible to a GORM
// read in the same Tx and both commit or roll back together (§5.1).
//
// A Tx is valid only inside the function passed to WithTx, and only in the
// goroutine that runs it.
type Tx struct {
	pgx  Querier
	gorm *gorm.DB
}

// Pgx returns the pgx handle of the transaction.
func (t Tx) Pgx() Querier { return t.pgx }

// Gorm returns the GORM handle of the transaction.
func (t Tx) Gorm() *gorm.DB { return t.gorm }

// WithTx runs fn in one transaction (READ COMMITTED). The transaction is
// committed when fn returns nil and rolled back when it returns an error or
// panics. The domain service opens it and passes tx to each repository
// through its WithTx method; repositories never begin or commit. fn must not
// make network calls (§1.2 of the init plan).
func (d *DB) WithTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) (err error) {
	// GORM can only run on database/sql, so the transaction is begun there
	// and pgx reaches the same connection underneath it.
	conn, err := d.sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("tx: acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	pc, err := pgxConn(conn)
	if err != nil {
		return fmt.Errorf("tx: %w", err)
	}
	sqlTx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("tx: begin: %w", err)
	}

	gtx := d.Gorm.Session(&gorm.Session{Context: ctx, NewDB: true})
	gtx.Statement.ConnPool = sqlTx

	defer func() {
		if p := recover(); p != nil {
			_ = sqlTx.Rollback()
			panic(p)
		}
	}()
	if err := fn(ctx, Tx{pgx: pc, gorm: gtx}); err != nil {
		if rbErr := sqlTx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("tx: %w (rollback: %w)", err, rbErr)
		}
		return fmt.Errorf("tx: %w", err)
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("tx: commit: %w", err)
	}
	return nil
}

// pgxConn returns the pgx connection under conn.
//
// database/sql asks that the driver connection is used only inside Raw. It is
// used after Raw here because WithTx holds conn exclusively until the
// transaction ends, and a transaction runs its statements one at a time, so
// database/sql never touches the connection concurrently with pgx. The
// handle is dropped before conn is released.
func pgxConn(conn *sql.Conn) (*pgx.Conn, error) {
	var pc *pgx.Conn
	err := conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("driver connection is %T, not pgx", driverConn)
		}
		pc = c.Conn()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get pgx connection: %w", err)
	}
	return pc, nil
}
