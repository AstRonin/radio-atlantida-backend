package ingest

import (
	"context"
	"fmt"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// ReplaceFile points the track id at the file rel under the media root and
// analyses that file, in one step (POST /api/tracks/{id}/replace-file). It
// runs synchronously, so the caller reads the track afterwards.
//
// The track's old file is not removed: another track may use it. A track
// used for playout is refused, as is a path that another track has. Replacing
// with the same path and the same content changes nothing; any other replace
// clears the reviewed mark (SaveAnalysis) and the stored waveform stops
// matching the file by its checksum.
func (s *Service) ReplaceFile(ctx context.Context, id uint64, rel string) error {
	if err := media.ValidatePath(rel); err != nil {
		return validation.Field("file_path", validation.Format, "%s", err)
	}
	// The worker runs one media operation at a time; a replace takes the same
	// slot, so it never overlaps an import, an analysis, or a transcode.
	if !s.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer s.running.Store(false)

	set, err := s.readSettings(ctx)
	if err != nil {
		return err
	}
	tr, err := s.trackForReplace(ctx, id)
	if err != nil {
		return err
	}
	if err = s.uses.FileInUse(ctx, id); err != nil {
		return err
	}
	busy, err := s.busy(ctx, id, set)
	if err != nil {
		return fmt.Errorf("check busy track %d: %w", id, err)
	}
	if busy {
		return &library.InUseError{Err: library.ErrTrackInUse, UsedBy: []library.Ref{
			{Kind: library.KindPlayout, Name: "queued in Liquidsoap or on air"},
		}}
	}
	if err = s.checkPathFree(ctx, id, rel); err != nil {
		return err
	}
	// Hashed up front: the no-op check below and the analysis use the same
	// read of the file.
	sum, err := s.files.Checksum(ctx, rel)
	if err != nil {
		return fmt.Errorf("read file of track %d: %w", id, err)
	}
	if rel == tr.FilePath && tr.Checksum != nil && *tr.Checksum == sum {
		return nil
	}
	if err := s.toolsAvailable(); err != nil {
		return err
	}

	// The replaced file is an original: the analysis does not keep the
	// bitrate of the old file (Checksum nil), and it always decodes the file
	// (force). SaveAnalysis stores the new path and clears the reviewed mark.
	res := s.analyseTrack(ctx, Track{ID: id, FilePath: rel, Hashed: sum}, set.Standard, true, newTally())
	switch res.outcome {
	case outcomeAnalysed:
		s.log.InfoContext(ctx, "track file replaced", "track_id", id, "from", tr.FilePath, "to", rel)
		return nil
	case outcomeUnreadable:
		return validation.Field("file_path", validation.Invalid, "the file is not readable as audio")
	case outcomeDeleted:
		return library.ErrTrackNotFound
	default:
		// analyseTrack has logged the step that failed.
		return fmt.Errorf("replace file of track %d: %s", id, res.outcome)
	}
}

// trackForReplace returns the track, or library.ErrTrackNotFound.
func (s *Service) trackForReplace(ctx context.Context, id uint64) (Track, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	ts, err := s.store.Tracks(ctx, []uint64{id})
	if err != nil {
		return Track{}, fmt.Errorf("read track %d: %w", id, err)
	}
	if len(ts) == 0 {
		return Track{}, library.ErrTrackNotFound
	}
	return ts[0], nil
}

// checkPathFree refuses a path that another track has. The track's own path
// is free to keep.
func (s *Service) checkPathFree(ctx context.Context, id uint64, rel string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	owner, taken, err := s.store.TrackByPath(ctx, rel)
	if err != nil {
		return fmt.Errorf("check path of track %d: %w", id, err)
	}
	if taken && owner != id {
		return validation.Field("file_path", validation.Taken, "track %d has this file", owner)
	}
	return nil
}
