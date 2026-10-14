package library

import (
	"context"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

// Playlist is a pool of tracks (§4). The order of its tracks is not stored.
type Playlist struct {
	ID         uint64
	Name       string
	Type       TypeRef
	TrackCount int
	// PlayRuleIDs are the play rules applied to every track of the
	// playlist (§7.3), in ID order.
	PlayRuleIDs []uint64
	CreatedAt   time.Time
}

// PlaylistInput is the editable part of a playlist.
type PlaylistInput struct {
	Name   string
	TypeID uint64
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
	// Users returns the clocks that use the playlist in an element or as
	// filler, at most limit.
	Users(ctx context.Context, id uint64, limit int) ([]Ref, error)
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

// CreatePlaylist adds a playlist.
func (s *Service) CreatePlaylist(ctx context.Context, in PlaylistInput) (Playlist, error) {
	n, err := name("name", in.Name)
	if err != nil {
		return Playlist{}, err
	}
	in.Name = n
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	id, err := s.playlists.Create(ctx, in)
	if err != nil {
		return Playlist{}, asFieldError(err)
	}
	return s.playlists.Get(ctx, id)
}

// UpdatePlaylist renames a playlist or changes its type.
func (s *Service) UpdatePlaylist(ctx context.Context, id uint64, in PlaylistInput) (Playlist, error) {
	n, err := name("name", in.Name)
	if err != nil {
		return Playlist{}, err
	}
	in.Name = n
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if err := s.playlists.Update(ctx, id, in); err != nil {
		return Playlist{}, asFieldError(err)
	}
	return s.playlists.Get(ctx, id)
}

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
