// Package clocks stores the clock templates: clocks, their elements, the
// playlists of each element, and the filler pool (§4 of the design). It
// implements the stores of the domain package with the same name; import it
// as clocksrepo next to the domain.
//
// Store is GORM (admin CRUD, §5). CheckStore is pgx: the dry check reads
// play rules (time and smallint[] columns) and the tracks of the elements'
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
	"uq_clocks_name":                         clocks.ErrNameTaken,
	"fk_clock_element_playlists_playlist_id": clocks.ErrUnknownPlaylist,
	"fk_clock_elements_filler_playlist_id":   clocks.ErrUnknownPlaylist,
	"fk_clocks_default_filler_playlist_id":   clocks.ErrUnknownPlaylist,
	"fk_clock_elements_filler_artist_id":     clocks.ErrUnknownArtist,
	"fk_clocks_default_filler_artist_id":     clocks.ErrUnknownArtist,
	"fk_clock_filler_pool_track_id":          clocks.ErrUnknownTrack,
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
	ID           int64
	Name         string
	Status       string
	ElementCount int
	UpdatedAt    time.Time
}

// List returns the clocks in name order. The element count uses
// uq_clock_elements_clock_id_position.
func (s *Store) List(ctx context.Context) ([]clocks.Summary, error) {
	var rows []summaryRow
	err := s.g.WithContext(ctx).Raw(`
SELECT c.id, c.name, c.status, c.updated_at,
       (SELECT count(*) FROM clock_elements e WHERE e.clock_id = c.id) AS element_count
FROM clocks c
ORDER BY c.name, c.id`).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("select clocks: %w", err)
	}
	out := make([]clocks.Summary, len(rows))
	for i, r := range rows {
		out[i] = clocks.Summary{
			ID: r.ID, Name: r.Name, Status: clocks.Status(r.Status),
			ElementCount: r.ElementCount, UpdatedAt: r.UpdatedAt.UTC(),
		}
	}
	return out, nil
}

type clockRow struct {
	ID                      int64
	Name                    string
	Status                  string
	DefaultFillerMode       *string
	DefaultFillerPlaylistID *int64
	DefaultFillerArtistID   *int64
	UpdatedAt               time.Time
}

type elementRow struct {
	ID                  int64
	Position            int
	Placement           string
	FixedMinute         *int
	FixedToleranceMS    int `gorm:"column:fixed_tolerance_ms"`
	NominalDurationMS   int `gorm:"column:nominal_duration_ms"`
	DurationToleranceMS int `gorm:"column:duration_tolerance_ms"`
	FillerMode          string
	FillerPlaylistID    *int64
	FillerArtistID      *int64
}

type linkRow struct {
	OwnerID int64
	OtherID int64
}

// source returns the typed filler column that matches the mode.
func source(mode string, playlistID, artistID *int64) *int64 {
	switch clocks.FillerMode(mode) {
	case clocks.FillerPlaylist:
		return playlistID
	case clocks.FillerArtist:
		return artistID
	}
	return nil
}

// columns splits a filler source into the typed columns.
func columns(f clocks.Filler) (playlistID, artistID *int64) {
	switch f.Mode {
	case clocks.FillerPlaylist:
		return f.SourceID, nil
	case clocks.FillerArtist:
		return nil, f.SourceID
	}
	return nil, nil
}

// Get returns a clock with its elements in position order, or
// clocks.ErrNotFound.
func (s *Store) Get(ctx context.Context, id int64) (clocks.Clock, error) {
	g := s.g.WithContext(ctx)
	var cs []clockRow
	err := g.Raw(`
SELECT id, name, status, default_filler_mode, default_filler_playlist_id, default_filler_artist_id, updated_at
FROM clocks WHERE id = ?`, id).Scan(&cs).Error
	if err != nil {
		return clocks.Clock{}, fmt.Errorf("select clock %d: %w", id, err)
	}
	if len(cs) == 0 {
		return clocks.Clock{}, clocks.ErrNotFound
	}
	c := cs[0]

	var es []elementRow
	err = g.Raw(`
SELECT id, position, placement, fixed_minute, fixed_tolerance_ms, nominal_duration_ms,
       duration_tolerance_ms, filler_mode, filler_playlist_id, filler_artist_id
FROM clock_elements WHERE clock_id = ? ORDER BY position`, id).Scan(&es).Error
	if err != nil {
		return clocks.Clock{}, fmt.Errorf("select elements of clock %d: %w", id, err)
	}
	var links []linkRow
	err = g.Raw(`
SELECT ep.clock_element_id AS owner_id, ep.playlist_id AS other_id
FROM clock_element_playlists ep
JOIN clock_elements e ON e.id = ep.clock_element_id
WHERE e.clock_id = ?
ORDER BY ep.playlist_id`, id).Scan(&links).Error
	if err != nil {
		return clocks.Clock{}, fmt.Errorf("select element playlists of clock %d: %w", id, err)
	}
	playlists := map[int64][]int64{}
	for _, l := range links {
		playlists[l.OwnerID] = append(playlists[l.OwnerID], l.OtherID)
	}

	out := clocks.Clock{
		ID: c.ID, Name: c.Name, Status: clocks.Status(c.Status),
		UpdatedAt: c.UpdatedAt.UTC(), Elements: make([]clocks.Element, len(es)),
	}
	if c.DefaultFillerMode != nil {
		out.DefaultFiller = &clocks.Filler{
			Mode:     clocks.FillerMode(*c.DefaultFillerMode),
			SourceID: source(*c.DefaultFillerMode, c.DefaultFillerPlaylistID, c.DefaultFillerArtistID),
		}
	}
	for i, e := range es {
		out.Elements[i] = clocks.Element{
			ID: e.ID, Placement: clocks.Placement(e.Placement), FixedMinute: e.FixedMinute,
			FixedToleranceMS: e.FixedToleranceMS, NominalDurationMS: e.NominalDurationMS,
			DurationToleranceMS: e.DurationToleranceMS,
			Filler: clocks.Filler{
				Mode:     clocks.FillerMode(e.FillerMode),
				SourceID: source(e.FillerMode, e.FillerPlaylistID, e.FillerArtistID),
			},
			PlaylistIDs: playlists[e.ID],
		}
	}
	return out, nil
}

// Lock locks the clock row, so concurrent saves of one clock run one after
// the other.
func (s *Store) Lock(ctx context.Context, id int64) error {
	var ids []int64
	if err := s.g.WithContext(ctx).Raw(`SELECT id FROM clocks WHERE id = ? FOR UPDATE`, id).Scan(&ids).Error; err != nil {
		return fmt.Errorf("lock clock %d: %w", id, err)
	}
	if len(ids) == 0 {
		return clocks.ErrNotFound
	}
	return nil
}

type clockWrite struct {
	ID                      int64 `gorm:"primaryKey"`
	Name                    string
	Status                  string
	DefaultFillerMode       *string
	DefaultFillerPlaylistID *int64
	DefaultFillerArtistID   *int64
}

func (clockWrite) TableName() string { return "clocks" }

type elementWrite struct {
	ID                  int64 `gorm:"primaryKey"`
	ClockID             int64
	Position            int
	Placement           string
	FixedMinute         *int
	FixedToleranceMS    int `gorm:"column:fixed_tolerance_ms"`
	NominalDurationMS   int `gorm:"column:nominal_duration_ms"`
	DurationToleranceMS int `gorm:"column:duration_tolerance_ms"`
	FillerMode          string
	FillerPlaylistID    *int64
	FillerArtistID      *int64
}

func (elementWrite) TableName() string { return "clock_elements" }

type elementPlaylistWrite struct {
	ClockElementID int64
	PlaylistID     int64
}

func (elementPlaylistWrite) TableName() string { return "clock_element_playlists" }

func clockFields(c clocks.Clock) clockWrite {
	w := clockWrite{ID: c.ID, Name: c.Name, Status: string(c.Status)}
	if c.DefaultFiller != nil {
		w.DefaultFillerMode = new(string(c.DefaultFiller.Mode))
		w.DefaultFillerPlaylistID, w.DefaultFillerArtistID = columns(*c.DefaultFiller)
	}
	return w
}

func elementFields(clockID int64, position int, e clocks.Element) elementWrite {
	w := elementWrite{
		ID: e.ID, ClockID: clockID, Position: position, Placement: string(e.Placement),
		FixedMinute: e.FixedMinute, FixedToleranceMS: e.FixedToleranceMS,
		NominalDurationMS: e.NominalDurationMS, DurationToleranceMS: e.DurationToleranceMS,
		FillerMode: string(e.Filler.Mode),
	}
	w.FillerPlaylistID, w.FillerArtistID = columns(e.Filler)
	return w
}

// Create adds a clock with its elements and their playlists. It runs
// several statements: call it in a transaction.
func (s *Store) Create(ctx context.Context, c clocks.Clock) (int64, error) {
	g := s.g.WithContext(ctx)
	w := clockFields(c)
	if err := g.Create(&w).Error; err != nil {
		return 0, writeError("insert clock", err)
	}
	return w.ID, writeElements(g, w.ID, c.Elements)
}

// Update replaces the clock row and its elements. Elements with an ID keep
// it; elements left out are deleted with their playlists (CASCADE). It runs
// several statements: call it in a transaction.
func (s *Store) Update(ctx context.Context, c clocks.Clock) error {
	g := s.g.WithContext(ctx)
	w := clockFields(c)
	res := g.Model(&clockWrite{}).Where("id = ?", c.ID).Updates(map[string]any{
		"name": w.Name, "status": w.Status, "default_filler_mode": w.DefaultFillerMode,
		"default_filler_playlist_id": w.DefaultFillerPlaylistID,
		"default_filler_artist_id":   w.DefaultFillerArtistID,
		"updated_at":                 gorm.Expr("now()"),
	})
	if res.Error != nil {
		return writeError(fmt.Sprintf("update clock %d", c.ID), res.Error)
	}
	if res.RowsAffected == 0 {
		return clocks.ErrNotFound
	}
	keep := []int64{0}
	for _, e := range c.Elements {
		if e.ID != 0 {
			keep = append(keep, e.ID)
		}
	}
	if err := g.Exec(`DELETE FROM clock_elements WHERE clock_id = ? AND id NOT IN ?`, c.ID, keep).Error; err != nil {
		return fmt.Errorf("delete elements of clock %d: %w", c.ID, err)
	}
	// Positions are unique per clock and not deferrable: move the kept
	// elements out of the way before they take their new positions.
	if err := g.Exec(`UPDATE clock_elements SET position = position + ? WHERE clock_id = ?`, positionShift, c.ID).Error; err != nil {
		return fmt.Errorf("shift elements of clock %d: %w", c.ID, err)
	}
	return writeElements(g, c.ID, c.Elements)
}

// positionShift is above any real position (a clock has at most a few
// hundred elements).
const positionShift = 1_000_000

// writeElements inserts new elements, updates kept ones, and replaces the
// playlists of all of them.
func writeElements(g *gorm.DB, clockID int64, elements []clocks.Element) error {
	ids := make([]int64, len(elements))
	for i, e := range elements {
		w := elementFields(clockID, i, e)
		if e.ID == 0 {
			if err := g.Omit("id").Create(&w).Error; err != nil {
				return writeError(fmt.Sprintf("insert element %d of clock %d", i, clockID), err)
			}
		} else {
			err := g.Model(&elementWrite{}).Where("id = ? AND clock_id = ?", e.ID, clockID).Updates(map[string]any{
				"position": w.Position, "placement": w.Placement, "fixed_minute": w.FixedMinute,
				"fixed_tolerance_ms": w.FixedToleranceMS, "nominal_duration_ms": w.NominalDurationMS,
				"duration_tolerance_ms": w.DurationToleranceMS, "filler_mode": w.FillerMode,
				"filler_playlist_id": w.FillerPlaylistID, "filler_artist_id": w.FillerArtistID,
			}).Error
			if err != nil {
				return writeError(fmt.Sprintf("update element %d of clock %d", e.ID, clockID), err)
			}
		}
		ids[i] = w.ID
	}
	if len(ids) > 0 {
		if err := g.Exec(`DELETE FROM clock_element_playlists WHERE clock_element_id IN ?`, ids).Error; err != nil {
			return fmt.Errorf("delete element playlists of clock %d: %w", clockID, err)
		}
	}
	var links []elementPlaylistWrite
	for i, e := range elements {
		for _, p := range e.PlaylistIDs {
			links = append(links, elementPlaylistWrite{ClockElementID: ids[i], PlaylistID: p})
		}
	}
	if len(links) == 0 {
		return nil
	}
	if err := g.Create(&links).Error; err != nil {
		return writeError(fmt.Sprintf("insert element playlists of clock %d", clockID), err)
	}
	return nil
}

// Delete removes a clock; its elements and pool go with it (CASCADE). Grid
// slots refuse it (clocks.ErrInUse), the play log and the track plan for
// good (clocks.ErrHasHistory).
func (s *Store) Delete(ctx context.Context, id int64) error {
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
	ID        int64
	Weekday   int
	Hour      int
	ValidFrom time.Time
	ValidTo   *time.Time
}

// Users returns the grid slots of the clock (ix_grid_slots_clock_id).
func (s *Store) Users(ctx context.Context, id int64, limit int) ([]clocks.SlotRef, error) {
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

// MissingPlaylists returns the IDs that are not playlists.
func (s *Store) MissingPlaylists(ctx context.Context, ids []int64) ([]int64, error) {
	return s.missing(ctx, `SELECT id FROM playlists WHERE id IN ?`, ids)
}

// MissingArtists returns the IDs that are not artists.
func (s *Store) MissingArtists(ctx context.Context, ids []int64) ([]int64, error) {
	return s.missing(ctx, `SELECT id FROM artists WHERE id IN ?`, ids)
}

func (s *Store) missing(ctx context.Context, sql string, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var found []int64
	if err := s.g.WithContext(ctx).Raw(sql, ids).Scan(&found).Error; err != nil {
		return nil, fmt.Errorf("check ids: %w", err)
	}
	var out []int64
	for _, id := range ids {
		if !slices.Contains(found, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

type poolRow struct {
	TrackID  int64
	Title    string
	Position int
	UsedAt   *time.Time
}

// Pool returns the filler pool in position order
// (uq_clock_filler_pool_clock_id_position).
func (s *Store) Pool(ctx context.Context, clockID int64) ([]clocks.PoolEntry, error) {
	var rows []poolRow
	err := s.g.WithContext(ctx).Raw(`
SELECT p.track_id, t.title, p.position, p.used_at
FROM clock_filler_pool p
JOIN tracks t ON t.id = p.track_id
WHERE p.clock_id = ?
ORDER BY p.position`, clockID).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("select filler pool of clock %d: %w", clockID, err)
	}
	out := make([]clocks.PoolEntry, len(rows))
	for i, r := range rows {
		out[i] = clocks.PoolEntry{TrackID: r.TrackID, Title: r.Title, Position: r.Position, UsedAt: r.UsedAt}
		if r.UsedAt != nil {
			out[i].UsedAt = new(r.UsedAt.UTC())
		}
	}
	return out, nil
}

type poolWrite struct {
	ClockID  int64
	TrackID  int64
	Position int
}

func (poolWrite) TableName() string { return "clock_filler_pool" }

// SetPool deletes the unused pool rows and appends trackIDs after the used
// ones. Run it in a transaction.
func (s *Store) SetPool(ctx context.Context, clockID int64, trackIDs []int64) error {
	g := s.g.WithContext(ctx)
	if err := g.Exec(`DELETE FROM clock_filler_pool WHERE clock_id = ? AND used_at IS NULL`, clockID).Error; err != nil {
		return fmt.Errorf("delete unused pool of clock %d: %w", clockID, err)
	}
	if len(trackIDs) == 0 {
		return nil
	}
	var next int
	err := g.Raw(`SELECT COALESCE(max(position) + 1, 0) FROM clock_filler_pool WHERE clock_id = ?`, clockID).Scan(&next).Error
	if err != nil {
		return fmt.Errorf("select pool position of clock %d: %w", clockID, err)
	}
	rows := make([]poolWrite, len(trackIDs))
	for i, t := range trackIDs {
		rows[i] = poolWrite{ClockID: clockID, TrackID: t, Position: next + i}
	}
	if err := g.Create(&rows).Error; err != nil {
		return writeError(fmt.Sprintf("insert pool of clock %d", clockID), err)
	}
	return nil
}
