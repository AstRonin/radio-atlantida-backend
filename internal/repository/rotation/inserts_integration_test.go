//go:build integration

package rotation_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

func TestInsertsAndRescheduledMissedItems(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	o := f.id(`INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component)
VALUES ($1, $1, 'icecast', 'icecast') RETURNING id`, hour.Add(-time.Hour))
	missed := f.id(`INSERT INTO play_log (track_id, status, planned_start_utc, outage_id, resolution)
VALUES ($1, 'missed', $2, $3, 'open') RETURNING id`, f.tracks[2], hour.Add(-time.Hour), o)

	_, err := f.store.Missed(ctx, 999999)
	require.ErrorIs(t, err, outage.ErrMissedNotFound)
	m, err := f.store.Missed(ctx, missed)
	require.NoError(t, err)
	require.Equal(t, rotation.MissedItem{ID: missed, TrackID: f.tracks[2], Resolution: outage.ResolutionOpen}, m)

	target := hour.Add(2 * time.Hour)
	var insertID uint64
	f.inTx(func(ctx context.Context, st rotation.Store) error {
		locked, lockErr := st.LockMissed(ctx, missed)
		require.NoError(t, lockErr)
		var createErr error
		insertID, createErr = st.CreateInsert(ctx, rotation.Insert{
			HourStart: target, TrackID: locked.TrackID, MissedID: &locked.ID, Placement: clocks.PlacementFixed,
			FixedMinute: new(30), FixedToleranceMS: 60000,
		})
		return createErr
	})
	ins, err := f.store.Inserts(ctx, target)
	require.NoError(t, err)
	require.Equal(t, []rotation.Insert{{
		ID: insertID, HourStart: target, TrackID: f.tracks[2], MissedID: &missed, Placement: clocks.PlacementFixed,
		FixedMinute: new(30), FixedToleranceMS: 60000,
	}}, ins)

	// The build writes the reschedule item; the link resolves the missed row.
	f.inTx(func(ctx context.Context, st rotation.Store) error {
		return st.ReplaceHour(ctx, f.planHour(target, nil), []rotation.Item{{
			TrackID: f.tracks[2], OffsetMS: 30 * 60000, OutMS: 30000, Source: rotation.SourceReschedule, RescheduleOfID: &missed,
		}})
	})
	require.NoError(t, f.store.LinkRescheduled(ctx, missed, hour))
	var resolution string
	var resolvedAt time.Time
	var itemID, planned uint64
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT resolution, resolved_at, resolved_item_id FROM play_log WHERE id = $1`, missed).
		Scan(&resolution, &resolvedAt, &itemID))
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT id FROM scheduled_items WHERE reschedule_of_id = $1`, missed).Scan(&planned))
	require.Equal(t, "rescheduled", resolution)
	require.Equal(t, planned, itemID)

	// A rebuild links the new row and keeps the first resolution time.
	f.inTx(func(ctx context.Context, st rotation.Store) error {
		return st.ReplaceHour(ctx, f.planHour(target, nil), []rotation.Item{{
			TrackID: f.tracks[2], OffsetMS: 31 * 60000, OutMS: 30000, Source: rotation.SourceReschedule, RescheduleOfID: &missed,
		}})
	})
	require.NoError(t, f.store.LinkRescheduled(ctx, missed, hour.Add(time.Hour)))
	var at2 time.Time
	var item2 uint64
	require.NoError(t, f.d.Pool.QueryRow(ctx, `SELECT resolved_at, resolved_item_id FROM play_log WHERE id = $1`, missed).Scan(&at2, &item2))
	require.True(t, resolvedAt.Equal(at2))
	require.NotEqual(t, itemID, item2)

	require.NoError(t, f.store.ReopenMissed(ctx, missed))
	require.NoError(t, f.store.DeleteInsert(ctx, insertID))
	m, err = f.store.Missed(ctx, missed)
	require.NoError(t, err)
	require.Equal(t, outage.ResolutionOpen, m.Resolution)
	ins, err = f.store.Inserts(ctx, target)
	require.NoError(t, err)
	require.Empty(t, ins)
}
