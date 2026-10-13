package separation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

type fakeStore struct {
	rule Rule
}

func (f *fakeStore) List(context.Context) ([]Rule, error) { return []Rule{f.rule}, nil }

func (f *fakeStore) Get(context.Context, int64) (Rule, error) { return f.rule, nil }

func (f *fakeStore) Update(_ context.Context, _ int64, hours int, notes string) error {
	f.rule.MinIntervalHours, f.rule.Notes = hours, notes
	return nil
}

func TestUpdateIsCappedByRetention(t *testing.T) {
	t.Parallel()
	store := &fakeStore{rule: Rule{ID: 1, Scope: ScopeTrack, MinIntervalHours: 72}}
	svc := NewService(store, func(context.Context) (int, error) { return 240, nil })

	r, err := svc.Update(t.Context(), 1, 240, " ref ")
	require.NoError(t, err)
	require.Equal(t, 240, r.MinIntervalHours)
	require.Equal(t, "ref", r.Notes)

	for _, hours := range []int{0, 241} {
		_, err = svc.Update(t.Context(), 1, hours, "")
		var ve *validation.Error
		require.ErrorAs(t, err, &ve, hours)
		require.Equal(t, "min_interval_hours", ve.Fields[0].Field)
		require.Equal(t, validation.Range, ve.Fields[0].Rule)
	}
	require.Equal(t, 240, store.rule.MinIntervalHours, "refused values are not saved")
}
