package library

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// Artists is the artist store.
type Artists struct {
	g *gorm.DB
}

// NewArtists returns the store on the database.
func NewArtists(d *db.DB) *Artists { return &Artists{g: d.Gorm} }

type artistRow struct {
	ID         uint64
	Name       string
	TrackCount int
}

func (r artistRow) domain() library.Artist {
	return library.Artist{ID: r.ID, Name: r.Name, TrackCount: r.TrackCount}
}

// The track count uses ix_tracks_artist_id.
const selectArtists = `
SELECT a.id, a.name, (SELECT count(*) FROM tracks t WHERE t.artist_id = a.id) AS track_count
FROM artists a`

// List returns the artists whose name contains query, in name order. The
// library has hundreds of artists, so the list is not paged.
func (s *Artists) List(ctx context.Context, query string) ([]library.Artist, error) {
	var rows []artistRow
	q := s.g.WithContext(ctx)
	var err error
	if query != "" {
		err = q.Raw(selectArtists+` WHERE a.name ILIKE ? ORDER BY a.name, a.id`, containsPattern(query)).Scan(&rows).Error
	} else {
		err = q.Raw(selectArtists + ` ORDER BY a.name, a.id`).Scan(&rows).Error
	}
	if err != nil {
		return nil, fmt.Errorf("select artists: %w", err)
	}
	out := make([]library.Artist, len(rows))
	for i, r := range rows {
		out[i] = r.domain()
	}
	return out, nil
}

// Get returns one artist, or library.ErrArtistNotFound.
func (s *Artists) Get(ctx context.Context, id uint64) (library.Artist, error) {
	var rows []artistRow
	if err := s.g.WithContext(ctx).Raw(selectArtists+` WHERE a.id = ?`, id).Scan(&rows).Error; err != nil {
		return library.Artist{}, fmt.Errorf("select artist %d: %w", id, err)
	}
	if len(rows) == 0 {
		return library.Artist{}, library.ErrArtistNotFound
	}
	return rows[0].domain(), nil
}

type artistWrite struct {
	ID   uint64 `gorm:"primaryKey"`
	Name string
}

func (artistWrite) TableName() string { return "artists" }

// Create adds an artist.
func (s *Artists) Create(ctx context.Context, name string) (uint64, error) {
	r := artistWrite{Name: name}
	if err := s.g.WithContext(ctx).Create(&r).Error; err != nil {
		return 0, writeError("insert artist", err)
	}
	return r.ID, nil
}

// Rename changes the name of an artist.
func (s *Artists) Rename(ctx context.Context, id uint64, name string) error {
	res := s.g.WithContext(ctx).Table("artists").Where("id = ?", id).Update("name", name)
	if res.Error != nil {
		return writeError(fmt.Sprintf("rename artist %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrArtistNotFound
	}
	return nil
}

// Delete removes an artist. Tracks and the play log refuse it (RESTRICT).
func (s *Artists) Delete(ctx context.Context, id uint64) error {
	res := s.g.WithContext(ctx).Exec(`DELETE FROM artists WHERE id = ?`, id)
	if res.Error != nil {
		return deleteError(fmt.Sprintf("delete artist %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrArtistNotFound
	}
	return nil
}

// Users returns the tracks of the artist, at most limit
// (ix_tracks_artist_id).
func (s *Artists) Users(ctx context.Context, id uint64, limit int) ([]library.Ref, error) {
	var rows []refRow
	err := s.g.WithContext(ctx).Raw(`
SELECT 'track' AS kind, id, title AS name FROM tracks WHERE artist_id = ? ORDER BY title, id LIMIT ?`, id, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("select users of artist %d: %w", id, err)
	}
	return refs(rows), nil
}
