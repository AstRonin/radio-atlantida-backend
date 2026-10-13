package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenPoolHidesInvalidURL(t *testing.T) {
	t.Parallel()

	_, err := OpenPool(context.Background(), "postgres://user:s3cret@host:notaport/db")

	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cret")
}
