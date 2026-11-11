//go:build integration

package ingest

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
)

// TestTrackPathsUsesTheIndex keeps the plan in the doc comment of
// TrackPaths: the folder lookup is an index range scan.
func TestTrackPathsUsesTheIndex(t *testing.T) {
	t.Parallel()
	d := dbtest.Migrated(t)
	ctx := t.Context()
	_, err := d.Pool.Exec(ctx, `INSERT INTO tracks (title, file_path, type_id)
SELECT 'T' || g, format('f%s/track-%s.mp3', lpad((g % 30)::text, 2, '0'), g), pt.id
FROM generate_series(1, 3000) AS g, playlist_types pt WHERE pt.code = 'music'`)
	require.NoError(t, err)
	_, err = d.Pool.Exec(ctx, `ANALYZE tracks`)
	require.NoError(t, err)

	rows, err := d.Pool.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY ON) `+trackPathsUnderSQL, "f07/", "f070")
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
	require.Contains(t, strings.Join(plan, "\n"), "Index Scan on ix_tracks_file_path", "a bitmap scan of the index for one folder of thirty")

	got, err := New(d.Pool).TrackPaths(ctx, "f07")
	require.NoError(t, err)
	require.Len(t, got, 100)
}

// TestTrackSummariesUsesTheIndex keeps the plan in the doc comment of
// TrackSummaries: the folder lookup is an index range scan.
func TestTrackSummariesUsesTheIndex(t *testing.T) {
	t.Parallel()
	d := dbtest.Migrated(t)
	ctx := t.Context()
	_, err := d.Pool.Exec(ctx, `INSERT INTO artists (name) SELECT 'A' || g FROM generate_series(1, 300) AS g`)
	require.NoError(t, err)
	_, err = d.Pool.Exec(ctx, `INSERT INTO tracks (title, file_path, type_id, artist_id)
SELECT 'T' || g, format('f%s/track-%s.mp3', lpad((g % 30)::text, 2, '0'), g), pt.id,
       (SELECT min(id) FROM artists) + g % 300
FROM generate_series(1, 3000) AS g, playlist_types pt WHERE pt.code = 'music'`)
	require.NoError(t, err)
	_, err = d.Pool.Exec(ctx, `ANALYZE tracks, artists`)
	require.NoError(t, err)

	rows, err := d.Pool.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY ON) `+trackSummariesUnderSQL, "f07/", "f070")
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
	require.Contains(t, strings.Join(plan, "\n"), "Index Scan on ix_tracks_file_path", "an index scan of the path range")

	got, err := New(d.Pool).TrackSummaries(ctx, "f07")
	require.NoError(t, err)
	require.Len(t, got, 100)
	for _, s := range got {
		require.NotEmpty(t, s.Artist, s.FilePath)
	}
}
