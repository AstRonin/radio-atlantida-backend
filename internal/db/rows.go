package db

import (
	"errors"

	"github.com/jackc/pgx/v5"
)

// CollectOne scans exactly one row into R (by column name) and converts it
// with conv. No row returns notFound, the domain's sentinel, so the domain
// never sees pgx.ErrNoRows. Other errors are returned as they are; the
// repository wraps them with context.
func CollectOne[R, T any](rows pgx.Rows, conv func(R) T, notFound error) (T, error) {
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[R])
	if errors.Is(err, pgx.ErrNoRows) {
		var zero T
		return zero, notFound
	}
	if err != nil {
		var zero T
		return zero, err
	}
	return conv(r), nil
}

// CollectAll scans every row into R (by column name) and converts each with
// conv.
func CollectAll[R, T any](rows pgx.Rows, conv func(R) T) ([]T, error) {
	rs, err := pgx.CollectRows(rows, pgx.RowToStructByName[R])
	if err != nil {
		return nil, err
	}
	out := make([]T, len(rs))
	for i, r := range rs {
		out[i] = conv(r)
	}
	return out, nil
}
