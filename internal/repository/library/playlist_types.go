package library

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// Types is the playlist type store.
type Types struct {
	g *gorm.DB
}

// NewTypes returns the store on the database.
func NewTypes(d *db.DB) *Types { return &Types{g: d.Gorm} }

type typeRow struct {
	ID            int64
	Code          string
	Name          string
	IsSystem      bool
	CreatedAt     time.Time
	PlaylistCount int
	TrackCount    int
}

func (r typeRow) domain() library.PlaylistType {
	return library.PlaylistType{
		ID: r.ID, Code: r.Code, Name: r.Name, IsSystem: r.IsSystem, CreatedAt: r.CreatedAt.UTC(),
		PlaylistCount: r.PlaylistCount, TrackCount: r.TrackCount,
	}
}

// The counts use ix_playlists_type_id and ix_tracks_type_id.
const selectTypes = `
SELECT t.id, t.code, t.name, t.is_system, t.created_at,
       (SELECT count(*) FROM playlists p WHERE p.type_id = t.id) AS playlist_count,
       (SELECT count(*) FROM tracks tr WHERE tr.type_id = t.id) AS track_count
FROM playlist_types t`

// List returns every type in name order.
func (s *Types) List(ctx context.Context) ([]library.PlaylistType, error) {
	var rows []typeRow
	if err := s.g.WithContext(ctx).Raw(selectTypes + ` ORDER BY t.name, t.id`).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("select playlist types: %w", err)
	}
	out := make([]library.PlaylistType, len(rows))
	for i, r := range rows {
		out[i] = r.domain()
	}
	return out, nil
}

// Get returns one type, or library.ErrTypeNotFound.
func (s *Types) Get(ctx context.Context, id int64) (library.PlaylistType, error) {
	var rows []typeRow
	if err := s.g.WithContext(ctx).Raw(selectTypes+` WHERE t.id = ?`, id).Scan(&rows).Error; err != nil {
		return library.PlaylistType{}, fmt.Errorf("select playlist type %d: %w", id, err)
	}
	if len(rows) == 0 {
		return library.PlaylistType{}, library.ErrTypeNotFound
	}
	return rows[0].domain(), nil
}

type typeInsert struct {
	ID   int64 `gorm:"primaryKey"`
	Code string
	Name string
}

func (typeInsert) TableName() string { return "playlist_types" }

// Create adds a custom type (is_system defaults to false).
func (s *Types) Create(ctx context.Context, code, name string) (int64, error) {
	r := typeInsert{Code: code, Name: name}
	if err := s.g.WithContext(ctx).Create(&r).Error; err != nil {
		return 0, writeError("insert playlist type", err)
	}
	return r.ID, nil
}

// Rename changes the name; the code never changes (§4).
func (s *Types) Rename(ctx context.Context, id int64, name string) error {
	res := s.g.WithContext(ctx).Table("playlist_types").Where("id = ?", id).Update("name", name)
	if res.Error != nil {
		return writeError(fmt.Sprintf("rename playlist type %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrTypeNotFound
	}
	return nil
}

// Delete removes a type. The database refuses it while a playlist or a
// track has the type (fk_playlists_type_id, fk_tracks_type_id: RESTRICT).
func (s *Types) Delete(ctx context.Context, id int64) error {
	res := s.g.WithContext(ctx).Exec(`DELETE FROM playlist_types WHERE id = ?`, id)
	if res.Error != nil {
		return deleteError(fmt.Sprintf("delete playlist type %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrTypeNotFound
	}
	return nil
}

// Users returns the playlists and tracks of the type, at most limit of each.
func (s *Types) Users(ctx context.Context, id int64, limit int) ([]library.Ref, error) {
	var rows []refRow
	err := s.g.WithContext(ctx).Raw(`
(SELECT 'playlist' AS kind, id, name FROM playlists WHERE type_id = ? ORDER BY name, id LIMIT ?)
UNION ALL
(SELECT 'track' AS kind, id, title AS name FROM tracks WHERE type_id = ? ORDER BY title, id LIMIT ?)`,
		id, limit, id, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("select users of playlist type %d: %w", id, err)
	}
	return refs(rows), nil
}
