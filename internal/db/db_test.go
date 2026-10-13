package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenPoolHidesInvalidURL(t *testing.T) {
	t.Parallel()

	_, err := OpenPool(t.Context(), "postgres://user:s3cret@host:notaport/db")

	require.Error(t, err)
	require.NotContains(t, err.Error(), "s3cret")
}
