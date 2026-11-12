//go:build integration

package settings_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	settingsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/settings"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
)

var t0 = time.Date(2026, 10, 13, 6, 44, 0, 0, time.UTC)

// upsert saves one value.
func upsert(ctx context.Context, store interface {
	UpsertMany(context.Context, []settings.Stored) ([]settings.Stored, error)
}, st settings.Stored,
) (settings.Stored, error) {
	out, err := store.UpsertMany(ctx, []settings.Stored{st})
	if err != nil {
		return settings.Stored{}, err
	}
	return out[0], nil
}

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
		st, err := upsert(ctx, store, settings.Stored{Key: "liquidsoap.fade_out_ms", Value: json.RawMessage(`2600`), UpdatedAt: t0})
		require.NoError(t, err)
		require.Equal(t, t0, st.UpdatedAt)

		_, err = upsert(ctx, store, settings.Stored{Key: "liquidsoap.fade_out_ms", Value: json.RawMessage(`2700`), UpdatedAt: t0.Add(time.Minute)})
		require.NoError(t, err)

		got, err := store.Get(ctx, "liquidsoap.fade_out_ms")
		require.NoError(t, err)
		require.JSONEq(t, `2700`, string(got.Value))
		require.Equal(t, t0.Add(time.Minute), got.UpdatedAt)
		require.Equal(t, time.UTC, got.UpdatedAt.Location())
	})

	t.Run("objects and null round-trip as JSON", func(t *testing.T) {
		_, err := upsert(ctx, store, settings.Stored{Key: "test.object", Value: json.RawMessage(`{"mode":"playlist","source_id":3}`), UpdatedAt: t0})
		require.NoError(t, err)
		_, err = upsert(ctx, store, settings.Stored{Key: "playout.emergency_playlist_id", Value: json.RawMessage(`null`), UpdatedAt: t0})
		require.NoError(t, err)

		all, err := store.List(ctx)
		require.NoError(t, err)
		require.Len(t, all, 3)
		require.Equal(t, []string{"liquidsoap.fade_out_ms", "playout.emergency_playlist_id", "test.object"},
			[]string{all[0].Key, all[1].Key, all[2].Key}, "key order")
		require.JSONEq(t, `{"mode":"playlist","source_id":3}`, string(all[2].Value))
		require.JSONEq(t, `null`, string(all[1].Value))

		var typ string
		require.NoError(t, d.Pool.QueryRow(ctx, `SELECT jsonb_typeof(value) FROM settings WHERE key = 'test.object'`).Scan(&typ))
		require.Equal(t, "object", typ, "stored as jsonb, not as a JSON string")
	})

	t.Run("an invalid key is refused by the database", func(t *testing.T) {
		_, err := upsert(ctx, store, settings.Stored{Key: "Bad Key", Value: json.RawMessage(`1`), UpdatedAt: t0})
		require.Error(t, err)
		ce, ok := db.AsConstraintError(err)
		require.True(t, ok)
		require.Equal(t, "ck_settings_key", ce.Constraint)
	})

	t.Run("inside a transaction", func(t *testing.T) {
		err := d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
			_, err := upsert(ctx, store.WithTx(tx), settings.Stored{Key: "station.name", Value: json.RawMessage(`"X"`), UpdatedAt: t0})
			require.NoError(t, err)
			return context.Canceled // roll back
		})
		require.ErrorIs(t, err, context.Canceled)
		_, err = store.Get(ctx, "station.name")
		require.ErrorIs(t, err, settings.ErrNotFound, "rolled back")
	})

	t.Run("a batch is one statement: all rows or none", func(t *testing.T) {
		at := t0.Add(time.Hour)
		out, err := store.UpsertMany(ctx, []settings.Stored{
			{Key: "stream.codec", Value: json.RawMessage(`"vorbis"`), UpdatedAt: at},
			{Key: "stream.bitrate_kbps", Value: json.RawMessage(`192`), UpdatedAt: at},
			{Key: "liquidsoap.fade_out_ms", Value: json.RawMessage(`2800`), UpdatedAt: at},
		})
		require.NoError(t, err)
		require.Len(t, out, 3)
		var n int
		require.NoError(t, d.Pool.QueryRow(ctx,
			`SELECT count(*) FROM settings WHERE updated_at = $1 AND key IN ('stream.codec', 'stream.bitrate_kbps', 'liquidsoap.fade_out_ms')`, at).Scan(&n))
		require.Equal(t, 3, n, "every row with one updated_at")
		got, err := store.Get(ctx, "liquidsoap.fade_out_ms")
		require.NoError(t, err)
		require.JSONEq(t, `2800`, string(got.Value), "an existing key is updated")

		// A row the database refuses fails the whole statement.
		_, err = store.UpsertMany(ctx, []settings.Stored{
			{Key: "station.name", Value: json.RawMessage(`"Y"`), UpdatedAt: at},
			{Key: "Bad Key", Value: json.RawMessage(`1`), UpdatedAt: at},
		})
		require.Error(t, err)
		_, err = store.Get(ctx, "station.name")
		require.ErrorIs(t, err, settings.ErrNotFound, "the good row was not saved either")

		empty, err := store.UpsertMany(ctx, nil)
		require.NoError(t, err)
		require.Empty(t, empty)
	})
}

// The service on the real store: a batch with one invalid value saves
// nothing, the earlier values stay; a valid batch is saved with one
// notification.
func TestServiceBatchOnStore(t *testing.T) {
	d := dbtest.Migrated(t)
	ctx := t.Context()
	svc, err := settings.NewService(settings.NewRegistry(), settingsrepo.New(d),
		map[string]bool{"ICECAST_SOURCE_PASSWORD": true, "LIQUIDSOAP_CONTROL_USER": true, "LIQUIDSOAP_CONTROL_PASSWORD": true},
		slog.New(slog.DiscardHandler), func() time.Time { return t0 })
	require.NoError(t, err)
	var calls [][]settings.Change
	svc.Subscribe(func(cs []settings.Change) { calls = append(calls, cs) })

	_, err = svc.SetMany(ctx, map[string]json.RawMessage{
		"stream.codec": json.RawMessage(`"aac"`), "station.name": json.RawMessage(`"Before"`),
	})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Len(t, calls[0], 2)

	_, err = svc.SetMany(ctx, map[string]json.RawMessage{
		"stream.codec":           json.RawMessage(`"vorbis"`),
		"station.name":           json.RawMessage(`"After"`),
		"liquidsoap.fade_out_ms": json.RawMessage(`1`),
	})
	var ive *settings.InvalidValuesError
	require.ErrorAs(t, err, &ive)
	require.Len(t, calls, 1, "no notification")

	codec, err := settings.Value(ctx, svc, settings.StreamCodec)
	require.NoError(t, err)
	require.Equal(t, "aac", codec, "the earlier value stays")
	name, err := settings.Value(ctx, svc, settings.StationName)
	require.NoError(t, err)
	require.Equal(t, "Before", name)
	var n int
	require.NoError(t, d.Pool.QueryRow(ctx, `SELECT count(*) FROM settings`).Scan(&n))
	require.Equal(t, 2, n)
}
