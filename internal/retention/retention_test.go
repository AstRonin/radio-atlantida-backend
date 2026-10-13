package retention_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/retention"
)

type fakeStore struct {
	playLogCutoff, opsCutoff time.Time
	playLogErr               error
}

func (f *fakeStore) DeletePlayLog(_ context.Context, cutoff time.Time) (int64, error) {
	f.playLogCutoff = cutoff
	return 4, f.playLogErr
}

func (f *fakeStore) DeleteOperations(_ context.Context, cutoff time.Time) (int64, error) {
	f.opsCutoff = cutoff
	return 1, nil
}

func TestRun(t *testing.T) {
	t.Parallel()
	// A non-UTC clock: cutoffs are still UTC instants.
	yerevan := time.FixedZone("Asia/Yerevan", 4*3600)
	now := time.Date(2026, 10, 13, 7, 0, 0, 0, yerevan)
	store := &fakeStore{}

	summary, err := retention.New(store, func() time.Time { return now }).Run(t.Context())

	require.NoError(t, err)
	require.Equal(t, "deleted 4 play log rows and 1 operations", summary)
	want := time.Date(2026, 10, 3, 3, 0, 0, 0, time.UTC)
	require.Equal(t, want, store.playLogCutoff)
	require.Equal(t, time.UTC, store.playLogCutoff.Location())
	require.Equal(t, want, store.opsCutoff)
}

func TestRunStopsOnError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	store := &fakeStore{playLogErr: boom}

	_, err := retention.New(store, time.Now).Run(t.Context())

	require.ErrorIs(t, err, boom)
	require.True(t, store.opsCutoff.IsZero(), "operations are not touched after a failure")
}
