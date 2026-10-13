package library

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// Playlists is the playlist store.
type Playlists struct {
	g *gorm.DB
}

// NewPlaylists returns the store on the database.
func NewPlaylists(d *db.DB) *Playlists { return &Playlists{g: d.Gorm} }

// WithTx returns the store bound to tx.
func (s *Playlists) WithTx(tx db.Tx) library.PlaylistStore { return &Playlists{g: tx.Gorm()} }

type playlistRow struct {
	ID         int64
	Name       string
	CreatedAt  time.Time
	TypeID     int64
	TypeCode   string
	TypeName   string
	TrackCount int
}

// The track count uses ix_track_playlists_playlist_id.
const selectPlaylists = `
SELECT p.id, p.name, p.created_at, t.id AS type_id, t.code AS type_code, t.name AS type_name,
       (SELECT count(*) FROM track_playlists tp WHERE tp.playlist_id = p.id) AS track_count
FROM playlists p
JOIN playlist_types t ON t.id = p.type_id`

// List returns the playlists in name order, of one type when typeID is not 0.
func (s *Playlists) List(ctx context.Context, typeID int64) ([]library.Playlist, error) {
	var rows []playlistRow
	q := s.g.WithContext(ctx)
	var err error
	if typeID != 0 {
		err = q.Raw(selectPlaylists+` WHERE p.type_id = ? ORDER BY p.name, p.id`, typeID).Scan(&rows).Error
	} else {
		err = q.Raw(selectPlaylists + ` ORDER BY p.name, p.id`).Scan(&rows).Error
	}
	if err != nil {
		return nil, fmt.Errorf("select playlists: %w", err)
	}
	return s.withRules(ctx, rows)
}

// Get returns one playlist, or library.ErrPlaylistNotFound.
func (s *Playlists) Get(ctx context.Context, id int64) (library.Playlist, error) {
	var rows []playlistRow
	if err := s.g.WithContext(ctx).Raw(selectPlaylists+` WHERE p.id = ?`, id).Scan(&rows).Error; err != nil {
		return library.Playlist{}, fmt.Errorf("select playlist %d: %w", id, err)
	}
	if len(rows) == 0 {
		return library.Playlist{}, library.ErrPlaylistNotFound
	}
	ps, err := s.withRules(ctx, rows)
	if err != nil {
		return library.Playlist{}, err
	}
	return ps[0], nil
}

// withRules adds the play rule IDs of the playlists in one query.
func (s *Playlists) withRules(ctx context.Context, rows []playlistRow) ([]library.Playlist, error) {
	if len(rows) == 0 {
		return []library.Playlist{}, nil
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	var links []linkRow
	err := s.g.WithContext(ctx).Raw(`
SELECT playlist_id AS owner_id, rule_id AS other_id FROM playlist_play_rules
WHERE playlist_id IN ? ORDER BY rule_id`, ids).Scan(&links).Error
	if err != nil {
		return nil, fmt.Errorf("select playlist play rules: %w", err)
	}
	rules := group(links)

	out := make([]library.Playlist, len(rows))
	for i, r := range rows {
		out[i] = library.Playlist{
			ID:          r.ID,
			Name:        r.Name,
			Type:        library.TypeRef{ID: r.TypeID, Code: r.TypeCode, Name: r.TypeName},
			TrackCount:  r.TrackCount,
			PlayRuleIDs: rules[r.ID],
			CreatedAt:   r.CreatedAt.UTC(),
		}
	}
	return out, nil
}

type playlistWrite struct {
	ID     int64 `gorm:"primaryKey"`
	Name   string
	TypeID int64
}

func (playlistWrite) TableName() string { return "playlists" }

// Create adds a playlist.
func (s *Playlists) Create(ctx context.Context, in library.PlaylistInput) (int64, error) {
	r := playlistWrite{Name: in.Name, TypeID: in.TypeID}
	if err := s.g.WithContext(ctx).Create(&r).Error; err != nil {
		return 0, writeError("insert playlist", err)
	}
	return r.ID, nil
}

// Update changes the name and the type of a playlist.
func (s *Playlists) Update(ctx context.Context, id int64, in library.PlaylistInput) error {
	res := s.g.WithContext(ctx).Table("playlists").Where("id = ?", id).
		Updates(map[string]any{"name": in.Name, "type_id": in.TypeID})
	if res.Error != nil {
		return writeError(fmt.Sprintf("update playlist %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrPlaylistNotFound
	}
	return nil
}

// Delete removes a playlist; its memberships and play rule links go with it
// (CASCADE). Clock elements and clock filler sources refuse the delete
// (RESTRICT).
func (s *Playlists) Delete(ctx context.Context, id int64) error {
	res := s.g.WithContext(ctx).Exec(`DELETE FROM playlists WHERE id = ?`, id)
	if res.Error != nil {
		return deleteError(fmt.Sprintf("delete playlist %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrPlaylistNotFound
	}
	return nil
}

// Users returns the clocks that use the playlist: in an element, as the
// filler of an element, or as the clock's default filler. clocks holds a
// few dozen rows; the element lookups use the primary keys and
// uq_clock_elements_clock_id_position.
func (s *Playlists) Users(ctx context.Context, id int64, limit int) ([]library.Ref, error) {
	var rows []refRow
	err := s.g.WithContext(ctx).Raw(`
SELECT 'clock' AS kind, c.id, c.name
FROM clocks c
WHERE c.default_filler_playlist_id = ?
   OR EXISTS (SELECT 1 FROM clock_elements e
              JOIN clock_element_playlists ep ON ep.clock_element_id = e.id
              WHERE e.clock_id = c.id AND ep.playlist_id = ?)
   OR EXISTS (SELECT 1 FROM clock_elements e
              WHERE e.clock_id = c.id AND e.filler_playlist_id = ?)
ORDER BY c.name, c.id
LIMIT ?`, id, id, id, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("select users of playlist %d: %w", id, err)
	}
	return refs(rows), nil
}

// Lock takes a key share lock on the playlist, so it cannot be deleted
// before the transaction ends.
func (s *Playlists) Lock(ctx context.Context, id int64) error {
	var ids []int64
	if err := s.g.WithContext(ctx).Raw(`SELECT id FROM playlists WHERE id = ? FOR KEY SHARE`, id).Scan(&ids).Error; err != nil {
		return fmt.Errorf("lock playlist %d: %w", id, err)
	}
	if len(ids) == 0 {
		return library.ErrPlaylistNotFound
	}
	return nil
}

type trackPlaylistRow struct {
	TrackID    int64
	PlaylistID int64
}

func (trackPlaylistRow) TableName() string { return "track_playlists" }

// linkBatch bounds the rows of one insert (two parameters per row; the
// protocol allows 65535).
const linkBatch = 1000

// SetTracks makes trackIDs the members of the playlist: removes the others,
// adds the missing ones. Run it in a transaction.
func (s *Playlists) SetTracks(ctx context.Context, id int64, trackIDs []int64) error {
	g := s.g.WithContext(ctx)
	del := g.Where("playlist_id = ?", id)
	if len(trackIDs) > 0 {
		del = del.Where("track_id NOT IN ?", trackIDs)
	}
	if err := del.Delete(&trackPlaylistRow{}).Error; err != nil {
		return fmt.Errorf("delete tracks of playlist %d: %w", id, err)
	}
	if len(trackIDs) == 0 {
		return nil
	}
	rows := make([]trackPlaylistRow, len(trackIDs))
	for i, t := range trackIDs {
		rows[i] = trackPlaylistRow{TrackID: t, PlaylistID: id}
	}
	err := g.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, linkBatch).Error
	if err != nil {
		return writeError(fmt.Sprintf("insert tracks of playlist %d", id), err)
	}
	return nil
}
