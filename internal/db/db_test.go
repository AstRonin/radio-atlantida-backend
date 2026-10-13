package db

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOpenPoolHidesInvalidURL(t *testing.T) {
	t.Parallel()

	_, err := OpenPool(t.Context(), "postgres://user:s3cret@host:notaport/db")

	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cret")
}

func TestAsConstraintError(t *testing.T) {
	t.Parallel()
	pg := func(code string) error {
		return fmt.Errorf("insert: %w", &pgconn.PgError{Code: code, ConstraintName: "c", TableName: "t"})
	}
	tests := []struct {
		name   string
		err    error
		wantOK bool
		want   ConstraintKind
	}{
		{"unique", pg("23505"), true, Unique},
		{"foreign key", pg("23503"), true, ForeignKey},
		{"check", pg("23514"), true, Check},
		{"exclusion", pg("23P01"), true, Exclusion},
		{"GORM keeps the driver error", fmt.Errorf("%w: %w", gorm.ErrDuplicatedKey, &pgconn.PgError{Code: "23505"}), true, Unique},
		{"not null is not mapped", pg("23502"), false, ""},
		{"other error", errors.New("boom"), false, ""},
		{"nil", nil, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ce, ok := AsConstraintError(tt.err)

			require.Equal(t, tt.wantOK, ok)
			if !ok {
				return
			}
			require.Equal(t, tt.want, ce.Kind)
			require.ErrorIs(t, ce, tt.err, "the original error stays reachable")
			again, ok := AsConstraintError(fmt.Errorf("repo: %w", ce))
			require.True(t, ok)
			require.Same(t, ce, again)
		})
	}
}
