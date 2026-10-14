package library

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Track is a file in the library with its metadata (§4).
type Track struct {
	ID     uint64
	Title  string
	Artist *ArtistRef
	// DurationMS is nil until it is entered or read from the file; such a
	// track cannot be planned.
	DurationMS *int
	// FilePath is relative to MEDIA_ROOT, with forward slashes (§8.7).
	FilePath   string
	Format     *string
	Bitrate    *int
	SampleRate *int
	// QualityOK is nil until the file is analysed.
	QualityOK    *bool
	QualityNotes string
	Type         TypeRef
	FileChecksum *string
	CreatedAt    time.Time
	// PlaylistIDs and PlayRuleIDs are in ID order.
	PlaylistIDs []uint64
	PlayRuleIDs []uint64
}

// TrackInput is the editable part of a track. Nil metadata fields are read
// from the file when it exists and ffprobe is available.
type TrackInput struct {
	Title        string
	ArtistID     *uint64
	FilePath     string
	TypeID       uint64
	DurationMS   *int
	Format       *string
	Bitrate      *int
	SampleRate   *int
	QualityOK    *bool
	QualityNotes string
	PlaylistIDs  []uint64
}

// TrackCursor is the position after the last track of a page (title order).
type TrackCursor struct {
	Title string
	ID    uint64
}

// TrackFilter selects tracks. Zero fields do not filter.
type TrackFilter struct {
	TypeID     uint64
	ArtistID   uint64
	PlaylistID uint64
	// Query matches the title or the artist name (case-insensitive).
	Query string
	After *TrackCursor
	Limit int
}

// TrackPage is a page of tracks in title order. Next is set when more
// tracks follow.
type TrackPage struct {
	Items []Track
	Next  *TrackCursor
}

// TrackStore persists tracks (internal/repository/library).
type TrackStore interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) TrackStore
	// List returns at most f.Limit tracks in (title, id) order after
	// f.After.
	List(ctx context.Context, f TrackFilter) ([]Track, error)
	// Get returns one track, or ErrTrackNotFound.
	Get(ctx context.Context, id uint64) (Track, error)
	// Create adds a track without playlists. ErrFilePathTaken,
	// ErrUnknownType, ErrUnknownArtist.
	Create(ctx context.Context, in TrackInput) (uint64, error)
	// Update changes the metadata of a track (not its playlists).
	// ErrTrackNotFound, ErrFilePathTaken, ErrUnknownType, ErrUnknownArtist.
	Update(ctx context.Context, id uint64, in TrackInput) error
	// SetPlaylists makes playlistIDs the playlists of the track.
	// ErrUnknownPlaylist.
	SetPlaylists(ctx context.Context, id uint64, playlistIDs []uint64) error
	// Delete removes a track, its memberships, and its play rule links.
	// ErrTrackNotFound; ErrTrackHasHistory when the play log or the track
	// plan refers to it; ErrInUse when a clock filler pool does.
	Delete(ctx context.Context, id uint64) error
	// Users returns the clocks whose filler pool holds the track, at most
	// limit.
	Users(ctx context.Context, id uint64, limit int) ([]Ref, error)
}

// Files opens track files under MEDIA_ROOT (*media.Library).
type Files interface {
	Open(rel string) (*os.File, error)
}

// Prober reads the format of a track file (*media.Prober).
type Prober interface {
	Probe(ctx context.Context, rel string) (media.Info, error)
}

// ListTracks returns a page of tracks.
func (s *Service) ListTracks(ctx context.Context, f TrackFilter) (TrackPage, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	limit := f.Limit
	// One more row tells whether a next page exists.
	f.Limit++
	ts, err := s.tracks.List(ctx, f)
	if err != nil {
		return TrackPage{}, fmt.Errorf("list tracks: %w", err)
	}
	page := TrackPage{Items: ts}
	if len(ts) > limit {
		page.Items = ts[:limit]
		last := page.Items[limit-1]
		page.Next = &TrackCursor{Title: last.Title, ID: last.ID}
	}
	return page, nil
}

// GetTrack returns one track.
func (s *Service) GetTrack(ctx context.Context, id uint64) (Track, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return s.tracks.Get(ctx, id)
}

// CreateTrack registers a track by its path under MEDIA_ROOT. The file may
// not exist yet. When it does, metadata left empty is read from it.
func (s *Service) CreateTrack(ctx context.Context, in TrackInput) (Track, error) {
	in, err := s.prepareTrack(ctx, in)
	if err != nil {
		return Track{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var id uint64
	err = s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		ts := s.tracks.WithTx(tx)
		created, cerr := ts.Create(ctx, in)
		if cerr != nil {
			return cerr
		}
		id = created
		return ts.SetPlaylists(ctx, id, in.PlaylistIDs)
	})
	if err != nil {
		return Track{}, asFieldError(err)
	}
	return s.tracks.Get(ctx, id)
}

// UpdateTrack replaces the metadata and the playlists of a track.
func (s *Service) UpdateTrack(ctx context.Context, id uint64, in TrackInput) (Track, error) {
	in, err := s.prepareTrack(ctx, in)
	if err != nil {
		return Track{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err = s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		ts := s.tracks.WithTx(tx)
		if uerr := ts.Update(ctx, id, in); uerr != nil {
			return uerr
		}
		return ts.SetPlaylists(ctx, id, in.PlaylistIDs)
	})
	if err != nil {
		return Track{}, asFieldError(err)
	}
	return s.tracks.Get(ctx, id)
}

// DeleteTrack deletes a track that has no play history, is not planned, and
// is in no filler pool. The file is not touched.
func (s *Service) DeleteTrack(ctx context.Context, id uint64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.tracks.Delete(ctx, id)
	return refuseDelete(ctx, err, ErrTrackInUse, func(ctx context.Context) ([]Ref, error) {
		return s.tracks.Users(ctx, id, MaxUsers)
	})
}

// Audio is an open track file for the preview (§6.6). The caller closes
// File.
type Audio struct {
	File        *os.File
	Name        string
	ModTime     time.Time
	Size        int64
	ContentType string
}

// TrackAudio opens the file of a track for the preview player. It returns
// ErrTrackNotFound for an unknown track and media.ErrFileNotFound when the
// file is missing. The preview never reaches the air or the play log.
func (s *Service) TrackAudio(ctx context.Context, id uint64) (Audio, error) {
	t, err := s.GetTrack(ctx, id)
	if err != nil {
		return Audio{}, err
	}
	f, err := s.files.Open(t.FilePath)
	if err != nil {
		return Audio{}, fmt.Errorf("track %d: %w", id, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return Audio{}, fmt.Errorf("track %d: stat file: %w", id, err)
	}
	format := ""
	if t.Format != nil {
		format = *t.Format
	}
	return Audio{
		File:        f,
		Name:        path.Base(t.FilePath),
		ModTime:     info.ModTime().UTC(),
		Size:        info.Size(),
		ContentType: media.ContentType(format, t.FilePath),
	}, nil
}

// prepareTrack checks the input and fills empty metadata from the file. It
// runs before the transaction: reading the file is I/O.
func (s *Service) prepareTrack(ctx context.Context, in TrackInput) (TrackInput, error) {
	errs := &validation.Error{}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		errs.Add("title", validation.Required, "must not be blank")
	}
	if err := media.ValidatePath(in.FilePath); err != nil {
		errs.Add("file_path", validation.Format, "%s", err)
	}
	in.QualityNotes = strings.TrimSpace(in.QualityNotes)
	if err := errs.Err(); err != nil {
		return in, err
	}

	if in.DurationMS != nil && in.Format != nil && in.Bitrate != nil && in.SampleRate != nil {
		return in, nil
	}
	info, err := s.prober.Probe(ctx, in.FilePath)
	switch {
	case errors.Is(err, media.ErrFileNotFound), errors.Is(err, media.ErrProbeUnavailable):
		// Registered by path before the file arrives, or no ffprobe on a
		// development host: the metadata stays as entered.
		return in, nil
	case err != nil:
		// Not fatal: the import phase analyses files properly.
		s.log.WarnContext(ctx, "read track metadata failed", "file_path", in.FilePath, logging.KeyError, err)
		return in, nil
	}
	// The entered value wins; the file fills the gaps.
	in.DurationMS = cmp.Or(in.DurationMS, info.DurationMS)
	in.Format = cmp.Or(in.Format, info.Format)
	in.Bitrate = cmp.Or(in.Bitrate, info.Bitrate)
	in.SampleRate = cmp.Or(in.SampleRate, info.SampleRate)
	return in, nil
}
