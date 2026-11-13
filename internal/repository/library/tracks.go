package library

import (
	"context"
	"fmt"

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

// trackEditColumns are the columns PUT changes of a track. A map, so nil and
// zero values are written on update (Updates skips them in a struct). No
// analysis column is here (file path, duration, format, bitrate, sample
// rate, checksum, measurements): only SaveAnalysis writes those.
func trackEditColumns(in library.TrackEdit) map[string]any {
	return map[string]any{
		"title":         in.Title,
		"artist_id":     in.ArtistID,
		"type_id":       in.TypeID,
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

// Update changes the metadata of a track. The reviewed mark is cleared in the
// same statement when the title or the artist changes, the SET expression
// sees the stored row, so there is no read before the write to race with.
func (s *Tracks) Update(ctx context.Context, id uint64, in library.TrackEdit) error {
	cols := trackEditColumns(in)
	cols["reviewed_at"] = gorm.Expr(`CASE WHEN title IS DISTINCT FROM ? OR artist_id IS DISTINCT FROM ? THEN NULL ELSE reviewed_at END`,
		in.Title, in.ArtistID)
	res := s.g.WithContext(ctx).Table("tracks").Where("id = ?", id).Updates(cols)
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
// (CASCADE). The play log, the track plan, clock inserts, and one-time
// inserts refuse it (RESTRICT).
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
