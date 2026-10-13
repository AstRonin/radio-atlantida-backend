//go:build integration

package settings_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	settingsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/settings"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
)

var t0 = time.Date(2026, 10, 13, 6, 44, 0, 0, time.UTC)

func TestStore(t *testing.T) {
	d := dbtest.Migrated(t)
	store := settingsrepo.New(d)
	ctx := t.Context()

	t.Run("nothing saved", func(t *testing.T) {
		all, err := store.List(ctx)
		require.NoError(t, err)
		require.Empty(t, all)

		_, err = store.Get(ctx, "liquidsoap.fade_out_ms")
		require.ErrorIs(t, err, settings.ErrNotFound)
	})

	t.Run("insert, then update the same key", func(t *testing.T) {
		st, err := store.Upsert(ctx, settings.Stored{Key: "liquidsoap.fade_out_ms", Value: json.RawMessage(`2600`), UpdatedAt: t0})
		require.NoError(t, err)
		require.Equal(t, t0, st.UpdatedAt)

		_, err = store.Upsert(ctx, settings.Stored{Key: "liquidsoap.fade_out_ms", Value: json.RawMessage(`2700`), UpdatedAt: t0.Add(time.Minute)})
		require.NoError(t, err)

		got, err := store.Get(ctx, "liquidsoap.fade_out_ms")
		require.NoError(t, err)
		require.JSONEq(t, `2700`, string(got.Value))
		require.Equal(t, t0.Add(time.Minute), got.UpdatedAt)
		require.Equal(t, time.UTC, got.UpdatedAt.Location())
	})

	t.Run("objects and null round-trip as JSON", func(t *testing.T) {
		_, err := store.Upsert(ctx, settings.Stored{Key: "filler.fallback", Value: json.RawMessage(`{"mode":"playlist","source_id":3}`), UpdatedAt: t0})
		require.NoError(t, err)
		_, err = store.Upsert(ctx, settings.Stored{Key: "playout.emergency_playlist_id", Value: json.RawMessage(`null`), UpdatedAt: t0})
		require.NoError(t, err)

		all, err := store.List(ctx)
		require.NoError(t, err)
		require.Len(t, all, 3)
		require.Equal(t, []string{"filler.fallback", "liquidsoap.fade_out_ms", "playout.emergency_playlist_id"},
			[]string{all[0].Key, all[1].Key, all[2].Key}, "key order")
		require.JSONEq(t, `{"mode":"playlist","source_id":3}`, string(all[0].Value))
		require.JSONEq(t, `null`, string(all[2].Value))

		var typ string
		require.NoError(t, d.Pool.QueryRow(ctx, `SELECT jsonb_typeof(value) FROM settings WHERE key = 'filler.fallback'`).Scan(&typ))
		require.Equal(t, "object", typ, "stored as jsonb, not as a JSON string")
	})

	t.Run("an invalid key is refused by the database", func(t *testing.T) {
		_, err := store.Upsert(ctx, settings.Stored{Key: "Bad Key", Value: json.RawMessage(`1`), UpdatedAt: t0})
		require.Error(t, err)
		ce, ok := db.AsConstraintError(err)
		require.True(t, ok)
		require.Equal(t, "ck_settings_key", ce.Constraint)
	})

	t.Run("inside a transaction", func(t *testing.T) {
		err := d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
			_, err := store.WithTx(tx).Upsert(ctx, settings.Stored{Key: "station.name", Value: json.RawMessage(`"X"`), UpdatedAt: t0})
			require.NoError(t, err)
			return context.Canceled // roll back
		})
		require.ErrorIs(t, err, context.Canceled)
		_, err = store.Get(ctx, "station.name")
		require.ErrorIs(t, err, settings.ErrNotFound, "rolled back")
	})
}
