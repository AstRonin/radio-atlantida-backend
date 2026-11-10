//go:build integration

package rotation_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
			HourStart: target, TrackID: locked.TrackID, MissedID: &locked.ID, Placement: rotation.PlacementFixed,
			OffsetMS: new(30*60000 + 40000), ToleranceMS: 60000,
		})
		return createErr
	})
	ins, err := f.store.Inserts(ctx, target)
	require.NoError(t, err)
	want := rotation.Insert{
		ID: insertID, HourStart: target, TrackID: f.tracks[2], MissedID: &missed, Placement: rotation.PlacementFixed,
		OffsetMS: new(30*60000 + 40000), ToleranceMS: 60000,
	}
	require.Equal(t, []rotation.Insert{want}, ins)
	one, err := f.store.Insert(ctx, insertID)
	require.NoError(t, err)
	require.Equal(t, want, one)
	_, err = f.store.Insert(ctx, 999999)
	require.ErrorIs(t, err, rotation.ErrInsertNotFound)

	// The build writes the reschedule item; the link resolves the missed row.
	f.inTx(func(ctx context.Context, st rotation.Store) error {
		return st.ReplaceHour(ctx, f.planHour(target), []rotation.Item{{
			TrackID: f.tracks[2], OffsetMS: 30 * 60000, OutMS: 30000, Source: rotation.SourceReschedule, RescheduleOfID: &missed,
			HourInsertID: &insertID,
		}})
	})
	require.NoError(t, f.store.LinkRescheduled(ctx, missed, hour))
	plan, err := f.store.Plan(ctx, target, target.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, []rotation.Insert{want}, plan[0].Inserts, "the plan lists the hour's one-time inserts")
	require.Equal(t, insertID, *plan[0].Items[0].HourInsertID)
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
		return st.ReplaceHour(ctx, f.planHour(target), []rotation.Item{{
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

// The slot claim of a pin goes through hour_inserts, a move keeps it, and a
// deleted playlist clears the playlist but leaves the origin.
func TestInsertClaimRoundTrip(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	jingles := f.id(`INSERT INTO playlists (name, type_id) SELECT 'J', id FROM playlist_types WHERE code = 'jingle' RETURNING id`)
	target := hour.Add(2 * time.Hour)
	var id uint64
	f.inTx(func(ctx context.Context, st rotation.Store) (err error) {
		id, err = st.CreateInsert(ctx, rotation.Insert{
			HourStart: target, TrackID: f.tracks[2], Placement: rotation.PlacementFixed, OffsetMS: new(1200000), ToleranceMS: 60000,
			RulePlaylistID: &jingles, RuleOriginMS: new(754321),
		})
		return err
	})
	want := rotation.Insert{
		ID: id, HourStart: target, TrackID: f.tracks[2], Placement: rotation.PlacementFixed, OffsetMS: new(1200000), ToleranceMS: 60000,
		RulePlaylistID: &jingles, RuleOriginMS: new(754321),
	}
	got, err := f.store.Insert(ctx, id)
	require.NoError(t, err)
	require.Equal(t, want, got)
	list, err := f.store.Inserts(ctx, target)
	require.NoError(t, err)
	require.Equal(t, []rotation.Insert{want}, list)
	ranged, err := f.store.InsertsRange(ctx, target, target.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, []rotation.Insert{want}, ranged)

	moved := want
	moved.OffsetMS = new(1260000)
	require.NoError(t, f.store.UpdateInsert(ctx, moved))
	got, err = f.store.Insert(ctx, id)
	require.NoError(t, err)
	require.Equal(t, moved, got, "a move keeps the claim and its origin")

	_, err = f.d.Pool.Exec(ctx, `INSERT INTO hour_inserts (hour_start_utc, track_id, placement, rule_playlist_id)
VALUES ($1, $2, 'start', $3)`, target, f.tracks[2], jingles)
	require.ErrorContains(t, err, "ck_hour_inserts_rule_fixed")
	_, err = f.d.Pool.Exec(ctx, `UPDATE hour_inserts SET rule_origin_ms = 3600000 WHERE id = $1`, id)
	require.ErrorContains(t, err, "ck_hour_inserts_rule_origin_ms")

	f.exec(`DELETE FROM playlists WHERE id = $1`, jingles)
	got, err = f.store.Insert(ctx, id)
	require.NoError(t, err)
	require.Nil(t, got.RulePlaylistID, "the deleted playlist is cleared")
	require.Equal(t, new(754321), got.RuleOriginMS)
}
