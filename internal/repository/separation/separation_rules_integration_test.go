//go:build integration

package separation_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	separationrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/separation"
	"github.com/AstRonin/radio-atlantida-backend/internal/separation"
)

func TestStore(t *testing.T) {
	d := dbtest.Migrated(t)
	store := separationrepo.New(d)
	ctx := t.Context()

	rules, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, rules, 2, "seeded by 0001_init")
	require.Equal(t, separation.ScopeTrack, rules[0].Scope)
	require.Equal(t, separation.ScopeArtist, rules[1].Scope)

	require.NoError(t, store.Update(ctx, rules[1].ID, 48, "reference value"))
	got, err := store.Get(ctx, rules[1].ID)
	require.NoError(t, err)
	require.Equal(t, separation.Rule{ID: rules[1].ID, Scope: separation.ScopeArtist, MinIntervalHours: 48, Notes: "reference value"}, got)

	// Saving the same values again still finds the row.
	require.NoError(t, store.Update(ctx, rules[1].ID, 48, "reference value"))
	require.ErrorIs(t, store.Update(ctx, 9999, 48, ""), separation.ErrNotFound)
	_, err = store.Get(ctx, 9999)
	require.ErrorIs(t, err, separation.ErrNotFound)
}
