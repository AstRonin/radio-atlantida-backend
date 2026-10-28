// Package library holds the content library (§4 of the design): playlist
// types, playlists, artists, and tracks.
//
// A playlist is a pool, not a queue: it stores which tracks belong to it,
// never their order; rotation chooses the track. A track can be in several
// playlists. Every playlist and every track has a type, which is only a
// label for the manager.
//
// Deletes that the database refuses (ON DELETE RESTRICT) come back as an
// *InUseError that names what blocks them, so the panel can show it. Tracks
// and artists with play history cannot be deleted at all (§4).
//
// Persistence goes through internal/repository/library.
package library

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Errors returned by the service.
var (
	ErrTypeNotFound     = errors.New("playlist type not found")
	ErrPlaylistNotFound = errors.New("playlist not found")
	ErrArtistNotFound   = errors.New("artist not found")
	ErrTrackNotFound    = errors.New("track not found")

	// ErrSystemType: the four system types are never deleted (§4).
	ErrSystemType = errors.New("a system playlist type cannot be deleted")

	// In-use refusals; returned inside an *InUseError.
	ErrTypeInUse     = errors.New("playlist type is in use")
	ErrPlaylistInUse = errors.New("playlist is in use")
	ErrArtistInUse   = errors.New("artist is in use")
	ErrTrackInUse    = errors.New("track is in use")

	// History refusals: the play log or the track plan refers to the row,
	// and the played-tracks list must stay complete (§4).
	ErrArtistHasHistory = errors.New("artist has play history")
	ErrTrackHasHistory  = errors.New("track has play history or is planned")
)

// Errors returned by the stores. The service turns them into the errors
// above or into field errors.
var (
	// ErrInUse: a delete was refused by a foreign key.
	ErrInUse = errors.New("referenced by other rows")

	ErrTypeCodeTaken     = errors.New("playlist type code is taken")
	ErrTypeNameTaken     = errors.New("playlist type name is taken")
	ErrPlaylistNameTaken = errors.New("playlist name is taken")
	ErrArtistNameTaken   = errors.New("artist name is taken")
	ErrFilePathTaken     = errors.New("file path is taken")

	ErrUnknownType     = errors.New("unknown playlist type")
	ErrUnknownArtist   = errors.New("unknown artist")
	ErrUnknownPlaylist = errors.New("unknown playlist")
	ErrUnknownTrack    = errors.New("unknown track")
)

// Kinds of entities named in a refusal.
const (
	KindPlaylist = "playlist"
	KindTrack    = "track"
	KindClock    = "clock"
	// KindHourInsert is a one-time insert of a planned hour (§7.2).
	KindHourInsert = "hour_insert"
	// KindPlanItem is a track plan item of the track that has not aired
	// (a change of the track's file names it, see FileInUse).
	KindPlanItem = "plan_item"
	// KindPlayout is a track queued in Liquidsoap or on air (named by the
	// ingest service, which owns the check).
	KindPlayout = "playout"
)

// Ref names an entity that blocks a delete.
type Ref struct {
	Kind string
	ID   uint64
	Name string
}

// MaxUsers bounds the entities of each kind listed in an *InUseError.
const MaxUsers = 50

// InUseError refuses a delete and names what blocks it (at most MaxUsers
// of each kind). Err is one of the Err*InUse values.
type InUseError struct {
	Err    error
	UsedBy []Ref
}

func (e *InUseError) Error() string { return e.Err.Error() }

func (e *InUseError) Unwrap() error { return e.Err }

// TxRunner runs fn in one transaction (*db.DB).
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error
}

// dbTimeout bounds each statement or transaction of the service.
const dbTimeout = 5 * time.Second

// Service manages the library.
type Service struct {
	tx        TxRunner
	types     TypeStore
	playlists PlaylistStore
	artists   ArtistStore
	tracks    TrackStore
	files     Files
	prober    Prober
	onAir     OnAir
	log       *slog.Logger
}

// NewService returns the service. onAir tells which track is on air; log
// is the root logger.
func NewService(tx TxRunner, types TypeStore, playlists PlaylistStore, artists ArtistStore, tracks TrackStore,
	files Files, prober Prober, onAir OnAir, log *slog.Logger,
) *Service {
	return &Service{
		tx: tx, types: types, playlists: playlists, artists: artists, tracks: tracks,
		files: files, prober: prober, onAir: onAir,
		log: logging.For(log, logging.ComponentAPI),
	}
}

// refuseDelete turns a delete refused by the database into the error for
// the manager: an *InUseError naming the blockers, or the history error.
func refuseDelete(ctx context.Context, err, inUse error, users func(ctx context.Context) ([]Ref, error)) error {
	if !errors.Is(err, ErrInUse) && !errors.Is(err, ErrTrackHasHistory) && !errors.Is(err, ErrArtistHasHistory) {
		return err
	}
	refs, uerr := users(ctx)
	if uerr != nil {
		return fmt.Errorf("list users: %w", uerr)
	}
	if len(refs) > 0 || errors.Is(err, ErrInUse) {
		return &InUseError{Err: inUse, UsedBy: refs}
	}
	return err
}

// name trims a display name and refuses a blank one.
func name(field, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", validation.Field(field, validation.Required, "must not be blank")
	}
	return v, nil
}

// fieldErrors maps store errors to the request field they concern.
var fieldErrors = []struct {
	err   error
	field string
	rule  validation.Rule
	msg   string
}{
	{ErrTypeCodeTaken, "code", validation.Taken, "another playlist type has this code"},
	{ErrTypeNameTaken, "name", validation.Taken, "another playlist type has this name"},
	{ErrPlaylistNameTaken, "name", validation.Taken, "another playlist has this name"},
	{ErrArtistNameTaken, "name", validation.Taken, "another artist has this name"},
	{ErrFilePathTaken, "file_path", validation.Taken, "another track has this file"},
	{ErrUnknownType, "type_id", validation.UnknownID, "no playlist type has this ID"},
	{ErrUnknownArtist, "artist_id", validation.UnknownID, "no artist has this ID"},
	{ErrUnknownPlaylist, "playlist_ids", validation.UnknownID, "a playlist ID does not exist"},
	{ErrUnknownTrack, "track_ids", validation.UnknownID, "a track ID does not exist"},
}

// asFieldError returns err as a field error when a store error names a
// request field, and err unchanged otherwise.
func asFieldError(err error) error {
	for _, fe := range fieldErrors {
		if errors.Is(err, fe.err) {
			return validation.Field(fe.field, fe.rule, "%s", fe.msg)
		}
	}
	return err
}
