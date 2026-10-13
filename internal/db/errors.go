package db

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// ConstraintKind is the kind of a violated constraint.
type ConstraintKind string

// Constraint kinds by SQLSTATE.
const (
	Unique     ConstraintKind = "unique"      // 23505
	ForeignKey ConstraintKind = "foreign_key" // 23503
	Check      ConstraintKind = "check"       // 23514
	Exclusion  ConstraintKind = "exclusion"   // 23P01
)

var constraintKinds = map[string]ConstraintKind{
	"23505": Unique,
	"23503": ForeignKey,
	"23514": Check,
	"23P01": Exclusion,
}

// ConstraintError is a constraint violation reported by PostgreSQL. A
// repository maps it to its domain's sentinel error by Constraint (the
// names follow pk_/fk_/uq_/ck_/ex_<table>_..., §1.2 of the init plan).
type ConstraintError struct {
	Kind       ConstraintKind
	Constraint string
	Table      string
	Err        error
}

func (e *ConstraintError) Error() string {
	return fmt.Sprintf("%s constraint %s violated: %v", e.Kind, e.Constraint, e.Err)
}

func (e *ConstraintError) Unwrap() error { return e.Err }

// AsConstraintError returns the constraint violation inside err, from pgx
// or GORM (which keeps the driver error wrapped). ok is false for any other
// error, which the repository wraps with context and returns unmapped.
func AsConstraintError(err error) (ce *ConstraintError, ok bool) {
	if errors.As(err, &ce) {
		return ce, true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil, false
	}
	kind, ok := constraintKinds[pgErr.Code]
	if !ok {
		return nil, false
	}
	return &ConstraintError{Kind: kind, Constraint: pgErr.ConstraintName, Table: pgErr.TableName, Err: err}, true
}
