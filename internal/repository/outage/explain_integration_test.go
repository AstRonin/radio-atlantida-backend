//go:build integration

package outage

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
)

// TestExplainHistory keeps the plan of the played-tracks page in the doc
// comment of History honest: 10 days at 15 plays per hour, the newest page
// of the last day.
func TestExplainHistory(t *testing.T) {
	d := dbtest.Migrated(t)
	ctx := t.Context()
	end := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	_, err := d.Pool.Exec(ctx, `
WITH a AS (INSERT INTO artists (name) VALUES ('A') RETURNING id),
     t AS (INSERT INTO tracks (title, artist_id, duration_ms, file_path, type_id)
           SELECT 'T', a.id, 180000, 't.mp3', pt.id FROM a, playlist_types pt WHERE pt.code = 'music' RETURNING id, artist_id)
INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, started_at, planned_out_ms, actual_duration_ms)
SELECT t.id, t.artist_id, 'aired', g, g, 180000, 180000
FROM t, generate_series($1::timestamptz - interval '10 days', $1::timestamptz, interval '4 minutes') g`, end)
	require.NoError(t, err)
	_, err = d.Pool.Exec(ctx, `ANALYZE play_log`)
	require.NoError(t, err)

	rows, err := d.Pool.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY ON) `+entrySelect+`
WHERE COALESCE(pl.started_at, pl.planned_start_utc) >= $1
  AND COALESCE(pl.started_at, pl.planned_start_utc) < $2
  AND ($3::text IS NULL OR pl.status = $3)
  AND ($4::timestamptz IS NULL OR (COALESCE(pl.started_at, pl.planned_start_utc), pl.id) < ($4, $5::bigint))
ORDER BY COALESCE(pl.started_at, pl.planned_start_utc) DESC, pl.id DESC
LIMIT $6`, end.Add(-24*time.Hour), end, nil, nil, nil, 101)
	require.NoError(t, err)
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		t.Log(line)
		plan = append(plan, line)
	}
	require.NoError(t, rows.Err())
	require.Contains(t, strings.Join(plan, "\n"), "ix_play_log_sort_time")
}
