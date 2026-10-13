package library

import (
	"context"
	"fmt"
	"time"
)

// PlaylistType labels playlists and tracks (§4). System types (music,
// jingle, news, segment) can be renamed but not deleted; the code of a type
// never changes.
type PlaylistType struct {
	ID        int64
	Code      string
	Name      string
	IsSystem  bool
	CreatedAt time.Time
	// Usage counts.
	PlaylistCount int
	TrackCount    int
}

// TypeRef is the type of a playlist or a track.
type TypeRef struct {
	ID   int64
	Code string
	Name string
}

// TypeStore persists playlist types (internal/repository/library).
type TypeStore interface {
	// List returns every type with its usage counts.
	List(ctx context.Context) ([]PlaylistType, error)
	// Get returns one type, or ErrTypeNotFound.
	Get(ctx context.Context, id int64) (PlaylistType, error)
	// Create adds a custom type. ErrTypeCodeTaken, ErrTypeNameTaken.
	Create(ctx context.Context, code, name string) (int64, error)
	// Rename changes the name. ErrTypeNotFound, ErrTypeNameTaken.
	Rename(ctx context.Context, id int64, name string) error
	// Delete removes a type. ErrTypeNotFound; ErrInUse when a playlist or
	// a track has it.
	Delete(ctx context.Context, id int64) error
	// Users returns the playlists and tracks that have the type, at most
	// limit of each kind.
	Users(ctx context.Context, id int64, limit int) ([]Ref, error)
}

// ListTypes returns every playlist type.
func (s *Service) ListTypes(ctx context.Context) ([]PlaylistType, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	ts, err := s.types.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list playlist types: %w", err)
	}
	return ts, nil
}

// GetType returns one playlist type.
func (s *Service) GetType(ctx context.Context, id int64) (PlaylistType, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return s.types.Get(ctx, id)
}

// CreateType adds a custom type. code is a slug, checked by the API schema
// and the database (ck_playlist_types_code_slug).
func (s *Service) CreateType(ctx context.Context, code, displayName string) (PlaylistType, error) {
	n, err := name("name", displayName)
	if err != nil {
		return PlaylistType{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	id, err := s.types.Create(ctx, code, n)
	if err != nil {
		return PlaylistType{}, asFieldError(err)
	}
	return s.types.Get(ctx, id)
}

// RenameType changes the display name of a type, system types included.
func (s *Service) RenameType(ctx context.Context, id int64, displayName string) (PlaylistType, error) {
	n, err := name("name", displayName)
	if err != nil {
		return PlaylistType{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if err := s.types.Rename(ctx, id, n); err != nil {
		return PlaylistType{}, asFieldError(err)
	}
	return s.types.Get(ctx, id)
}

// DeleteType deletes a custom type that no playlist or track has.
// ErrSystemType for a system type; an *InUseError naming the playlists and
// tracks for a type in use.
func (s *Service) DeleteType(ctx context.Context, id int64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	t, err := s.types.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.IsSystem {
		return ErrSystemType
	}
	err = s.types.Delete(ctx, id)
	return refuseDelete(ctx, err, ErrTypeInUse, func(ctx context.Context) ([]Ref, error) {
		return s.types.Users(ctx, id, MaxUsers)
	})
}
