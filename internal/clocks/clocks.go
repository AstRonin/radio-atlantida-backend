// Package clocks holds the clock templates (§4, §6.0, §7.2 of the design): a
// clock is a set of playlists and timed inserts. Each playlist has a
// placement rule (fill, spread, start, end, start_end, middle), stored on
// the playlist and overridable per clock; a clock needs at least one fill
// playlist. A timed insert plays a track, or a track drawn from a playlist,
// at a time of the hour with seconds and a start tolerance. Rotation
// (internal/rotation) lays out the hour from them; tracks of any length are
// used.
//
// Saving a clock is one step (§6.0): validate every field, store the clock
// with its playlists and inserts in one transaction, and run the dry check
// (warnings that never block). The API adds the sample hour, built by
// rotation, and starts a rebuild of the clock's planned hours.
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
	ErrUnknownTrack    = errors.New("unknown track")
)

// HourMS is the length of every clock (§4: length_seconds is fixed at 3600).
const HourMS = 3_600_000

// Limits of the clock fields (§7.2).
const (
	// MaxOffsetMS is the latest start of an insert: 59:59.
	MaxOffsetMS = HourMS - 1000
	// MaxToleranceMS is the widest start tolerance of an insert.
	MaxToleranceMS = 600_000
	// MaxSpreadCount is the most tracks a spread playlist places.
	MaxSpreadCount = 12
	// MaxFillWeight is the largest share of a fill playlist.
	MaxFillWeight = 100
)

// Rule is the placement rule of a playlist in clocks (§7.2).
type Rule string

// Placement rules.
const (
	// RuleFill fills the time not taken by inserts and rule tracks.
	RuleFill Rule = "fill"
	// RuleSpread places a number of tracks spread over the hour.
	RuleSpread Rule = "spread"
	// RuleStart places the first track of the hour.
	RuleStart Rule = "start"
	// RuleEnd places the last track of the hour, ending at 60:00.
	RuleEnd Rule = "end"
	// RuleStartEnd places one track at the start and one at the end.
	RuleStartEnd Rule = "start_end"
	// RuleMiddle places a track that starts at about 30:00.
	RuleMiddle Rule = "middle"
)

// Rules lists every placement rule.
func Rules() []Rule {
	return []Rule{RuleFill, RuleSpread, RuleStart, RuleEnd, RuleStartEnd, RuleMiddle}
}

// Valid reports whether r is a placement rule.
func (r Rule) Valid() bool {
	switch r {
	case RuleFill, RuleSpread, RuleStart, RuleEnd, RuleStartEnd, RuleMiddle:
		return true
	}
	return false
}

// PlaylistRule is the placement rule a playlist carries itself.
type PlaylistRule struct {
	Rule Rule
	// Count is set for spread only.
	Count *int
}

// ClockPlaylist is a playlist of a clock.
type ClockPlaylist struct {
	PlaylistID uint64
	// Rule and Count override the playlist's rule in this clock; nil keeps
	// it. Count is set only with a spread override.
	Rule  *Rule
	Count *int
	// FillWeight is the share of a fill playlist (1 to 100); either every
	// fill playlist of a clock has one or none.
	FillWeight *int
	// Own is the playlist's own rule, read with the clock.
	Own PlaylistRule
}

// EffectiveRule is the rule of the playlist in this clock: the override,
// else the playlist's own.
func (cp ClockPlaylist) EffectiveRule() Rule {
	if cp.Rule != nil {
		return *cp.Rule
	}
	return cp.Own.Rule
}

// EffectiveCount is the spread count of the playlist in this clock; nil
// unless the effective rule is spread.
func (cp ClockPlaylist) EffectiveCount() *int {
	switch {
	case cp.EffectiveRule() != RuleSpread:
		return nil
	case cp.Rule != nil:
		return cp.Count
	}
	return cp.Own.Count
}

// Insert is a timed insert of the template: it plays in every hour of the
// clock.
type Insert struct {
	// ID is 0 for an insert that is not stored yet.
	ID          uint64
	OffsetMS    int
	ToleranceMS int
	// Exactly one of TrackID and PlaylistID is set.
	TrackID    *uint64
	PlaylistID *uint64
}

// Clock is a 60-minute template.
type Clock struct {
	ID   uint64
	Name string
	// Playlists in position order.
	Playlists []ClockPlaylist
	// Inserts by offset.
	Inserts   []Insert
	UpdatedAt time.Time
}

// Summary is a clock in the list.
type Summary struct {
	ID            uint64
	Name          string
	PlaylistCount int
	InsertCount   int
	UpdatedAt     time.Time
}

// PlaylistInput is a playlist of a save.
type PlaylistInput struct {
	PlaylistID uint64
	Rule       *Rule
	Count      *int
	FillWeight *int
}

// InsertInput is an insert of a save. A nil tolerance takes the default
// from the settings.
type InsertInput struct {
	OffsetMS    int
	ToleranceMS *int
	TrackID     *uint64
	PlaylistID  *uint64
}

// Input is the editable part of a clock.
type Input struct {
	Name      string
	Playlists []PlaylistInput
	Inserts   []InsertInput
}

// Result is a saved clock with the warnings of its dry check.
type Result struct {
	Clock    Clock
	Warnings []Warning
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
	// Get returns a clock with its playlists and inserts, or ErrNotFound.
	Get(ctx context.Context, id uint64) (Clock, error)
	// Lock locks the clock row until the transaction ends. ErrNotFound.
	Lock(ctx context.Context, id uint64) error
	// Create adds a clock with its playlists and inserts. ErrNameTaken,
	// ErrUnknownPlaylist, ErrUnknownTrack.
	Create(ctx context.Context, c Clock) (uint64, error)
	// Update replaces the clock, its playlists, and its inserts (an insert
	// at the offset of a stored one keeps its ID) and touches updated_at.
	// ErrNotFound, ErrNameTaken, ErrUnknownPlaylist, ErrUnknownTrack.
	Update(ctx context.Context, c Clock) error
	// Delete removes a clock. ErrNotFound; ErrInUse while grid slots use
	// it; ErrHasHistory.
	Delete(ctx context.Context, id uint64) error
	// Users returns the grid slots of the clock, at most limit.
	Users(ctx context.Context, id uint64, limit int) ([]SlotRef, error)
	// PlaylistRules returns the own rule of each of the playlists that
	// exists.
	PlaylistRules(ctx context.Context, ids []uint64) (map[uint64]PlaylistRule, error)
	// MissingTracks returns the IDs that are not tracks.
	MissingTracks(ctx context.Context, ids []uint64) ([]uint64, error)
}

// Candidate is a track of a playlist, as the dry check sees it.
type Candidate struct {
	TrackID uint64
	// AirMS is the length on air; nil when the track cannot be planned (no
	// duration).
	AirMS *int
	// PlaylistIDs are the requested playlists that hold the track.
	PlaylistIDs []uint64
	// RuleIDs are the play rules of the track and of all its playlists.
	RuleIDs []uint64
}

// Candidates are the tracks of a set of playlists and their play rules.
type Candidates struct {
	Tracks []Candidate
	Rules  map[uint64]playrules.RuleInput
	// PlaylistRules are the rule IDs attached to each requested playlist.
	PlaylistRules map[uint64][]uint64
}

// CheckStore reads what the dry check needs (internal/repository/clocks).
type CheckStore interface {
	// Candidates returns the tracks of the playlists with their rules.
	Candidates(ctx context.Context, playlistIDs []uint64) (Candidates, error)
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
	// FixedToleranceMS is the default start tolerance of a new insert.
	FixedToleranceMS int
	GridHorizonDays  int
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

// Get returns a clock with its playlists and inserts.
func (s *Service) Get(ctx context.Context, id uint64) (Clock, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return s.store.Get(ctx, id)
}
