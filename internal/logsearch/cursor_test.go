package logsearch

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCursorRoundTripsNanoseconds(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 10, 27, 10, 15, 30, 123456789, time.UTC)
	got, err := decodeCursor(encodeCursor(ts))
	require.NoError(t, err)
	require.True(t, ts.Equal(got))
	require.Equal(t, time.UTC, got.Location())
}

func TestCursorIsOpaqueURLSafe(t *testing.T) {
	t.Parallel()
	c := encodeCursor(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC))
	require.NotContains(t, c, "=")
	require.NotContains(t, c, "+")
	require.NotContains(t, c, "/")
}

func TestDecodeCursorRefusesForeignValues(t *testing.T) {
	t.Parallel()
	for _, c := range []string{
		"",
		"%%%",
		base64.RawURLEncoding.EncodeToString([]byte("v2:123")),
		base64.RawURLEncoding.EncodeToString([]byte("v1:not-a-number")),
		base64.RawURLEncoding.EncodeToString([]byte("123")),
	} {
		_, err := decodeCursor(c)
		require.Error(t, err, c)
	}
}
