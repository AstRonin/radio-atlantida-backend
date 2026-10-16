package library

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
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

	// Analysis of the file (internal/ingest); nil until it is analysed.
	Codec           *string
	Channels        *int
	LoudnessLUFS    *float64
	TruePeakDBTP    *float64
	LoudnessRangeLU *float64
	// SourceBitrate is the bitrate of the lossy file this one was
	// transcoded from (the low_bitrate issue stays with the copy).
	SourceBitrate *int
	QualityIssues []QualityIssue
	AnalyzedAt    *time.Time

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

// QualityIssue is a problem that the analysis found in a track file
// (docs/phase-11-plan.md §2, decision 5). The codes are part of the API and
// of ck_tracks_quality_issues: add codes, never rename one.
type QualityIssue string

// Quality issues.
const (
	// IssueFormat: the container is not the target format.
	IssueFormat QualityIssue = "format"
	// IssueSampleRate: the sample rate is not the target.
	IssueSampleRate QualityIssue = "sample_rate"
	// IssueChannels: the channel count is not the target.
	IssueChannels QualityIssue = "channels"
	// IssueLoudness: the integrated loudness is too far from the target.
	IssueLoudness QualityIssue = "loudness"
	// IssueTruePeak: the true peak is above the limit.
	IssueTruePeak QualityIssue = "true_peak"
	// IssueLowBitrate: a lossy source below the minimum bitrate. A
	// transcode cannot add the lost detail; the source must be replaced.
	IssueLowBitrate QualityIssue = "low_bitrate"
	// IssueFileMissing: the file is not under the media root.
	IssueFileMissing QualityIssue = "file_missing"
)

// QualityIssues lists every issue code (for the API enum).
func QualityIssues() []QualityIssue {
	return []QualityIssue{IssueFormat, IssueSampleRate, IssueChannels, IssueLoudness, IssueTruePeak, IssueLowBitrate, IssueFileMissing}
}

// Fix is a transcode the manager can start for a track: the buttons "Fix
// loudness" and "Optimize track" of the panel.
type Fix string

// Fixes.
const (
	// FixLoudness brings the loudness to the target; the format stays.
	FixLoudness Fix = "loudness"
	// FixOptimize converts to the broadcast standard and fixes the
	// loudness.
	FixOptimize Fix = "optimize"
)

// Covers reports whether the fix removes issue.
func (f Fix) Covers(issue QualityIssue) bool {
	switch issue {
	case IssueLoudness, IssueTruePeak:
		return f == FixLoudness || f == FixOptimize
	case IssueFormat, IssueSampleRate, IssueChannels:
		return f == FixOptimize
	default:
		return false
	}
}

// FixesFor returns the fixes that remove at least one of issues, in the
// order loudness, optimize.
func FixesFor(issues []QualityIssue) []Fix {
	var out []Fix
	for _, f := range []Fix{FixLoudness, FixOptimize} {
		if slices.ContainsFunc(issues, f.Covers) {
			out = append(out, f)
		}
	}
	return out
}

// Quality filters of the track list.
const (
	// QualityOK: analysed, without issues.
	QualityOK = "ok"
	// QualityIssuesFound: at least one issue.
	QualityIssuesFound = "issues"
	// QualityUnanalysed: never analysed.
	QualityUnanalysed = "unanalysed"
)

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
	// Quality is one of the Quality* filters, or empty.
	Quality string
	After   *TrackCursor
	Limit   int
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
	// DeleteUnreferenced deletes, in one statement, the tracks of ids that
	// no play log row, plan item, hour insert, or filler pool refers to,
	// and returns their IDs. A reference added while it runs fails the
	// whole statement with ErrInUse or ErrTrackHasHistory: nothing is
	// deleted then.
	DeleteUnreferenced(ctx context.Context, ids []uint64) ([]uint64, error)
	// Blockers returns, for each track of ids that exists, its title and
	// what refers to it (at most limit clocks).
	Blockers(ctx context.Context, ids []uint64, limit int) (map[uint64]TrackBlockers, error)
}

// TrackBlockers is what keeps a track from being deleted.
type TrackBlockers struct {
	Title string
	// History: the play log, the track plan, or an hour insert refers to
	// the track.
	History bool
	// UsedBy names the clocks whose filler pool holds the track.
	UsedBy []Ref
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

// MaxDeleteTracks bounds one bulk delete, like an analysis by IDs.
const MaxDeleteTracks = 1000

// deleteAttempts bounds the bulk delete statements one request runs. A
// statement fails when a track gets a new reference while it runs (a plan
// build may plan a just imported track); the next attempt leaves that
// track out.
const deleteAttempts = 3

// TrackRefusal is a track a bulk delete did not remove.
type TrackRefusal struct {
	TrackID uint64
	// Title is empty for ErrTrackNotFound.
	Title string
	// Err is ErrTrackInUse, ErrTrackHasHistory, or ErrTrackNotFound.
	Err error
	// UsedBy names the clocks whose filler pool holds the track, for
	// ErrTrackInUse.
	UsedBy []Ref
}

// DeleteTracksResult is the outcome of a bulk delete, in request order.
type DeleteTracksResult struct {
	Deleted []uint64
	Refused []TrackRefusal
}

// DeleteTracksError stops a bulk delete on an error that is not a
// refusal. Deleted is 0 when the delete itself failed (it is one
// statement), and the number of deleted tracks when reading the refusals
// failed after it.
type DeleteTracksError struct {
	Deleted   int
	Requested int
	Err       error
}

func (e *DeleteTracksError) Error() string {
	return fmt.Sprintf("deleted %d of %d tracks before the error: %v", e.Deleted, e.Requested, e.Err)
}

func (e *DeleteTracksError) Unwrap() error { return e.Err }

// DeleteTracks deletes the tracks that DeleteTrack would delete, in one
// statement, and reports why each other track was kept. A refused track
// does not keep the others. It runs to the end when the caller goes away:
// the manager may leave the page right after the click.
func (s *Service) DeleteTracks(ctx context.Context, ids []uint64) (DeleteTracksResult, error) {
	if err := checkDeleteTracks(ids); err != nil {
		return DeleteTracksResult{}, err
	}
	ctx = context.WithoutCancel(ctx)

	deleted, err := s.deleteUnreferenced(ctx, ids)
	if err != nil {
		return DeleteTracksResult{}, &DeleteTracksError{Requested: len(ids), Err: err}
	}
	gone := make(map[uint64]bool, len(deleted))
	for _, id := range deleted {
		gone[id] = true
	}
	res := DeleteTracksResult{Deleted: []uint64{}, Refused: []TrackRefusal{}}
	var kept []uint64
	for _, id := range ids {
		if gone[id] {
			res.Deleted = append(res.Deleted, id)
		} else {
			kept = append(kept, id)
		}
	}

	if len(kept) > 0 {
		bctx, cancel := context.WithTimeout(ctx, dbTimeout)
		blockers, err := s.tracks.Blockers(bctx, kept, MaxUsers)
		cancel()
		if err != nil {
			return DeleteTracksResult{}, &DeleteTracksError{
				Deleted: len(res.Deleted), Requested: len(ids), Err: fmt.Errorf("read why tracks were kept: %w", err),
			}
		}
		for _, id := range kept {
			b, ok := blockers[id]
			r := TrackRefusal{TrackID: id, Title: b.Title}
			switch {
			case !ok:
				r.Err = ErrTrackNotFound
			case len(b.UsedBy) > 0:
				// As DeleteTrack: the pool is what the manager can fix.
				r.Err, r.UsedBy = ErrTrackInUse, b.UsedBy
			default:
				// History, or a plan item a rebuild removed right after
				// the delete: the track was planned when it was refused.
				r.Err = ErrTrackHasHistory
			}
			res.Refused = append(res.Refused, r)
		}
	}

	s.log.InfoContext(ctx, "tracks deleted",
		"requested", len(ids), "deleted", len(res.Deleted), "refused", len(res.Refused))
	return res, nil
}

// deleteUnreferenced runs the bulk delete statement, again when a new
// reference failed it.
func (s *Service) deleteUnreferenced(ctx context.Context, ids []uint64) ([]uint64, error) {
	for attempt := 1; ; attempt++ {
		dctx, cancel := context.WithTimeout(ctx, dbTimeout)
		deleted, err := s.tracks.DeleteUnreferenced(dctx, ids)
		cancel()
		raced := errors.Is(err, ErrInUse) || errors.Is(err, ErrTrackHasHistory)
		switch {
		case err == nil:
			return deleted, nil
		case !raced:
			return nil, fmt.Errorf("delete tracks: %w", err)
		case attempt == deleteAttempts:
			return nil, fmt.Errorf("delete tracks: new references in each of %d attempts: %w", deleteAttempts, err)
		}
		s.log.InfoContext(ctx, "bulk delete met a new reference; retrying", "attempt", attempt, logging.KeyError, err)
	}
}

// checkDeleteTracks refuses an empty, too long, or repeating list; the
// handler's schema does the same, this guards other callers.
func checkDeleteTracks(ids []uint64) error {
	if len(ids) == 0 || len(ids) > MaxDeleteTracks {
		return validation.Field("track_ids", validation.Range, "must list 1 to %d tracks", MaxDeleteTracks)
	}
	seen := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			return validation.Field("track_ids", validation.Invalid, "track %d is listed twice", id)
		}
		seen[id] = struct{}{}
	}
	return nil
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
