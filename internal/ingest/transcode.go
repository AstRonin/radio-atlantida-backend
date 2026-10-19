package ingest

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/workerpool"
)

// transcodeWorkers: an encode decodes and encodes on one core and may run
// for a minute on a long programme; one at a time keeps playout safe.
const transcodeWorkers = 10

// optimalSuffix marks the copy written next to the original (decision 8).
const optimalSuffix = ".optimal"

// Outcomes of one transcoded track.
const (
	outcomeTranscoded   = "transcoded"
	outcomeBusy         = "busy"
	outcomeNothingToFix = "nothing to fix"
	outcomePathTaken    = "path taken"
)

// OutputMode says where the fixed file goes.
type OutputMode string

// Output modes.
const (
	// OutputReplace replaces the original file.
	OutputReplace OutputMode = "replace"
	// OutputCopy writes "<name>.optimal.<ext>" next to the original, which
	// stays on disk; the track then points at the copy.
	OutputCopy OutputMode = "copy"
)

// TranscodeRequest is a transcode of tracks.
type TranscodeRequest struct {
	TrackIDs []uint64
	Fix      library.Fix
	Output   OutputMode
}

// Transcode starts a transcode operation and returns its ID (decision 8).
func (s *Service) Transcode(ctx context.Context, req TranscodeRequest) (uint64, error) {
	if err := s.toolsAvailable(); err != nil {
		return 0, err
	}
	if _, err := s.existing(ctx, req.TrackIDs); err != nil {
		return 0, err
	}
	return s.start(ctx, operations.KindTranscode, func(ctx context.Context, id uint64) (string, error) {
		return s.runTranscode(ctx, id, req)
	})
}

func (s *Service) runTranscode(ctx context.Context, id uint64, req TranscodeRequest) (string, error) {
	set, err := s.readSettings(ctx)
	if err != nil {
		return "", err
	}
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	ts, err := s.store.Tracks(rctx, req.TrackIDs)
	cancel()
	if err != nil {
		return "", fmt.Errorf("read tracks: %w", err)
	}
	t := newTally()
	err = workerpool.ForEach(ctx, transcodeWorkers, ts, func(ctx context.Context, tr Track) error {
		outcome := s.transcodeTrack(ctx, tr, req, set, t)
		done := t.add(outcome, tr.FilePath, outcome == outcomeFailed)
		s.ops.Progress(id, operations.KindTranscode, done, len(ts), tr.FilePath)
		return ctx.Err()
	})
	if err != nil {
		return "", fmt.Errorf("transcode stopped after %d of %d tracks: %w", t.done, len(ts), err)
	}
	return t.summary(outcomeTranscoded, outcomeNothingToFix, outcomeBusy, outcomePathTaken, outcomeMissing,
		outcomeDeleted, outcomeUnreadable, outcomeFailed), nil
}

// transcodeTrack fixes the file of one track and stores the analysis of
// the new file; a reset cue is recorded in t.
func (s *Service) transcodeTrack(ctx context.Context, tr Track, req TranscodeRequest, set Settings, t *tally) string {
	if busy, err := s.busy(ctx, tr.ID, set); err != nil {
		return s.trackFailed(ctx, tr, "check busy", err)
	} else if busy {
		return outcomeBusy
	}

	// The stored analysis may be old: measure the file as it is now. The
	// second pass of loudnorm needs these values of this very file.
	info, loud, err := s.analyse(ctx, tr.FilePath)
	switch {
	case errors.Is(err, media.ErrFileNotFound):
		return outcomeMissing
	case err != nil:
		if ctx.Err() != nil {
			return outcomeFailed
		}
		s.log.WarnContext(ctx, "transcode: file not readable as audio", "track_id", tr.ID, "file_path", tr.FilePath, logging.KeyError, err)
		return outcomeUnreadable
	}
	now := measurementsOf(info, loud, tr.Measurements.SourceBitrate)
	issues := Check(now, set.Standard)
	if !slices.ContainsFunc(issues, req.Fix.Covers) {
		return outcomeNothingToFix
	}

	enc, ok := encodingFor(req.Fix, now, set.Standard)
	if !ok {
		s.log.WarnContext(ctx, "transcode: the format of the file cannot be written; use Optimize track",
			"track_id", tr.ID, "file_path", tr.FilePath)
		return outcomeFailed
	}
	var norm *media.Normalize
	if loud != nil && (slices.Contains(issues, library.IssueLoudness) || slices.Contains(issues, library.IssueTruePeak)) {
		norm = &media.Normalize{TargetLUFS: set.Standard.LoudnessLUFS, TruePeakDBTP: set.Standard.MaxTruePeakDBTP, Measured: *loud}
	}

	final, tmp := targetPaths(tr.FilePath, enc.Output.Ext, req.Output, tr.ID)
	if final != tr.FilePath {
		dctx, cancel := context.WithTimeout(ctx, dbTimeout)
		owner, taken, terr := s.store.TrackByPath(dctx, final)
		cancel()
		if terr != nil {
			return s.trackFailed(ctx, tr, "check target path", terr)
		}
		if taken && owner != tr.ID {
			s.log.WarnContext(ctx, "transcode: target path belongs to another track", "track_id", tr.ID, "file_path", final, "other_track_id", owner)
			return outcomePathTaken
		}
	}

	if err = s.ff.Encode(ctx, tr.FilePath, tmp, enc, norm); err != nil {
		s.removeTemp(ctx, tmp)
		return s.trackFailed(ctx, tr, "encode", err)
	}
	a, err := s.analyseOutput(ctx, tr, tmp, final, now)
	if err != nil {
		s.removeTemp(ctx, tmp)
		return s.trackFailed(ctx, tr, "analyse new file", err)
	}
	a.Issues = Check(a.Measurements, set.Standard)

	// The encode took a while: the track may have been planned meanwhile.
	if busy, err := s.busy(ctx, tr.ID, set); err != nil || busy {
		s.removeTemp(ctx, tmp)
		if err != nil {
			return s.trackFailed(ctx, tr, "check busy", err)
		}
		return outcomeBusy
	}
	if err := s.files.Commit(tmp, final); err != nil {
		s.removeTemp(ctx, tmp)
		return s.trackFailed(ctx, tr, "write new file", err)
	}
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	reset, serr := s.store.SaveAnalysis(dctx, tr.ID, a)
	if serr != nil {
		deleted := errors.Is(serr, library.ErrTrackNotFound)
		switch {
		case final != tr.FilePath:
			s.removeTemp(ctx, final)
		case deleted:
			// No track refers to the file any more; files are never
			// deleted (§4), so the new one stays where the old one was.
		default:
			// The original is replaced already; the next analysis reads
			// the new file.
			s.log.ErrorContext(ctx, "transcode: file replaced but its analysis not saved; run the analysis of the track",
				"track_id", tr.ID, "file_path", final, logging.KeyError, serr)
		}
		if deleted {
			return s.trackDeleted(ctx, tr)
		}
		return s.trackFailed(ctx, tr, "save analysis", serr)
	}
	if req.Output == OutputReplace && final != tr.FilePath {
		if err := s.files.Remove(tr.FilePath); err != nil {
			s.log.WarnContext(ctx, "transcode: original file not removed", "track_id", tr.ID, "file_path", tr.FilePath, logging.KeyError, err)
		}
	}
	s.cueReset(ctx, tr, reset, t)
	s.log.InfoContext(ctx, "track transcoded", "track_id", tr.ID, "fix", string(req.Fix), "from", tr.FilePath, "to", final)
	return outcomeTranscoded
}

// analyseOutput reads the new file at tmp, which becomes final. A file
// made from a lossy source keeps the lowest bitrate it went through.
func (s *Service) analyseOutput(ctx context.Context, tr Track, tmp, final string, src Measurements) (Analysis, error) {
	info, loud, err := s.analyse(ctx, tmp)
	if err != nil {
		return Analysis{}, err
	}
	sum, err := s.files.Checksum(ctx, tmp)
	if err != nil {
		return Analysis{}, err
	}
	source := tr.Measurements.SourceBitrate
	if b, ok := src.effectiveBitrate(); ok {
		source = &b
	}
	return Analysis{
		FilePath: final, Checksum: sum, Measurements: measurementsOf(info, loud, source), AnalyzedAt: s.now().UTC(),
	}, nil
}

// busy reports whether the track's file must not change now (decision 8).
func (s *Service) busy(ctx context.Context, id uint64, set Settings) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	now := s.now().UTC()
	ids, err := s.store.Busy(ctx, []uint64{id}, now, now.Add(set.BusyAhead))
	if err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}

func (s *Service) removeTemp(ctx context.Context, rel string) {
	if err := s.files.Remove(rel); err != nil {
		s.log.WarnContext(ctx, "transcode: temporary file not removed", "file_path", rel, logging.KeyError, err)
	}
}

// encodingFor returns how the fix writes a file with m. Fix loudness keeps
// the format and writes a lossy file at its own bitrate rounded up to a
// standard one; Optimize writes the standard. ok is false when the format
// of the file cannot be written.
func encodingFor(fix library.Fix, m Measurements, std Standard) (media.Encoding, bool) {
	if fix == library.FixOptimize {
		out, ok := std.output()
		return media.Encoding{Output: out, BitrateKbps: std.BitrateKbps, SampleRate: std.SampleRate, Channels: std.Channels}, ok
	}
	if m.Format == nil || m.Codec == nil || m.SampleRate == nil || m.Channels == nil {
		return media.Encoding{}, false
	}
	out, ok := media.OutputFor(*m.Format, *m.Codec)
	bitrate := 0
	if m.Bitrate != nil {
		bitrate = *m.Bitrate
	}
	return media.Encoding{Output: out, BitrateKbps: roundUpBitrate(bitrate), SampleRate: *m.SampleRate, Channels: *m.Channels}, ok
}

// targetPaths returns where the fixed file of src goes, and the temporary
// name it is written under first: hidden, in the same folder, so a rename
// puts it in place and an import skips it.
func targetPaths(src, ext string, mode OutputMode, trackID uint64) (final, tmp string) {
	dir, base := path.Split(src)
	stem := strings.TrimSuffix(base, path.Ext(base))
	if mode == OutputCopy && !strings.HasSuffix(stem, optimalSuffix) {
		stem += optimalSuffix
	}
	final = dir + stem + ext
	tmp = dir + "." + stem + ".transcode-" + strconv.FormatUint(trackID, 10) + ext
	return final, tmp
}
