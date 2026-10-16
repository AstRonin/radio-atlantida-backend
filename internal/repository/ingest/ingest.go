// Package ingest is the pgx store of internal/ingest: tracks created by an
// import, the track paths of a folder, the analysis columns of tracks, and
// the checks around them (§5.1).
package ingest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/ingest"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// Store implements ingest.Store.
type Store struct {
	q db.Querier
}

// New returns the store.
func New(q db.Querier) *Store { return &Store{q: q} }

// WithTx returns the store bound to tx.
func (s *Store) WithTx(tx db.Tx) ingest.Store { return &Store{q: tx.Pgx()} }

// TypeExists reports whether the playlist type exists.
func (s *Store) TypeExists(ctx context.Context, id uint64) (bool, error) {
	var ok bool
	if err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM playlist_types WHERE id = $1)`, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("select playlist type %d: %w", id, err)
	}
	return ok, nil
}

// MissingPlaylists returns the IDs of ids that are not playlists, in order.
func (s *Store) MissingPlaylists(ctx context.Context, ids []uint64) ([]uint64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.q.Query(ctx, `
SELECT u.id FROM unnest($1::bigint[]) AS u (id)
WHERE NOT EXISTS (SELECT 1 FROM playlists p WHERE p.id = u.id)
ORDER BY u.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("select missing playlists: %w", err)
	}
	missing, err := pgx.CollectRows(rows, pgx.RowTo[uint64])
	if err != nil {
		return nil, fmt.Errorf("scan missing playlists: %w", err)
	}
	return missing, nil
}

// ArtistExists reports whether the artist exists.
func (s *Store) ArtistExists(ctx context.Context, id uint64) (bool, error) {
	var ok bool
	if err := s.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM artists WHERE id = $1)`, id).Scan(&ok); err != nil {
		return false, fmt.Errorf("select artist %d: %w", id, err)
	}
	return ok, nil
}

// TrackByChecksum returns a track whose file has the checksum
// (ix_tracks_file_checksum).
func (s *Store) TrackByChecksum(ctx context.Context, sum string) (ingest.TrackFile, bool, error) {
	var t ingest.TrackFile
	err := s.q.QueryRow(ctx, `SELECT id, file_path FROM tracks WHERE file_checksum = $1 ORDER BY id LIMIT 1`, sum).
		Scan(&t.ID, &t.FilePath)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ingest.TrackFile{}, false, nil
	case err != nil:
		return ingest.TrackFile{}, false, fmt.Errorf("select track by checksum: %w", err)
	}
	return t, true, nil
}

// TrackByPath returns the track of the file path (uq_tracks_file_path).
func (s *Store) TrackByPath(ctx context.Context, path string) (uint64, bool, error) {
	var id uint64
	err := s.q.QueryRow(ctx, `SELECT id FROM tracks WHERE file_path = $1`, path).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("select track by path: %w", err)
	}
	return id, true, nil
}

// trackPathsUnderSQL selects the tracks under a folder: the paths from
// "<folder>/" up to "<folder>0" ('0' is the byte after '/'), compared
// bytewise by the pattern operators, so ix_tracks_file_path serves it with
// the bounds as parameters (a generic plan included).
const trackPathsUnderSQL = `SELECT file_path, id FROM tracks WHERE file_path ~>=~ $1 AND file_path ~<~ $2`

type pathRow struct {
	FilePath string `db:"file_path"`
	ID       uint64 `db:"id"`
}

// TrackPaths maps the file path of every track under folder ("" for every
// track) to the track. The import compares a folder's files with it by
// path, the originals of .optimal copies included, without reading a file.
//
// EXPLAIN ANALYZE (PostgreSQL 17.11, TestTrackPathsUsesTheIndex: 3,000
// tracks in 30 folders, folder f07):
//
//	Bitmap Heap Scan on tracks (actual rows=100 loops=1)
//	  Recheck Cond: ((file_path ~>=~ 'f07/'::text) AND (file_path ~<~ 'f070'::text))
//	  Heap Blocks: exact=39
//	  ->  Bitmap Index Scan on ix_tracks_file_path (actual rows=100 loops=1)
//	        Index Cond: ((file_path ~>=~ 'f07/'::text) AND (file_path ~<~ 'f070'::text))
//	Planning Time: 0.145 ms
//	Execution Time: 0.070 ms
//
// The root ("") reads every track: the folder counts need all of them.
func (s *Store) TrackPaths(ctx context.Context, folder string) (map[string]uint64, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if folder == "" {
		rows, err = s.q.Query(ctx, `SELECT file_path, id FROM tracks`)
	} else {
		rows, err = s.q.Query(ctx, trackPathsUnderSQL, folder+"/", folder+"0")
	}
	if err != nil {
		return nil, fmt.Errorf("select track paths: %w", err)
	}
	ps, err := pgx.CollectRows(rows, pgx.RowToStructByName[pathRow])
	if err != nil {
		return nil, fmt.Errorf("scan track paths: %w", err)
	}
	out := make(map[string]uint64, len(ps))
	for _, r := range ps {
		out[r.FilePath] = r.ID
	}
	return out, nil
}

// EnsureArtist returns the artist with the name, ignoring case, and creates
// it when there is none. The case-insensitive lookup scans artists: a
// library has a few thousand, and an import looks up one per file.
func (s *Store) EnsureArtist(ctx context.Context, name string) (uint64, error) {
	var id uint64
	err := s.q.QueryRow(ctx, `SELECT id FROM artists WHERE lower(name) = lower($1) ORDER BY id LIMIT 1`, name).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, fmt.Errorf("select artist: %w", err)
	}
	// Another import worker may create the same name at the same moment:
	// then the insert does nothing and the row is read.
	err = s.q.QueryRow(ctx, `INSERT INTO artists (name) VALUES ($1) ON CONFLICT (name) DO NOTHING RETURNING id`, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.q.QueryRow(ctx, `SELECT id FROM artists WHERE name = $1`, name).Scan(&id)
	}
	if err != nil {
		return 0, fmt.Errorf("insert artist: %w", err)
	}
	return id, nil
}

// CreateTrack adds a track with its playlists and analysis.
func (s *Store) CreateTrack(ctx context.Context, t ingest.NewTrack) (uint64, error) {
	a, m := t.Analysis, t.Analysis.Measurements
	var id uint64
	err := s.q.QueryRow(ctx, `
INSERT INTO tracks (title, artist_id, type_id, file_path, file_checksum, duration_ms, format, codec, bitrate,
                    sample_rate, channels, loudness_lufs, true_peak_dbtp, loudness_range_lu, source_bitrate,
                    quality_issues, quality_ok, analyzed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, cardinality($16::text[]) = 0, $17)
RETURNING id`,
		t.Title, t.ArtistID, t.TypeID, a.FilePath, a.Checksum, m.DurationMS, m.Format, m.Codec, m.Bitrate,
		m.SampleRate, m.Channels, m.LoudnessLUFS, m.TruePeakDBTP, m.LoudnessRangeLU, m.SourceBitrate,
		codes(a.Issues), a.AnalyzedAt).Scan(&id)
	if err != nil {
		return 0, trackError(err, "insert track")
	}
	if len(t.PlaylistIDs) > 0 {
		_, err = s.q.Exec(ctx, `
INSERT INTO track_playlists (track_id, playlist_id) SELECT $1, unnest($2::bigint[])`, id, t.PlaylistIDs)
		if err != nil {
			return 0, trackError(err, "insert track playlists")
		}
	}
	return id, nil
}

const trackColumns = `id, file_path, file_checksum, duration_ms, format, codec, bitrate, sample_rate, channels,
       loudness_lufs, true_peak_dbtp, loudness_range_lu, source_bitrate, quality_issues, analyzed_at`

type trackRow struct {
	ID              uint64     `db:"id"`
	FilePath        string     `db:"file_path"`
	FileChecksum    *string    `db:"file_checksum"`
	DurationMS      *int       `db:"duration_ms"`
	Format          *string    `db:"format"`
	Codec           *string    `db:"codec"`
	Bitrate         *int       `db:"bitrate"`
	SampleRate      *int       `db:"sample_rate"`
	Channels        *int       `db:"channels"`
	LoudnessLUFS    *float64   `db:"loudness_lufs"`
	TruePeakDBTP    *float64   `db:"true_peak_dbtp"`
	LoudnessRangeLU *float64   `db:"loudness_range_lu"`
	SourceBitrate   *int       `db:"source_bitrate"`
	QualityIssues   []string   `db:"quality_issues"`
	AnalyzedAt      *time.Time `db:"analyzed_at"`
}

func (r trackRow) domain() ingest.Track {
	t := ingest.Track{
		ID: r.ID, FilePath: r.FilePath, Checksum: r.FileChecksum,
		Measurements: ingest.Measurements{
			DurationMS: r.DurationMS, Format: r.Format, Codec: r.Codec, Bitrate: r.Bitrate,
			SampleRate: r.SampleRate, Channels: r.Channels, LoudnessLUFS: r.LoudnessLUFS,
			TruePeakDBTP: r.TruePeakDBTP, LoudnessRangeLU: r.LoudnessRangeLU, SourceBitrate: r.SourceBitrate,
		},
		Issues: make([]library.QualityIssue, len(r.QualityIssues)),
	}
	for i, c := range r.QualityIssues {
		t.Issues[i] = library.QualityIssue(c)
	}
	if r.AnalyzedAt != nil {
		t.AnalyzedAt = new(r.AnalyzedAt.UTC())
	}
	return t
}

// Tracks returns the tracks of ids that exist, in ID order.
func (s *Store) Tracks(ctx context.Context, ids []uint64) ([]ingest.Track, error) {
	rows, err := s.q.Query(ctx, `SELECT `+trackColumns+` FROM tracks WHERE id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return nil, fmt.Errorf("select tracks: %w", err)
	}
	ts, err := db.CollectAll(rows, trackRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan tracks: %w", err)
	}
	return ts, nil
}

// Unanalysed returns at most limit tracks never analysed, in ID order.
func (s *Store) Unanalysed(ctx context.Context, limit int) ([]ingest.Track, error) {
	rows, err := s.q.Query(ctx, `SELECT `+trackColumns+` FROM tracks WHERE analyzed_at IS NULL ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("select unanalysed tracks: %w", err)
	}
	ts, err := db.CollectAll(rows, trackRow.domain)
	if err != nil {
		return nil, fmt.Errorf("scan unanalysed tracks: %w", err)
	}
	return ts, nil
}

// SaveAnalysis stores the analysis of a track and its file path.
// quality_ok follows the issues: the manager's override lasts until the
// next analysis. A new checksum (a transcode, a file replaced on disk)
// clears the reviewed mark in the same statement; the SET expressions see
// the stored checksum.
func (s *Store) SaveAnalysis(ctx context.Context, id uint64, a ingest.Analysis) error {
	m := a.Measurements
	tag, err := s.q.Exec(ctx, `
UPDATE tracks SET file_path = $2, file_checksum = $3, duration_ms = $4, format = $5, codec = $6, bitrate = $7,
                  sample_rate = $8, channels = $9, loudness_lufs = $10, true_peak_dbtp = $11,
                  loudness_range_lu = $12, source_bitrate = $13, quality_issues = $14,
                  quality_ok = cardinality($14::text[]) = 0, analyzed_at = $15,
                  reviewed_at = CASE WHEN file_checksum IS DISTINCT FROM $3 THEN NULL ELSE reviewed_at END
WHERE id = $1`,
		id, a.FilePath, a.Checksum, m.DurationMS, m.Format, m.Codec, m.Bitrate, m.SampleRate, m.Channels,
		m.LoudnessLUFS, m.TruePeakDBTP, m.LoudnessRangeLU, m.SourceBitrate, codes(a.Issues), a.AnalyzedAt)
	if err != nil {
		return trackError(err, fmt.Sprintf("update track %d", id))
	}
	if tag.RowsAffected() == 0 {
		return library.ErrTrackNotFound
	}
	return nil
}

// MarkMissing records that the file of a track is missing.
func (s *Store) MarkMissing(ctx context.Context, id uint64, at time.Time) error {
	tag, err := s.q.Exec(ctx, `
UPDATE tracks SET quality_issues = ARRAY['file_missing'], quality_ok = false, analyzed_at = $2 WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("update track %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return library.ErrTrackNotFound
	}
	return nil
}

// Busy returns the tracks of ids that are queued in Liquidsoap, planned to
// start before until (late planned items included: the pusher may still
// take them), or on air at now. A play_log row without an end counts only
// within 6 hours of its start (the longest track), so a lost end callback
// does not keep a track busy forever.
//
// Both parts use an index that leads with track_id
// (ix_scheduled_items_track_id_planned_start_utc, ix_play_log_track_id_started_at).
func (s *Store) Busy(ctx context.Context, ids []uint64, now, until time.Time) ([]uint64, error) {
	rows, err := s.q.Query(ctx, `
SELECT si.track_id FROM scheduled_items si
WHERE si.track_id = ANY($1) AND (si.status = 'queued' OR (si.status = 'planned' AND si.planned_start_utc < $3))
UNION
SELECT pl.track_id FROM play_log pl
WHERE pl.track_id = ANY($1) AND pl.started_at IS NOT NULL AND pl.ended_at IS NULL
  AND pl.started_at > $2::timestamptz - interval '6 hours' AND pl.started_at <= $2
ORDER BY 1`, ids, now, until)
	if err != nil {
		return nil, fmt.Errorf("select busy tracks: %w", err)
	}
	busy, err := pgx.CollectRows(rows, pgx.RowTo[uint64])
	if err != nil {
		return nil, fmt.Errorf("scan busy tracks: %w", err)
	}
	return busy, nil
}

// codes returns the issue codes as text.
func codes(issues []library.QualityIssue) []string {
	out := make([]string, len(issues))
	for i, c := range issues {
		out[i] = string(c)
	}
	return out
}

// trackError maps the constraint violations of a track write to the
// library's errors.
func trackError(err error, what string) error {
	if ce, ok := db.AsConstraintError(err); ok {
		switch ce.Constraint {
		case "uq_tracks_file_path":
			return library.ErrFilePathTaken
		case "fk_tracks_type_id":
			return library.ErrUnknownType
		case "fk_track_playlists_playlist_id":
			return library.ErrUnknownPlaylist
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
