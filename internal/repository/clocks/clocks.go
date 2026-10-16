// Package clocks stores the clock templates: clocks, their playlists, and
// their timed inserts (§4 of the design). It implements the stores of the
// domain package with the same name; import it as clocksrepo next to the
// domain.
//
// Store is GORM (admin CRUD, §5). CheckStore is pgx: the dry check reads
// play rules (time and smallint[] columns) and the tracks of the clock's
// playlists, the same tables rotation reads on its hot path.
package clocks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"gorm.io/gorm"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

// Store is the clock store.
type Store struct {
	g *gorm.DB
}

// New returns the store on the database.
func New(d *db.DB) *Store { return &Store{g: d.Gorm} }

// WithTx returns the store bound to tx, as the domain's interface type.
func (s *Store) WithTx(tx db.Tx) clocks.Store { return &Store{g: tx.Gorm()} }

var writeErrors = map[string]error{
	"uq_clocks_name":                 clocks.ErrNameTaken,
	"fk_clock_playlists_playlist_id": clocks.ErrUnknownPlaylist,
	"fk_clock_inserts_playlist_id":   clocks.ErrUnknownPlaylist,
	"fk_clock_inserts_track_id":      clocks.ErrUnknownTrack,
}

// deleteErrors map the foreign keys that refuse a clock delete.
var deleteErrors = map[string]error{
	"fk_grid_slots_clock_id":      clocks.ErrInUse,
	"fk_play_log_clock_id":        clocks.ErrHasHistory,
	"fk_scheduled_items_clock_id": clocks.ErrHasHistory,
}

func writeError(op string, err error) error {
	if ce, ok := db.AsConstraintError(err); ok {
		if mapped, ok := writeErrors[ce.Constraint]; ok {
			return mapped
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

type summaryRow struct {
	ID            uint64
	Name          string
	PlaylistCount int
	InsertCount   int
	UpdatedAt     time.Time
}

// List returns the clocks in name order. The counts use pk_clock_playlists
// and uq_clock_inserts_clock_id_offset_ms.
func (s *Store) List(ctx context.Context) ([]clocks.Summary, error) {
	var rows []summaryRow
	err := s.g.WithContext(ctx).Raw(`
SELECT c.id, c.name, c.updated_at,
       (SELECT count(*) FROM clock_playlists p WHERE p.clock_id = c.id) AS playlist_count,
       (SELECT count(*) FROM clock_inserts i WHERE i.clock_id = c.id) AS insert_count
FROM clocks c
ORDER BY c.name, c.id`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("select clocks: %w", err)
	}
	out := make([]clocks.Summary, len(rows))
	for i, r := range rows {
		out[i] = clocks.Summary{
			ID: r.ID, Name: r.Name, PlaylistCount: r.PlaylistCount, InsertCount: r.InsertCount, UpdatedAt: r.UpdatedAt.UTC(),
		}
	}
	return out, nil
}

type clockRow struct {
	ID        uint64
	Name      string
	UpdatedAt time.Time
}

type playlistRow struct {
	PlaylistID uint64
	ClockRule  *string
	ClockCount *int
	FillWeight *int
	OwnRule    string
	OwnCount   *int
}

type insertRow struct {
	ID          uint64
	OffsetMS    int `gorm:"column:offset_ms"`
	ToleranceMS int `gorm:"column:tolerance_ms"`
	TrackID     *uint64
	PlaylistID  *uint64
}

// Get returns a clock with its playlists in position order (with each
// playlist's own rule) and its inserts by offset, or clocks.ErrNotFound.
func (s *Store) Get(ctx context.Context, id uint64) (clocks.Clock, error) {
	g := s.g.WithContext(ctx)
	var cs []clockRow
	if err := g.Raw(`SELECT id, name, updated_at FROM clocks WHERE id = ?`, id).Scan(&cs).Error; err != nil {
		return clocks.Clock{}, fmt.Errorf("select clock %d: %w", id, err)
	}
	if len(cs) == 0 {
		return clocks.Clock{}, clocks.ErrNotFound
	}
	var ps []playlistRow
	err := g.Raw(`
SELECT cp.playlist_id, cp.clock_rule, cp.clock_count, cp.fill_weight, p.clock_rule AS own_rule, p.clock_count AS own_count
FROM clock_playlists cp
JOIN playlists p ON p.id = cp.playlist_id
WHERE cp.clock_id = ?
ORDER BY cp.position`, id).Scan(&ps).Error
	if err != nil {
		return clocks.Clock{}, fmt.Errorf("select playlists of clock %d: %w", id, err)
	}
	var is []insertRow
	err = g.Raw(`
SELECT id, offset_ms, tolerance_ms, track_id, playlist_id FROM clock_inserts
WHERE clock_id = ? ORDER BY offset_ms`, id).Scan(&is).Error
	if err != nil {
		return clocks.Clock{}, fmt.Errorf("select inserts of clock %d: %w", id, err)
	}

	out := clocks.Clock{
		ID: cs[0].ID, Name: cs[0].Name, UpdatedAt: cs[0].UpdatedAt.UTC(),
		Playlists: make([]clocks.ClockPlaylist, len(ps)), Inserts: make([]clocks.Insert, len(is)),
	}
	for i, p := range ps {
		cp := clocks.ClockPlaylist{
			PlaylistID: p.PlaylistID, Count: p.ClockCount, FillWeight: p.FillWeight,
			Own: clocks.PlaylistRule{Rule: clocks.Rule(p.OwnRule), Count: p.OwnCount},
		}
		if p.ClockRule != nil {
			cp.Rule = new(clocks.Rule(*p.ClockRule))
		}
		out.Playlists[i] = cp
	}
	for i, r := range is {
		out.Inserts[i] = clocks.Insert{
			ID: r.ID, OffsetMS: r.OffsetMS, ToleranceMS: r.ToleranceMS, TrackID: r.TrackID, PlaylistID: r.PlaylistID,
		}
	}
	return out, nil
}

// Lock locks the clock row, so concurrent saves of one clock run one after
// the other.
func (s *Store) Lock(ctx context.Context, id uint64) error {
	var ids []uint64
	if err := s.g.WithContext(ctx).Raw(`SELECT id FROM clocks WHERE id = ? FOR UPDATE`, id).Scan(&ids).Error; err != nil {
		return fmt.Errorf("lock clock %d: %w", id, err)
	}
	if len(ids) == 0 {
		return clocks.ErrNotFound
	}
	return nil
}

type clockWrite struct {
	ID   uint64 `gorm:"primaryKey"`
	Name string
}

func (clockWrite) TableName() string { return "clocks" }

type playlistWrite struct {
	ClockID    uint64
	PlaylistID uint64
	Position   int
	ClockRule  *string
	ClockCount *int
	FillWeight *int
}

func (playlistWrite) TableName() string { return "clock_playlists" }

// Create adds a clock with its playlists and inserts. It runs several
// statements: call it in a transaction.
func (s *Store) Create(ctx context.Context, c clocks.Clock) (uint64, error) {
	g := s.g.WithContext(ctx)
	w := clockWrite{Name: c.Name}
	if err := g.Create(&w).Error; err != nil {
		return 0, writeError("insert clock", err)
	}
	if err := writePlaylists(g, w.ID, c.Playlists); err != nil {
		return 0, err
	}
	return w.ID, writeInserts(g, w.ID, c.Inserts)
}

// Update replaces the clock row, its playlists, and its inserts, and
// touches updated_at so the planned hours of the clock are stale. It runs
// several statements: call it in a transaction.
func (s *Store) Update(ctx context.Context, c clocks.Clock) error {
	g := s.g.WithContext(ctx)
	res := g.Model(&clockWrite{}).Where("id = ?", c.ID).Updates(map[string]any{
		"name": c.Name, "updated_at": gorm.Expr("now()"),
	})
	if res.Error != nil {
		return writeError(fmt.Sprintf("update clock %d", c.ID), res.Error)
	}
	if res.RowsAffected == 0 {
		return clocks.ErrNotFound
	}
	if err := g.Exec(`DELETE FROM clock_playlists WHERE clock_id = ?`, c.ID).Error; err != nil {
		return fmt.Errorf("delete playlists of clock %d: %w", c.ID, err)
	}
	if err := writePlaylists(g, c.ID, c.Playlists); err != nil {
		return err
	}
	// Inserts at a kept offset keep their ID, so plan items still name
	// them until the rebuild.
	keep := []int{-1}
	for _, ins := range c.Inserts {
		keep = append(keep, ins.OffsetMS)
	}
	if err := g.Exec(`DELETE FROM clock_inserts WHERE clock_id = ? AND offset_ms NOT IN ?`, c.ID, keep).Error; err != nil {
		return fmt.Errorf("delete inserts of clock %d: %w", c.ID, err)
	}
	return writeInserts(g, c.ID, c.Inserts)
}

func writePlaylists(g *gorm.DB, clockID uint64, ps []clocks.ClockPlaylist) error {
	if len(ps) == 0 {
		return nil
	}
	rows := make([]playlistWrite, len(ps))
	for i, p := range ps {
		rows[i] = playlistWrite{
			ClockID: clockID, PlaylistID: p.PlaylistID, Position: i, ClockCount: p.Count, FillWeight: p.FillWeight,
		}
		if p.Rule != nil {
			rows[i].ClockRule = new(string(*p.Rule))
		}
	}
	if err := g.Create(&rows).Error; err != nil {
		return writeError(fmt.Sprintf("insert playlists of clock %d", clockID), err)
	}
	return nil
}

// writeInserts adds the inserts, or updates the one stored at the same
// offset (uq_clock_inserts_clock_id_offset_ms).
func writeInserts(g *gorm.DB, clockID uint64, is []clocks.Insert) error {
	for _, ins := range is {
		err := g.Exec(`
INSERT INTO clock_inserts (clock_id, offset_ms, tolerance_ms, track_id, playlist_id)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (clock_id, offset_ms) DO UPDATE SET
    tolerance_ms = EXCLUDED.tolerance_ms, track_id = EXCLUDED.track_id, playlist_id = EXCLUDED.playlist_id`,
			clockID, ins.OffsetMS, ins.ToleranceMS, ins.TrackID, ins.PlaylistID).Error
		if err != nil {
			return writeError(fmt.Sprintf("store insert at %d ms of clock %d", ins.OffsetMS, clockID), err)
		}
	}
	return nil
}

// Delete removes a clock; its playlists and inserts go with it (CASCADE).
// Grid slots refuse it (clocks.ErrInUse), the play log and the track plan
// for good (clocks.ErrHasHistory).
func (s *Store) Delete(ctx context.Context, id uint64) error {
	res := s.g.WithContext(ctx).Exec(`DELETE FROM clocks WHERE id = ?`, id)
	if res.Error != nil {
		if ce, ok := db.AsConstraintError(res.Error); ok && ce.Kind == db.ForeignKey {
			if mapped, ok := deleteErrors[ce.Constraint]; ok {
				return errors.Join(mapped, res.Error)
			}
		}
		return fmt.Errorf("delete clock %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return clocks.ErrNotFound
	}
	return nil
}

type slotRow struct {
	ID        uint64
	Weekday   int
	Hour      int
	ValidFrom time.Time
	ValidTo   *time.Time
}

// Users returns the grid slots of the clock (ix_grid_slots_clock_id).
func (s *Store) Users(ctx context.Context, id uint64, limit int) ([]clocks.SlotRef, error) {
	var rows []slotRow
	err := s.g.WithContext(ctx).Raw(`
SELECT id, weekday, hour, valid_from, valid_to FROM grid_slots
WHERE clock_id = ?
ORDER BY weekday, hour, valid_from
LIMIT ?`, id, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("select grid slots of clock %d: %w", id, err)
	}
	out := make([]clocks.SlotRef, len(rows))
	for i, r := range rows {
		out[i] = clocks.SlotRef{ID: r.ID, Weekday: r.Weekday, Hour: r.Hour, ValidFrom: r.ValidFrom, ValidTo: r.ValidTo}
	}
	return out, nil
}

type ownRuleRow struct {
	ID         uint64
	ClockRule  string
	ClockCount *int
}

// PlaylistRules returns the placement rule of each existing playlist.
func (s *Store) PlaylistRules(ctx context.Context, ids []uint64) (map[uint64]clocks.PlaylistRule, error) {
	out := map[uint64]clocks.PlaylistRule{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []ownRuleRow
	if err := s.g.WithContext(ctx).Raw(`SELECT id, clock_rule, clock_count FROM playlists WHERE id IN ?`, ids).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("select playlist rules: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = clocks.PlaylistRule{Rule: clocks.Rule(r.ClockRule), Count: r.ClockCount}
	}
	return out, nil
}

// MissingTracks returns the IDs that are not tracks.
func (s *Store) MissingTracks(ctx context.Context, ids []uint64) ([]uint64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var found []uint64
	if err := s.g.WithContext(ctx).Raw(`SELECT id FROM tracks WHERE id IN ?`, ids).Scan(&found).Error; err != nil {
		return nil, fmt.Errorf("check track ids: %w", err)
	}
	var out []uint64
	for _, id := range ids {
		if !slices.Contains(found, id) {
			out = append(out, id)
		}
	}
	return out, nil
}
