package library

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// Tracks is the track store.
type Tracks struct {
	g *gorm.DB
}

// NewTracks returns the store on the database.
func NewTracks(d *db.DB) *Tracks { return &Tracks{g: d.Gorm} }

// WithTx returns the store bound to tx.
func (s *Tracks) WithTx(tx db.Tx) library.TrackStore { return &Tracks{g: tx.Gorm()} }

type trackRow struct {
	ID           uint64
	Title        string
	ArtistID     *uint64
	ArtistName   *string
	DurationMS   *int `gorm:"column:duration_ms"`
	FilePath     string
	Format       *string
	Bitrate      *int
	SampleRate   *int
	QualityOK    *bool `gorm:"column:quality_ok"`
	QualityNotes string
	TypeID       uint64
	TypeCode     string
	TypeName     string
	FileChecksum *string
	CreatedAt    time.Time
}

const selectTracks = `
SELECT tr.id, tr.title, tr.artist_id, a.name AS artist_name, tr.duration_ms, tr.file_path,
       tr.format, tr.bitrate, tr.sample_rate, tr.quality_ok, tr.quality_notes,
       t.id AS type_id, t.code AS type_code, t.name AS type_name, tr.file_checksum, tr.created_at
FROM tracks tr
JOIN playlist_types t ON t.id = tr.type_id
LEFT JOIN artists a ON a.id = tr.artist_id`

// List returns at most f.Limit tracks in (title, id) order after f.After.
//
// The library holds hundreds to tens of thousands of tracks; the keyset
// condition and the filters run on a sequential scan of tracks with a
// top-N sort, so no index on title is added (no speculative indexes).
// EXPLAIN ANALYZE, PostgreSQL 17, 30 000 tracks, 2 000 artists, limit 101:
// first page without filters 8.3 ms (Seq Scan tracks, hash joins, top-N
// heapsort 48 kB); with playlist_id, q, and a cursor 3.3 ms (Bitmap Index
// Scan on ix_track_playlists_playlist_id).
func (s *Tracks) List(ctx context.Context, f library.TrackFilter) ([]library.Track, error) {
	var (
		where []string
		args  []any
	)
	if f.TypeID != 0 {
		where = append(where, "tr.type_id = ?")
		args = append(args, f.TypeID)
	}
	if f.ArtistID != 0 {
		where = append(where, "tr.artist_id = ?")
		args = append(args, f.ArtistID)
	}
	if f.PlaylistID != 0 {
		where = append(where, "EXISTS (SELECT 1 FROM track_playlists tp WHERE tp.track_id = tr.id AND tp.playlist_id = ?)")
		args = append(args, f.PlaylistID)
	}
	if f.Query != "" {
		p := containsPattern(f.Query)
		where = append(where, "(tr.title ILIKE ? OR a.name ILIKE ?)")
		args = append(args, p, p)
	}
	if f.After != nil {
		where = append(where, "(tr.title, tr.id) > (?, ?)")
		args = append(args, f.After.Title, f.After.ID)
	}
	sql := selectTracks
	if len(where) > 0 {
		// The conditions are fixed strings above; values go in args.
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY tr.title, tr.id LIMIT ?"
	args = append(args, f.Limit)

	var rows []trackRow
	if err := s.g.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("select tracks: %w", err)
	}
	return s.withLinks(ctx, rows)
}

// Get returns one track, or library.ErrTrackNotFound.
func (s *Tracks) Get(ctx context.Context, id uint64) (library.Track, error) {
	var rows []trackRow
	if err := s.g.WithContext(ctx).Raw(selectTracks+` WHERE tr.id = ?`, id).Scan(&rows).Error; err != nil {
		return library.Track{}, fmt.Errorf("select track %d: %w", id, err)
	}
	if len(rows) == 0 {
		return library.Track{}, library.ErrTrackNotFound
	}
	ts, err := s.withLinks(ctx, rows)
	if err != nil {
		return library.Track{}, err
	}
	return ts[0], nil
}

// withLinks adds the playlist and play rule IDs of the tracks, one query
// each (the primary keys of the link tables lead with track_id).
func (s *Tracks) withLinks(ctx context.Context, rows []trackRow) ([]library.Track, error) {
	if len(rows) == 0 {
		return []library.Track{}, nil
	}
	ids := make([]uint64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	g := s.g.WithContext(ctx)
	var playlists, rules []linkRow
	err := g.Raw(`
SELECT track_id AS owner_id, playlist_id AS other_id FROM track_playlists
WHERE track_id IN ? ORDER BY playlist_id`, ids).Scan(&playlists).Error
	if err != nil {
		return nil, fmt.Errorf("select track playlists: %w", err)
	}
	err = g.Raw(`
SELECT track_id AS owner_id, rule_id AS other_id FROM track_play_rules
WHERE track_id IN ? ORDER BY rule_id`, ids).Scan(&rules).Error
	if err != nil {
		return nil, fmt.Errorf("select track play rules: %w", err)
	}
	byPlaylist, byRule := group(playlists), group(rules)

	out := make([]library.Track, len(rows))
	for i, r := range rows {
		t := library.Track{
			ID: r.ID, Title: r.Title, DurationMS: r.DurationMS, FilePath: r.FilePath,
			Format: r.Format, Bitrate: r.Bitrate, SampleRate: r.SampleRate,
			QualityOK: r.QualityOK, QualityNotes: r.QualityNotes,
			Type:         library.TypeRef{ID: r.TypeID, Code: r.TypeCode, Name: r.TypeName},
			FileChecksum: r.FileChecksum,
			CreatedAt:    r.CreatedAt.UTC(),
			PlaylistIDs:  byPlaylist[r.ID],
			PlayRuleIDs:  byRule[r.ID],
		}
		if r.ArtistID != nil && r.ArtistName != nil {
			t.Artist = &library.ArtistRef{ID: *r.ArtistID, Name: *r.ArtistName}
		}
		out[i] = t
	}
	return out, nil
}

// trackColumns are the editable columns of a track. A map, so nil and zero
// values are written on update (Updates skips them in a struct).
func trackColumns(in library.TrackInput) map[string]any {
	return map[string]any{
		"title":         in.Title,
		"artist_id":     in.ArtistID,
		"file_path":     in.FilePath,
		"type_id":       in.TypeID,
		"duration_ms":   in.DurationMS,
		"format":        in.Format,
		"bitrate":       in.Bitrate,
		"sample_rate":   in.SampleRate,
		"quality_ok":    in.QualityOK,
		"quality_notes": in.QualityNotes,
	}
}

type trackWrite struct {
	ID           uint64 `gorm:"primaryKey"`
	Title        string
	ArtistID     *uint64
	FilePath     string
	TypeID       uint64
	DurationMS   *int `gorm:"column:duration_ms"`
	Format       *string
	Bitrate      *int
	SampleRate   *int
	QualityOK    *bool `gorm:"column:quality_ok"`
	QualityNotes string
}

func (trackWrite) TableName() string { return "tracks" }

// Create adds a track and returns its ID.
func (s *Tracks) Create(ctx context.Context, in library.TrackInput) (uint64, error) {
	r := trackWrite{
		Title: in.Title, ArtistID: in.ArtistID, FilePath: in.FilePath, TypeID: in.TypeID,
		DurationMS: in.DurationMS, Format: in.Format, Bitrate: in.Bitrate, SampleRate: in.SampleRate,
		QualityOK: in.QualityOK, QualityNotes: in.QualityNotes,
	}
	if err := s.g.WithContext(ctx).Create(&r).Error; err != nil {
		return 0, writeError("insert track", err)
	}
	return r.ID, nil
}

// Update changes the metadata of a track.
func (s *Tracks) Update(ctx context.Context, id uint64, in library.TrackInput) error {
	res := s.g.WithContext(ctx).Table("tracks").Where("id = ?", id).Updates(trackColumns(in))
	if res.Error != nil {
		return writeError(fmt.Sprintf("update track %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrTrackNotFound
	}
	return nil
}

// SetPlaylists makes playlistIDs the playlists of the track. Run it in a
// transaction.
func (s *Tracks) SetPlaylists(ctx context.Context, id uint64, playlistIDs []uint64) error {
	g := s.g.WithContext(ctx)
	del := g.Where("track_id = ?", id)
	if len(playlistIDs) > 0 {
		del = del.Where("playlist_id NOT IN ?", playlistIDs)
	}
	if err := del.Delete(&trackPlaylistRow{}).Error; err != nil {
		return fmt.Errorf("delete playlists of track %d: %w", id, err)
	}
	if len(playlistIDs) == 0 {
		return nil
	}
	rows := make([]trackPlaylistRow, len(playlistIDs))
	for i, p := range playlistIDs {
		rows[i] = trackPlaylistRow{TrackID: id, PlaylistID: p}
	}
	err := g.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, linkBatch).Error
	if err != nil {
		return writeError(fmt.Sprintf("insert playlists of track %d", id), err)
	}
	return nil
}

// Delete removes a track; its memberships and play rule links go with it
// (CASCADE). The play log, the track plan, and clock filler pools refuse it
// (RESTRICT).
func (s *Tracks) Delete(ctx context.Context, id uint64) error {
	res := s.g.WithContext(ctx).Exec(`DELETE FROM tracks WHERE id = ?`, id)
	if res.Error != nil {
		return deleteError(fmt.Sprintf("delete track %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrTrackNotFound
	}
	return nil
}

// Users returns the clocks whose filler pool holds the track (uses
// ix_clock_filler_pool_track_id).
func (s *Tracks) Users(ctx context.Context, id uint64, limit int) ([]library.Ref, error) {
	var rows []refRow
	err := s.g.WithContext(ctx).Raw(`
SELECT 'clock' AS kind, c.id, c.name
FROM clocks c
WHERE EXISTS (SELECT 1 FROM clock_filler_pool p WHERE p.clock_id = c.id AND p.track_id = ?)
ORDER BY c.name, c.id
LIMIT ?`, id, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("select users of track %d: %w", id, err)
	}
	return refs(rows), nil
}
