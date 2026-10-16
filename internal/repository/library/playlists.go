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
	ID         uint64
	Name       string
	ClockRule  string
	ClockCount *int
	CreatedAt  time.Time
	TypeID     uint64
	TypeCode   string
	TypeName   string
	TrackCount int
}

// The track count uses ix_track_playlists_playlist_id.
const selectPlaylists = `
SELECT p.id, p.name, p.clock_rule, p.clock_count, p.created_at, t.id AS type_id, t.code AS type_code, t.name AS type_name,
       (SELECT count(*) FROM track_playlists tp WHERE tp.playlist_id = p.id) AS track_count
FROM playlists p
JOIN playlist_types t ON t.id = p.type_id`

// List returns the playlists in name order, of one type when typeID is not 0.
func (s *Playlists) List(ctx context.Context, typeID uint64) ([]library.Playlist, error) {
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
func (s *Playlists) Get(ctx context.Context, id uint64) (library.Playlist, error) {
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
	ids := make([]uint64, len(rows))
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
			ClockRule:   r.ClockRule,
			ClockCount:  r.ClockCount,
			TrackCount:  r.TrackCount,
			PlayRuleIDs: rules[r.ID],
			CreatedAt:   r.CreatedAt.UTC(),
		}
	}
	return out, nil
}

type playlistWrite struct {
	ID         uint64 `gorm:"primaryKey"`
	Name       string
	TypeID     uint64
	ClockRule  string
	ClockCount *int
}

func (playlistWrite) TableName() string { return "playlists" }

// clockRule is the placement rule of an input; empty is fill
// (library.PlaylistInput).
func clockRule(in library.PlaylistInput) string {
	if in.ClockRule == "" {
		return library.RuleFill
	}
	return in.ClockRule
}

// Create adds a playlist.
func (s *Playlists) Create(ctx context.Context, in library.PlaylistInput) (uint64, error) {
	r := playlistWrite{Name: in.Name, TypeID: in.TypeID, ClockRule: clockRule(in), ClockCount: in.ClockCount}
	if err := s.g.WithContext(ctx).Create(&r).Error; err != nil {
		return 0, writeError("insert playlist", err)
	}
	return r.ID, nil
}

// Update changes the name, the type, and the placement rule of a playlist.
func (s *Playlists) Update(ctx context.Context, id uint64, in library.PlaylistInput) error {
	res := s.g.WithContext(ctx).Table("playlists").Where("id = ?", id).
		Updates(map[string]any{"name": in.Name, "type_id": in.TypeID, "clock_rule": clockRule(in), "clock_count": in.ClockCount})
	if res.Error != nil {
		return writeError(fmt.Sprintf("update playlist %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrPlaylistNotFound
	}
	return nil
}

// Delete removes a playlist; its memberships and play rule links go with it
// (CASCADE). Clock playlists and clock inserts refuse the delete
// (RESTRICT).
func (s *Playlists) Delete(ctx context.Context, id uint64) error {
	res := s.g.WithContext(ctx).Exec(`DELETE FROM playlists WHERE id = ?`, id)
	if res.Error != nil {
		return deleteError(fmt.Sprintf("delete playlist %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return library.ErrPlaylistNotFound
	}
	return nil
}

// Users returns the clocks that use the playlist: as a clock playlist or as
// the source of an insert. clocks holds a few dozen rows; the lookups use
// pk_clock_playlists and ix_clock_inserts_playlist_id.
func (s *Playlists) Users(ctx context.Context, id uint64, limit int) ([]library.Ref, error) {
	var rows []refRow
	err := s.g.WithContext(ctx).Raw(`
SELECT 'clock' AS kind, c.id, c.name
FROM clocks c
WHERE EXISTS (SELECT 1 FROM clock_playlists cp WHERE cp.clock_id = c.id AND cp.playlist_id = ?)
   OR EXISTS (SELECT 1 FROM clock_inserts i WHERE i.clock_id = c.id AND i.playlist_id = ?)
ORDER BY c.name, c.id
LIMIT ?`, id, id, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("select users of playlist %d: %w", id, err)
	}
	return refs(rows), nil
}

// Lock takes a key share lock on the playlist, so it cannot be deleted
// before the transaction ends.
func (s *Playlists) Lock(ctx context.Context, id uint64) error {
	var ids []uint64
	if err := s.g.WithContext(ctx).Raw(`SELECT id FROM playlists WHERE id = ? FOR KEY SHARE`, id).Scan(&ids).Error; err != nil {
		return fmt.Errorf("lock playlist %d: %w", id, err)
	}
	if len(ids) == 0 {
		return library.ErrPlaylistNotFound
	}
	return nil
}

type trackPlaylistRow struct {
	TrackID    uint64
	PlaylistID uint64
}

func (trackPlaylistRow) TableName() string { return "track_playlists" }

// linkBatch bounds the rows of one insert (two parameters per row; the
// protocol allows 65535).
const linkBatch = 1000

// SetTracks makes trackIDs the members of the playlist: removes the others,
// adds the missing ones. Run it in a transaction.
func (s *Playlists) SetTracks(ctx context.Context, id uint64, trackIDs []uint64) error {
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

// TouchClocks sets updated_at of the clocks that use the playlist as a
// clock playlist, so their planned hours are stale (§4), and reports
// whether there were any (pk_clock_playlists, ix_clock_playlists_playlist_id).
func (s *Playlists) TouchClocks(ctx context.Context, id uint64) (bool, error) {
	res := s.g.WithContext(ctx).Exec(`
UPDATE clocks SET updated_at = now()
WHERE id IN (SELECT clock_id FROM clock_playlists WHERE playlist_id = ?)`, id)
	if res.Error != nil {
		return false, fmt.Errorf("touch clocks of playlist %d: %w", id, res.Error)
	}
	return res.RowsAffected > 0, nil
}
