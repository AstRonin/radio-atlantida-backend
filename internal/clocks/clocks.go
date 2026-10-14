// Package clocks holds the clock templates (§4, §6.0, §7.2 of the design):
// 60-minute structures of elements, each drawing a track from the union of
// its playlists. Elements are placed in flow, at the start or the end of the
// hour, or at a fixed minute; fixed elements split the hour into segments,
// which are fitted separately.
//
// Saving a clock runs the dry check (§6.0): the layout on nominal durations,
// refusals per field, the shortfall of each segment, and warnings. A short
// segment needs a filler mode on its holder element; until every short
// segment has one, the clock is incomplete and the panel asks for the
// choice (save-with-filler or save-incomplete).
//
// Rebuilding planned hours after a change and reminders for incomplete
// clocks belong to rotation (Phase 8).
//
// Persistence goes through internal/repository/clocks.
package clocks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
)

// Errors returned by the service.
var (
	ErrNotFound = errors.New("clock not found")
	// ErrInUse: grid slots assign the clock; returned inside an
	// *InUseError.
	ErrInUse = errors.New("clock is in use")
	// ErrHasHistory: the play log or the track plan refers to the clock,
	// which is kept for the played-tracks list (§4).
	ErrHasHistory = errors.New("clock has play history or is planned")
)

// Errors returned by the stores; the service turns them into field errors.
var (
	ErrNameTaken       = errors.New("clock name is taken")
	ErrUnknownPlaylist = errors.New("unknown playlist")
	ErrUnknownArtist   = errors.New("unknown artist")
	ErrUnknownTrack    = errors.New("unknown track")
)

// Placement says where an element goes in the hour (§7.2).
type Placement string

// Placements.
const (
	PlacementFlow  Placement = "flow"
	PlacementStart Placement = "start"
	PlacementEnd   Placement = "end"
	PlacementFixed Placement = "fixed"
)

// FillerMode is the source of filler for a short segment (§7).
type FillerMode string

// Filler modes. A clock's default mode is random, playlist, or artist.
const (
	FillerNone     FillerMode = "none"
	FillerAsk      FillerMode = "ask"
	FillerRandom   FillerMode = "random"
	FillerPlaylist FillerMode = "playlist"
	FillerArtist   FillerMode = "artist"
	FillerPool     FillerMode = "pool"
)

// Status of a clock.
type Status string

// Statuses.
const (
	StatusComplete   Status = "complete"
	StatusIncomplete Status = "incomplete"
)

// Filler is a filler mode with its source: a playlist ID for playlist, an
// artist ID for artist, nil otherwise.
type Filler struct {
	Mode     FillerMode
	SourceID *uint64
}

// Element is one slot of a clock.
type Element struct {
	// ID is 0 for an element that is not stored yet.
	ID          uint64
	Placement   Placement
	FixedMinute *int
	// FixedToleranceMS applies to the start of a fixed element.
	FixedToleranceMS    int
	NominalDurationMS   int
	DurationToleranceMS int
	Filler              Filler
	// PlaylistIDs: the track is chosen from the union of their tracks.
	PlaylistIDs []uint64
}

// target is the target start of a fixed element within the hour.
func (e Element) target() int {
	if e.FixedMinute == nil {
		return 0
	}
	return *e.FixedMinute * minuteMS
}

// earliest is the earliest start of a fixed element.
func (e Element) earliest() int { return e.target() - e.FixedToleranceMS }

// Clock is a 60-minute template.
type Clock struct {
	ID     uint64
	Name   string
	Status Status
	// DefaultFiller applies when the pool is empty or a reminder expires;
	// nil falls back to the global setting (§4, §6.3).
	DefaultFiller *Filler
	// Elements in position order.
	Elements  []Element
	UpdatedAt time.Time
}

// Summary is a clock in the list.
type Summary struct {
	ID           uint64
	Name         string
	Status       Status
	ElementCount int
	UpdatedAt    time.Time
}

// ElementInput is an element of a save. Nil tolerances take the defaults
// from the settings.
type ElementInput struct {
	// ID names an existing element of the clock; 0 for a new one.
	ID                  uint64
	Placement           Placement
	FixedMinute         *int
	FixedToleranceMS    *int
	NominalDurationMS   int
	DurationToleranceMS *int
	Filler              Filler
	PlaylistIDs         []uint64
}

// Input is the editable part of a clock.
type Input struct {
	Name          string
	DefaultFiller *Filler
	Elements      []ElementInput
}

// SegmentChoice is the filler chosen for one segment in the dialog.
type SegmentChoice struct {
	Segment int
	Filler  Filler
}

// FillerChoice is the answer of the filler dialog (§6.0).
type FillerChoice struct {
	Segments []SegmentChoice
	// PoolTrackIDs, when not nil, replace the unused rows of the clock's
	// filler pool, in this order.
	PoolTrackIDs []uint64
}

// PoolEntry is a track of a clock's filler pool. Used rows stay as history.
type PoolEntry struct {
	TrackID  uint64
	Title    string
	Position int
	UsedAt   *time.Time
}

// Result is a saved clock with its dry check.
type Result struct {
	Clock    Clock
	Layout   Layout
	Warnings []Warning
	// Replaced are fixed elements removed by the save (§7.2).
	Replaced []Element
}

// View is a clock with its layout.
type View struct {
	Clock  Clock
	Layout Layout
}

// Ref names an entity that blocks a delete.
type Ref struct {
	Kind string
	ID   uint64
	Name string
}

// KindGridSlot is the kind of a grid slot in a refusal.
const KindGridSlot = "grid_slot"

// MaxUsers bounds the grid slots listed in an *InUseError.
const MaxUsers = 50

// InUseError refuses the delete of a clock that the grid uses, and names the
// slots.
type InUseError struct {
	UsedBy []Ref
}

func (e *InUseError) Error() string { return ErrInUse.Error() }

func (e *InUseError) Unwrap() error { return ErrInUse }

// SlotRef is a grid slot that assigns a clock.
type SlotRef struct {
	ID        uint64
	Weekday   int
	Hour      int
	ValidFrom time.Time
	ValidTo   *time.Time
}

// Store persists clocks (internal/repository/clocks).
type Store interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) Store
	// List returns every clock in name order.
	List(ctx context.Context) ([]Summary, error)
	// Get returns a clock with its elements, or ErrNotFound.
	Get(ctx context.Context, id uint64) (Clock, error)
	// Lock locks the clock row until the transaction ends. ErrNotFound.
	Lock(ctx context.Context, id uint64) error
	// Create adds a clock with its elements. ErrNameTaken,
	// ErrUnknownPlaylist, ErrUnknownArtist.
	Create(ctx context.Context, c Clock) (uint64, error)
	// Update replaces the clock and its elements: elements with an ID are
	// updated, those left out are deleted, new ones are added. ErrNotFound,
	// ErrNameTaken, ErrUnknownPlaylist, ErrUnknownArtist.
	Update(ctx context.Context, c Clock) error
	// Delete removes a clock. ErrNotFound; ErrInUse while grid slots use
	// it; ErrHasHistory.
	Delete(ctx context.Context, id uint64) error
	// Users returns the grid slots of the clock, at most limit.
	Users(ctx context.Context, id uint64, limit int) ([]SlotRef, error)
	// MissingPlaylists, MissingArtists, and MissingTracks return the IDs
	// that do not exist.
	MissingPlaylists(ctx context.Context, ids []uint64) ([]uint64, error)
	MissingArtists(ctx context.Context, ids []uint64) ([]uint64, error)
	// Pool returns the filler pool in position order.
	Pool(ctx context.Context, clockID uint64) ([]PoolEntry, error)
	// SetPool replaces the unused pool rows with trackIDs, in order, after
	// the used ones. ErrUnknownTrack.
	SetPool(ctx context.Context, clockID uint64, trackIDs []uint64) error
}

// Candidate is a track of an element's playlists, as the dry check sees it.
type Candidate struct {
	TrackID    uint64
	DurationMS *int
	// PlaylistIDs are the requested playlists that hold the track.
	PlaylistIDs []uint64
	// RuleIDs are the play rules of the track and of all its playlists.
	RuleIDs []uint64
}

// Candidates are the tracks of a set of playlists and their play rules.
type Candidates struct {
	Tracks []Candidate
	Rules  map[uint64]playrules.RuleInput
}

// CheckStore reads what the dry check needs (internal/repository/clocks).
type CheckStore interface {
	// Candidates returns the tracks of the playlists with their rules.
	Candidates(ctx context.Context, playlistIDs []uint64) (Candidates, error)
	// TrackSeparationHours returns the longest track separation interval.
	TrackSeparationHours(ctx context.Context) (int, error)
}

// Hour is one hour of the grid.
type Hour struct {
	StartUTC time.Time
	// Weekday (1 = Monday ... 7 = Sunday) and Hour are station local.
	Weekday int
	Hour    int
}

// GridHours returns the hours the grid assigns to a clock (*grid.Service).
type GridHours interface {
	ClockHours(ctx context.Context, clockID uint64, from, to time.Time) ([]Hour, error)
}

// Settings are the settings the service reads.
type Settings struct {
	DurationToleranceMS int
	FixedToleranceMS    int
	GridHorizonDays     int
}

// SettingsFunc reads the settings when a clock is saved.
type SettingsFunc func(ctx context.Context) (Settings, error)

// TxRunner runs fn in one transaction (*db.DB).
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error
}

// dbTimeout bounds each statement or transaction of the service.
const dbTimeout = 5 * time.Second

// checkTimeout bounds the reads of the dry check.
const checkTimeout = 10 * time.Second

// Service manages clocks.
type Service struct {
	tx       TxRunner
	store    Store
	check    CheckStore
	grid     GridHours
	settings SettingsFunc
	now      func() time.Time
}

// NewService returns the service.
func NewService(tx TxRunner, store Store, check CheckStore, grid GridHours, settings SettingsFunc, now func() time.Time) *Service {
	return &Service{tx: tx, store: store, check: check, grid: grid, settings: settings, now: now}
}

// List returns every clock.
func (s *Service) List(ctx context.Context) ([]Summary, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	cs, err := s.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list clocks: %w", err)
	}
	return cs, nil
}

// Get returns a clock with its layout.
func (s *Service) Get(ctx context.Context, id uint64) (View, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := s.store.Get(ctx, id)
	if err != nil {
		return View{}, err
	}
	l, err := LayoutOf(c.Elements)
	if err != nil {
		return View{}, fmt.Errorf("layout of clock %d: %w", id, err)
	}
	return View{Clock: c, Layout: l}, nil
}
