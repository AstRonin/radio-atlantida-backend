package library

import (
	"cmp"
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

// artistOrder is the ORDER BY of f, built from the validated sort key only:
// the key in f's direction, then name and ID ascending, so equal keys keep
// their place between pages. track_count is the output column of
// selectArtists.
func artistOrder(f library.ArtistFilter) (string, error) {
	dir := " ASC"
	if f.Desc {
		dir = " DESC"
	}
	switch cmp.Or(f.Sort, library.ArtistSortName) {
	case library.ArtistSortName:
		return "a.name" + dir + ", a.id", nil
	case library.ArtistSortTrackCount:
		return "track_count" + dir + ", a.name, a.id", nil
	default:
		return "", fmt.Errorf("unknown artist sort %q", f.Sort)
	}
}

// List returns at most f.Limit artists in the order of f, skipping f.Offset,
// and the number of artists whose name contains f.Query.
//
// The list is paged with OFFSET, like the track list: the panel needs page
// numbers, the sort by track count, and the total, and the library holds
// thousands of artists, not millions. The page and the count are two
// statements, not one transaction: a list view accepts a total that is off
// by a concurrent import or delete, as the track list does.
//
// The track count is a correlated subquery on ix_tracks_artist_id (index
// only scan). In name order PostgreSQL walks uq_artists_name and counts the
// page rows alone; in track_count order it counts every matching artist and
// sorts them (top-N heapsort on the first pages). No index is added.
// EXPLAIN ANALYZE, PostgreSQL 17.11 in Docker Desktop on an Apple M1 Pro,
// 5 000 artists, 50 000 tracks, limit 50: name asc first page 0.4 ms, name
// desc last page (offset 4 950) 10 ms; track_count desc first page 9 ms,
// track_count asc last page 12 ms, track_count desc with q=a 6 ms. The
// count: 0.4 ms, with q=a 2 ms.
func (s *Artists) List(ctx context.Context, f library.ArtistFilter) ([]library.Artist, int, error) {
	order, err := artistOrder(f)
	if err != nil {
		return nil, 0, err
	}
	where, args := "", []any{}
	if f.Query != "" {
		where = " WHERE a.name ILIKE ?"
		args = append(args, containsPattern(f.Query))
	}
	var rows []artistRow
	q := s.g.WithContext(ctx)
	if err := q.Raw(selectArtists+where+" ORDER BY "+order+" LIMIT ? OFFSET ?", append(args, f.Limit, f.Offset)...).
		Scan(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("select artists: %w", err)
	}
	var total int
	if err := q.Raw("SELECT count(*) FROM artists a"+where, args...).Scan(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count artists: %w", err)
	}
	out := make([]library.Artist, len(rows))
	for i, r := range rows {
		out[i] = r.domain()
	}
	return out, total, nil
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

// Delete removes an artist. Tracks and the play log refuse it (RESTRICT):
// library.ErrInUse for a track, library.ErrArtistHasHistory for the play log
// (deleteError maps the constraint).
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
