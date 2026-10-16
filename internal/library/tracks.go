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

	// ReviewedAt is when the manager marked the track checked; nil when it
	// is not. A change of the title, the artist, the file path, or the file
	// itself clears it (docs/phase-21-plan.md §2, decision 8).
	ReviewedAt *time.Time

	// Cue is the part of the file that goes on air, with its fades
	// (docs/phase-22-plan.md); the zero Cue is the whole file.
	Cue Cue

	// NextPlannedAt is the earliest start of a planned or queued item of
	// the track; nil when it is not in the plan.
	NextPlannedAt *time.Time
	// PlayoutState is PlayoutOnAir, PlayoutQueued, or empty.
	PlayoutState string

	// PlaylistIDs and PlayRuleIDs are in ID order.
	PlaylistIDs []uint64
	PlayRuleIDs []uint64
}

// AirDurationMS is the length of the on-air part of the file; nil while the
// duration is unknown.
func (t Track) AirDurationMS() *int {
	if t.DurationMS == nil {
		return nil
	}
	return new(t.Cue.AirDurationMS(*t.DurationMS))
}

// Playout states of a track (Track.PlayoutState).
const (
	// PlayoutOnAir: the track is on air (a plan item or an emergency
	// track).
	PlayoutOnAir = "on_air"
	// PlayoutQueued: a plan item of the track waits in a Liquidsoap queue.
	PlayoutQueued = "queued"
)

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

// TrackCursor is the position after the last track of a page in the
// default order (title, id).
type TrackCursor struct {
	Title string
	ID    uint64
}

// TrackSort is a sort key of the track list. Ties are ordered by title and
// ID.
type TrackSort string

// Sort keys of the track list (docs/phase-20-plan.md §2, decision 2).
const (
	SortTitle  TrackSort = "title"
	SortArtist TrackSort = "artist"
	SortType   TrackSort = "type"
	// SortDuration: tracks without a duration last.
	SortDuration TrackSort = "duration"
	// SortFormat: tracks without a format last.
	SortFormat TrackSort = "format"
	// SortQuality: no issues, then with issues (more issues first), then
	// never analysed (last in both directions).
	SortQuality TrackSort = "quality"
	// SortReviewed: by the time of the reviewed mark; tracks not reviewed
	// last.
	SortReviewed TrackSort = "reviewed"
	// SortNextPlanned: by the next planned start; tracks not in the plan
	// last.
	SortNextPlanned TrackSort = "next_planned_at"
)

// TrackSorts lists every sort key (for the API enum).
func TrackSorts() []TrackSort {
	return []TrackSort{SortTitle, SortArtist, SortType, SortDuration, SortFormat, SortQuality, SortReviewed, SortNextPlanned}
}

// TrackFilter selects tracks and the page of them. Zero fields do not
// filter.
type TrackFilter struct {
	TypeID     uint64
	ArtistID   uint64
	PlaylistID uint64
	// Query matches the title or the artist name (case-insensitive).
	Query string
	// Quality is one of the Quality* filters, or empty.
	Quality string
	// Reviewed, when set, keeps only the tracks with (true) or without
	// (false) the reviewed mark.
	Reviewed *bool
	// InPlan, when set, keeps only the tracks with (true) or without
	// (false) a planned or queued item.
	InPlan *bool
	// InPlaylist, when set, keeps only the tracks in (true) or in no
	// (false) playlist.
	InPlaylist *bool
	// Sort is the sort key; empty is SortTitle.
	Sort TrackSort
	// Desc reverses the sort key; missing values stay last and ties stay
	// in ascending title and ID order.
	Desc bool
	// Offset skips that many tracks of the sorted list.
	Offset int
	// After continues the default order after a cursor, with Offset 0.
	After *TrackCursor
	Limit int
}

// DefaultOrder reports whether f lists in ascending title order, the only
// order a cursor works in.
func (f TrackFilter) DefaultOrder() bool {
	return (f.Sort == "" || f.Sort == SortTitle) && !f.Desc
}

// TrackPage is a page of tracks. Next is set in the default order when
// more tracks follow; Total counts every track matching the filters.
type TrackPage struct {
	Items []Track
	Next  *TrackCursor
	Total int
}

// TrackStore persists tracks (internal/repository/library).
type TrackStore interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) TrackStore
	// List returns at most f.Limit tracks in the order of f, after f.After
	// or skipping f.Offset, and the number of tracks matching the filters
	// (After, Offset, and Limit aside).
	List(ctx context.Context, f TrackFilter) ([]Track, int, error)
	// Get returns one track, or ErrTrackNotFound.
	Get(ctx context.Context, id uint64) (Track, error)
	// Create adds a track without playlists. ErrFilePathTaken,
	// ErrUnknownType, ErrUnknownArtist.
	Create(ctx context.Context, in TrackInput) (uint64, error)
	// Update changes the metadata of a track (not its playlists) and, in
	// the same statement, clears its reviewed mark when the title, the
	// artist, or the file path differs from the stored one.
	// ErrTrackNotFound, ErrFilePathTaken, ErrUnknownType, ErrUnknownArtist.
	Update(ctx context.Context, id uint64, in TrackInput) error
	// SetPlaylists makes playlistIDs the playlists of the track.
	// ErrUnknownPlaylist.
	SetPlaylists(ctx context.Context, id uint64, playlistIDs []uint64) error
	// Delete removes a track, its memberships, and its play rule links.
	// ErrTrackNotFound; ErrTrackHasHistory when the play log or the track
	// plan refers to it; ErrInUse when a clock insert or a one-time insert
	// does.
	Delete(ctx context.Context, id uint64) error
	// Users returns the clocks with an insert of the track and its
	// one-time inserts, at most limit.
	Users(ctx context.Context, id uint64, limit int) ([]Ref, error)
	// DeleteUnreferenced deletes, in one statement, the tracks of ids that
	// no play log row, plan item, clock insert, or one-time insert refers to,
	// and returns their IDs. A reference added while it runs fails the
	// whole statement with ErrInUse or ErrTrackHasHistory: nothing is
	// deleted then.
	DeleteUnreferenced(ctx context.Context, ids []uint64) ([]uint64, error)
	// Blockers returns, for each track of ids that exists, its title and
	// what refers to it (at most limit clocks).
	Blockers(ctx context.Context, ids []uint64, limit int) (map[uint64]TrackBlockers, error)
	// SetReviewed sets the reviewed mark of the tracks of ids (keeping the
	// time of an existing mark) or clears it, and returns the IDs of the
	// tracks that exist.
	SetReviewed(ctx context.Context, ids []uint64, reviewed bool) ([]uint64, error)
	// Titles returns the title of each track of ids that exists; forUpdate
	// locks the rows until the transaction ends.
	Titles(ctx context.Context, ids []uint64, forUpdate bool) (map[uint64]string, error)
	// SetTitles writes the titles and clears the reviewed mark of those
	// tracks, in one statement.
	SetTitles(ctx context.Context, titles map[uint64]string) error
	// CueState locks the track row until the transaction ends and returns
	// its duration, cue, and whether a plan item of it is queued.
	// ErrTrackNotFound.
	CueState(ctx context.Context, id uint64) (CueState, error)
	// SetCue writes the cue of a track; the reviewed mark stays.
	SetCue(ctx context.Context, id uint64, c Cue) error
	// PlannedHours returns the range of the built plan hours that hold a
	// planned item of the track; nil when none does.
	PlannedHours(ctx context.Context, id uint64) (*HourRange, error)
}

// TrackBlockers is what keeps a track from being deleted.
type TrackBlockers struct {
	Title string
	// History: the play log or the track plan refers to the track.
	History bool
	// UsedBy names the clocks with an insert of the track and its one-time
	// inserts.
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
	if !slices.Contains(TrackSorts(), cmp.Or(f.Sort, SortTitle)) {
		return TrackPage{}, validation.Field("query.sort", validation.Invalid, "unknown sort key %q", f.Sort)
	}
	if f.After != nil && (!f.DefaultOrder() || f.Offset > 0) {
		return TrackPage{}, validation.Field("query.cursor", validation.Invalid,
			"a cursor works only with the default order and without offset")
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	limit := f.Limit
	if f.DefaultOrder() {
		// One more row tells whether a next page exists.
		f.Limit++
	}
	ts, total, err := s.tracks.List(ctx, f)
	if err != nil {
		return TrackPage{}, fmt.Errorf("list tracks: %w", err)
	}
	s.markOnAir(ts)
	page := TrackPage{Items: ts, Total: total}
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
	t, err := s.tracks.Get(ctx, id)
	if err != nil {
		return Track{}, err
	}
	ts := []Track{t}
	s.markOnAir(ts)
	return ts[0], nil
}

// markOnAir sets PlayoutOnAir on the track that is on air; the store sets
// PlayoutQueued.
func (s *Service) markOnAir(ts []Track) {
	playing, ok := s.onAir.OnAirTrack()
	if !ok {
		return
	}
	for i := range ts {
		if ts[i].ID == playing {
			ts[i].PlayoutState = PlayoutOnAir
		}
	}
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

// UpdateTrack replaces the metadata and the playlists of a track. A new
// title, artist, or file path clears the reviewed mark.
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
// is in no clock insert or one-time insert. The file is not touched.
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
	// UsedBy names the clocks and one-time inserts that use the track, for
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
	if err := checkTrackIDs(ids, MaxDeleteTracks); err != nil {
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

// MaxReviewTracks bounds one change of the reviewed mark, like a bulk
// delete.
const MaxReviewTracks = 1000

// ReviewTracksResult is the outcome of a change of the reviewed mark, in
// request order.
type ReviewTracksResult struct {
	Updated  []uint64
	NotFound []uint64
}

// ReviewTracks sets the reviewed mark of the tracks (a track already
// reviewed keeps the time of its first mark) or clears it, in one
// statement.
func (s *Service) ReviewTracks(ctx context.Context, ids []uint64, reviewed bool) (ReviewTracksResult, error) {
	if err := checkTrackIDs(ids, MaxReviewTracks); err != nil {
		return ReviewTracksResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	found, err := s.tracks.SetReviewed(ctx, ids, reviewed)
	if err != nil {
		return ReviewTracksResult{}, fmt.Errorf("set reviewed mark: %w", err)
	}
	exists := make(map[uint64]bool, len(found))
	for _, id := range found {
		exists[id] = true
	}
	res := ReviewTracksResult{Updated: []uint64{}, NotFound: []uint64{}}
	for _, id := range ids {
		if exists[id] {
			res.Updated = append(res.Updated, id)
		} else {
			res.NotFound = append(res.NotFound, id)
		}
	}
	return res, nil
}

// checkTrackIDs refuses an empty, too long, or repeating list of a bulk
// request; the handler's schema does the same, this guards other callers.
func checkTrackIDs(ids []uint64, maxIDs int) error {
	if len(ids) == 0 || len(ids) > maxIDs {
		return validation.Field("track_ids", validation.Range, "must list 1 to %d tracks", maxIDs)
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
	in.Title = checkTitle(errs, "title", in.Title)
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
