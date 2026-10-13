//go:build integration

package retention

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
)

var now = time.Date(2026, 10, 13, 3, 0, 0, 0, time.UTC)

func TestDeletePlayLog(t *testing.T) {
	d := dbtest.Migrated(t)
	ctx := t.Context()
	pool := d.Pool

	var trackID, outageID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO tracks (title, file_path, type_id)
		VALUES ('Song', 'a.mp3', (SELECT id FROM playlist_types WHERE code = 'music'))
		RETURNING id`).Scan(&trackID))
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component)
		VALUES ($1, $1, 'icecast', 'icecast') RETURNING id`, now.Add(-30*24*time.Hour)).Scan(&outageID))

	cutoff := now.Add(-10 * 24 * time.Hour)
	old := cutoff.Add(-time.Hour)
	recent := cutoff.Add(time.Hour)
	insertAired := func(status string, startedAt time.Time) int64 {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO play_log (track_id, status, planned_start_utc, started_at, actual_duration_ms, truncated)
			VALUES ($1, $2, $3, $3, 1000, $2 = 'truncated') RETURNING id`,
			trackID, status, startedAt).Scan(&id))
		return id
	}
	insertMissed := func(resolution string, resolvedAt *time.Time) int64 {
		var id int64
		var resolvedItem *int64
		if resolution == "rescheduled" {
			one := int64(1)
			resolvedItem = &one
		}
		require.NoError(t, pool.QueryRow(ctx, `
			INSERT INTO play_log (track_id, status, planned_start_utc, outage_id, resolution, resolved_at, resolved_item_id)
			VALUES ($1, 'missed', $2, $3, $4, $5, $6) RETURNING id`,
			trackID, old.Add(-24*time.Hour), outageID, resolution, resolvedAt, resolvedItem).Scan(&id))
		return id
	}

	keep := []int64{
		insertAired("aired", recent),
		insertMissed("open", nil), // open missed items are never deleted
		insertMissed("dismissed", &recent),
	}
	insertAired("aired", old)
	insertAired("truncated", old)
	insertMissed("dismissed", &old)
	insertMissed("rescheduled", &old)

	// Batches of 3 over 4 expired rows: a full batch, then a short one.
	store := New(pool)
	store.batchSize = 3
	deleted, err := store.DeletePlayLog(ctx, cutoff)
	require.NoError(t, err)
	require.EqualValues(t, 4, deleted)

	rows, err := pool.Query(ctx, `SELECT id FROM play_log`)
	require.NoError(t, err)
	left, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	require.NoError(t, err)
	require.ElementsMatch(t, keep, left)
}

func TestDeleteOperations(t *testing.T) {
	d := dbtest.Migrated(t)
	ctx := t.Context()

	cutoff := now.Add(-10 * 24 * time.Hour)
	insert := func(status string, finishedAt *time.Time) int64 {
		var id int64
		var msg *string
		if status == "failed" {
			m := "boom"
			msg = &m
		}
		require.NoError(t, d.Pool.QueryRow(ctx, `
			INSERT INTO operations (kind, status, started_at, finished_at, error_message)
			VALUES ('retention', $1, $2, $3, $4) RETURNING id`,
			status, cutoff.Add(-48*time.Hour), finishedAt, msg).Scan(&id))
		return id
	}
	old := cutoff.Add(-time.Hour)
	recent := cutoff.Add(time.Hour)
	keep := []int64{
		insert("running", nil), // running operations are never deleted
		insert("completed", &recent),
	}
	for range 3 {
		insert("completed", &old)
	}
	insert("failed", &old)

	store := New(d.Pool)
	store.batchSize = 2
	deleted, err := store.DeleteOperations(ctx, cutoff)
	require.NoError(t, err)
	require.EqualValues(t, 4, deleted)

	rows, err := d.Pool.Query(ctx, `SELECT id FROM operations`)
	require.NoError(t, err)
	left, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	require.NoError(t, err)
	require.ElementsMatch(t, keep, left)
}
