package library

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Placement rules of a playlist in clocks (§7.2; clocks.Rule holds the same
// values, library does not depend on clocks).
const (
	RuleFill   = "fill"
	RuleSpread = "spread"
)

// clockRules are the placement rules a playlist may carry.
var clockRules = []string{RuleFill, RuleSpread, "start", "end", "start_end", "middle"}

// maxSpreadCount is the most tracks a spread playlist places in an hour.
const maxSpreadCount = 12

// Playlist is a pool of tracks (§4). The order of its tracks is not stored.
type Playlist struct {
	ID   uint64
	Name string
	Type TypeRef
	// ClockRule is the placement rule of the playlist in clocks; a clock
	// may override it. ClockCount is set for spread only.
	ClockRule  string
	ClockCount *int
	TrackCount int
	// PlayRuleIDs are the play rules applied to every track of the
	// playlist (§7.3), in ID order.
	PlayRuleIDs []uint64
	CreatedAt   time.Time
}

// PlaylistInput is the editable part of a playlist. An empty ClockRule is
// fill.
type PlaylistInput struct {
	Name       string
	TypeID     uint64
	ClockRule  string
	ClockCount *int
}

// PlaylistResult is a saved playlist. RuleChanged: its placement rule
// changed and clocks use it; their updated_at was touched in the same
// transaction, so their planned hours are stale and the caller starts a
// rebuild.
type PlaylistResult struct {
	Playlist    Playlist
	RuleChanged bool
}

// PlaylistStore persists playlists (internal/repository/library).
type PlaylistStore interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) PlaylistStore
	// List returns the playlists, of one type when typeID is not 0.
	List(ctx context.Context, typeID uint64) ([]Playlist, error)
	// Get returns one playlist, or ErrPlaylistNotFound.
	Get(ctx context.Context, id uint64) (Playlist, error)
	// Create adds a playlist. ErrPlaylistNameTaken, ErrUnknownType.
	Create(ctx context.Context, in PlaylistInput) (uint64, error)
	// Update changes a playlist. ErrPlaylistNotFound, ErrPlaylistNameTaken,
	// ErrUnknownType.
	Update(ctx context.Context, id uint64, in PlaylistInput) error
	// Delete removes a playlist and its memberships. ErrPlaylistNotFound;
	// ErrInUse when a clock uses it.
	Delete(ctx context.Context, id uint64) error
	// Users returns the clocks that use the playlist as a clock playlist or
	// as an insert source, at most limit.
	Users(ctx context.Context, id uint64, limit int) ([]Ref, error)
	// TouchClocks sets updated_at of the clocks that use the playlist as a
	// clock playlist and reports whether there were any.
	TouchClocks(ctx context.Context, id uint64) (bool, error)
	// Lock keeps the playlist from being deleted until the transaction
	// ends. ErrPlaylistNotFound.
	Lock(ctx context.Context, id uint64) error
	// SetTracks makes trackIDs the tracks of the playlist. ErrUnknownTrack.
	SetTracks(ctx context.Context, id uint64, trackIDs []uint64) error
}

// ListPlaylists returns the playlists with their type and track count, of
// one type when typeID is not 0.
func (s *Service) ListPlaylists(ctx context.Context, typeID uint64) ([]Playlist, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	ps, err := s.playlists.List(ctx, typeID)
	if err != nil {
		return nil, fmt.Errorf("list playlists: %w", err)
	}
	return ps, nil
}

// GetPlaylist returns one playlist.
func (s *Service) GetPlaylist(ctx context.Context, id uint64) (Playlist, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return s.playlists.Get(ctx, id)
}

// checkPlaylist validates a playlist save and returns it normalised.
func checkPlaylist(in PlaylistInput) (PlaylistInput, error) {
	errs := &validation.Error{}
	n, err := name("name", in.Name)
	if err != nil {
		var v *validation.Error
		if errors.As(err, &v) {
			errs.Fields = append(errs.Fields, v.Fields...)
		}
	}
	in.Name = n
	if in.ClockRule == "" {
		in.ClockRule = RuleFill
	}
	switch {
	case !slices.Contains(clockRules, in.ClockRule):
		errs.Add("clock_rule", validation.Invalid, "must be one of fill, spread, start, end, start_end, middle")
	case in.ClockRule == RuleSpread && in.ClockCount == nil:
		errs.Add("clock_count", validation.ClockCountRequired, "the spread rule needs the number of tracks")
	case in.ClockRule != RuleSpread && in.ClockCount != nil:
		errs.Add("clock_count", validation.ClockCountNotAllowed, "only the spread rule has a count")
	case in.ClockCount != nil && (*in.ClockCount < 1 || *in.ClockCount > maxSpreadCount):
		errs.Add("clock_count", validation.Range, "must be between 1 and %d", maxSpreadCount)
	}
	return in, errs.Err()
}

// CreatePlaylist adds a playlist.
func (s *Service) CreatePlaylist(ctx context.Context, in PlaylistInput) (Playlist, error) {
	in, err := checkPlaylist(in)
	if err != nil {
		return Playlist{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	id, err := s.playlists.Create(ctx, in)
	if err != nil {
		return Playlist{}, asFieldError(err)
	}
	return s.playlists.Get(ctx, id)
}

// UpdatePlaylist changes the name, the type, or the placement rule of a
// playlist. A changed rule touches the clocks that use the playlist in the
// same transaction (§7.2, "Rebuild after a change").
func (s *Service) UpdatePlaylist(ctx context.Context, id uint64, in PlaylistInput) (PlaylistResult, error) {
	in, err := checkPlaylist(in)
	if err != nil {
		return PlaylistResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	changed := false
	err = s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		ps := s.playlists.WithTx(tx)
		if txErr := ps.Lock(ctx, id); txErr != nil {
			return txErr
		}
		old, txErr := ps.Get(ctx, id)
		if txErr != nil {
			return txErr
		}
		if txErr = ps.Update(ctx, id, in); txErr != nil {
			return txErr
		}
		if old.ClockRule == in.ClockRule && equalCount(old.ClockCount, in.ClockCount) {
			return nil
		}
		changed, txErr = ps.TouchClocks(ctx, id)
		return txErr
	})
	if err != nil {
		return PlaylistResult{}, asFieldError(err)
	}
	p, err := s.playlists.Get(ctx, id)
	if err != nil {
		return PlaylistResult{}, err
	}
	return PlaylistResult{Playlist: p, RuleChanged: changed}, nil
}

func equalCount(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// DeletePlaylist deletes a playlist that no clock uses. Its tracks stay in
// the library. An *InUseError names the clocks that use it.
func (s *Service) DeletePlaylist(ctx context.Context, id uint64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.playlists.Delete(ctx, id)
	return refuseDelete(ctx, err, ErrPlaylistInUse, func(ctx context.Context) ([]Ref, error) {
		return s.playlists.Users(ctx, id, MaxUsers)
	})
}

// SetPlaylistTracks makes trackIDs the tracks of the playlist, in one
// transaction. The order of trackIDs is not stored.
func (s *Service) SetPlaylistTracks(ctx context.Context, id uint64, trackIDs []uint64) (Playlist, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		ps := s.playlists.WithTx(tx)
		if err := ps.Lock(ctx, id); err != nil {
			return err
		}
		return ps.SetTracks(ctx, id, trackIDs)
	})
	if err != nil {
		return Playlist{}, asFieldError(err)
	}
	return s.playlists.Get(ctx, id)
}
