package library

import (
	"context"
	"fmt"
)

// Artist is used for the artist interval of play rules (§4, §7.3).
type Artist struct {
	ID         uint64
	Name       string
	TrackCount int
}

// ArtistRef is the artist of a track.
type ArtistRef struct {
	ID   uint64
	Name string
}

// ArtistStore persists artists (internal/repository/library).
type ArtistStore interface {
	// List returns the artists whose name contains query (case-insensitive;
	// every artist when query is empty), in name order.
	List(ctx context.Context, query string) ([]Artist, error)
	// Get returns one artist, or ErrArtistNotFound.
	Get(ctx context.Context, id uint64) (Artist, error)
	// Create adds an artist. ErrArtistNameTaken.
	Create(ctx context.Context, name string) (uint64, error)
	// Rename changes the name. ErrArtistNotFound, ErrArtistNameTaken.
	Rename(ctx context.Context, id uint64, name string) error
	// Delete removes an artist. ErrArtistNotFound; ErrArtistHasHistory when
	// the play log refers to it; ErrInUse when a track does.
	Delete(ctx context.Context, id uint64) error
	// Users returns the tracks of the artist, at most limit.
	Users(ctx context.Context, id uint64, limit int) ([]Ref, error)
}

// ListArtists returns the artists whose name contains query.
func (s *Service) ListArtists(ctx context.Context, query string) ([]Artist, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	as, err := s.artists.List(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list artists: %w", err)
	}
	return as, nil
}

// GetArtist returns one artist.
func (s *Service) GetArtist(ctx context.Context, id uint64) (Artist, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return s.artists.Get(ctx, id)
}

// CreateArtist adds an artist.
func (s *Service) CreateArtist(ctx context.Context, artistName string) (Artist, error) {
	n, err := name("name", artistName)
	if err != nil {
		return Artist{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	id, err := s.artists.Create(ctx, n)
	if err != nil {
		return Artist{}, asFieldError(err)
	}
	return s.artists.Get(ctx, id)
}

// RenameArtist changes the name of an artist.
func (s *Service) RenameArtist(ctx context.Context, id uint64, artistName string) (Artist, error) {
	n, err := name("name", artistName)
	if err != nil {
		return Artist{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if err := s.artists.Rename(ctx, id, n); err != nil {
		return Artist{}, asFieldError(err)
	}
	return s.artists.Get(ctx, id)
}

// DeleteArtist deletes an artist without tracks, clock references, or play
// history.
func (s *Service) DeleteArtist(ctx context.Context, id uint64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.artists.Delete(ctx, id)
	return refuseDelete(ctx, err, ErrArtistInUse, func(ctx context.Context) ([]Ref, error) {
		return s.artists.Users(ctx, id, MaxUsers)
	})
}
