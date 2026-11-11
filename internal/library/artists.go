package library

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
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

// ArtistSort is a sort key of the artist list (docs/artists-paging-plan.md).
type ArtistSort string

// Sort keys of the artist list. Ties are ordered by name, then ID, ascending.
const (
	ArtistSortName       ArtistSort = "name"
	ArtistSortTrackCount ArtistSort = "track_count"
)

// ArtistSorts lists every sort key of the artist list.
func ArtistSorts() []ArtistSort {
	return []ArtistSort{ArtistSortName, ArtistSortTrackCount}
}

// ArtistFilter selects artists and the page of them.
type ArtistFilter struct {
	// Query keeps the artists whose name contains it (case-insensitive);
	// empty keeps every artist.
	Query string
	// Sort is the sort key; empty is ArtistSortName.
	Sort ArtistSort
	// Desc reverses the sort key; ties stay in ascending name and ID order.
	Desc bool
	// Offset skips that many artists of the sorted list.
	Offset int
	Limit  int
}

// ArtistPage is a page of artists. Total counts every artist matching the
// query, whatever the offset and limit.
type ArtistPage struct {
	Items []Artist
	Total int
}

// ArtistStore persists artists (internal/repository/library).
type ArtistStore interface {
	// List returns at most f.Limit artists in the order of f, skipping
	// f.Offset, and the number of artists matching f.Query (Offset and
	// Limit aside).
	List(ctx context.Context, f ArtistFilter) ([]Artist, int, error)
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

// ListArtists returns a page of the artists whose name contains f.Query.
func (s *Service) ListArtists(ctx context.Context, f ArtistFilter) (ArtistPage, error) {
	if !slices.Contains(ArtistSorts(), cmp.Or(f.Sort, ArtistSortName)) {
		return ArtistPage{}, validation.Field("query.sort", validation.Invalid, "unknown sort key %q", f.Sort)
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	as, total, err := s.artists.List(ctx, f)
	if err != nil {
		return ArtistPage{}, fmt.Errorf("list artists: %w", err)
	}
	return ArtistPage{Items: as, Total: total}, nil
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

// DeleteArtist deletes an artist. It is refused while tracks refer to the
// artist (ErrArtistInUse, with the tracks as users: artist_in_use) or the
// play log does (ErrArtistHasHistory: artist_has_history). Nothing cascades.
func (s *Service) DeleteArtist(ctx context.Context, id uint64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.artists.Delete(ctx, id)
	return refuseDelete(ctx, err, ErrArtistInUse, func(ctx context.Context) ([]Ref, error) {
		return s.artists.Users(ctx, id, MaxUsers)
	})
}
