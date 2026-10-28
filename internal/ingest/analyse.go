package ingest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
	"github.com/AstRonin/radio-atlantida-backend/internal/workerpool"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

const (
	// analyseWorkers: files decoded at a time, by an import and by an
	// analysis. Four cores decode while the rest stay with playout on the
	// same machine (docs/phase-11-plan.md §2, decision 9).
	analyseWorkers = 4
	// maxAnalyseAll bounds an analysis of every unanalysed track; the
	// library holds a few thousand.
	maxAnalyseAll = 50000
)

// Outcomes of one analysed track.
const (
	outcomeAnalysed  = "analysed"
	outcomeRechecked = "rechecked"
	outcomeMissing   = "file missing"
	// outcomeDeleted: the track was deleted while the operation ran (a
	// delete is not refused during media operations, docs/phase-19-plan.md
	// decision 8).
	outcomeDeleted = "track deleted"
)

// AnalyseRequest selects the tracks to analyse.
type AnalyseRequest struct {
	// TrackIDs are the tracks; empty means every track never analysed.
	TrackIDs []uint64
	// Force decodes files whose checksum has not changed since their last
	// analysis; otherwise only their issues are checked again.
	Force bool
}

// Analyse starts an analysis operation and returns its ID (decision 7).
func (s *Service) Analyse(ctx context.Context, req AnalyseRequest) (uint64, error) {
	if err := s.toolsAvailable(); err != nil {
		return 0, err
	}
	if len(req.TrackIDs) > 0 {
		if _, err := s.existing(ctx, req.TrackIDs); err != nil {
			return 0, err
		}
	}
	return s.start(ctx, operations.KindAnalyse, func(ctx context.Context, id uint64) (string, error) {
		return s.runAnalyse(ctx, id, req)
	})
}

// existing returns the tracks of ids, or a field error naming those that
// do not exist.
func (s *Service) existing(ctx context.Context, ids []uint64) ([]Track, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	ts, err := s.store.Tracks(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read tracks: %w", err)
	}
	if len(ts) == len(ids) {
		return ts, nil
	}
	var missing []uint64
	for _, id := range ids {
		if !slices.ContainsFunc(ts, func(t Track) bool { return t.ID == id }) {
			missing = append(missing, id)
		}
	}
	return nil, validation.Field("track_ids", validation.UnknownID, "no track has the IDs %v", missing)
}

func (s *Service) runAnalyse(ctx context.Context, id uint64, req AnalyseRequest) (string, error) {
	set, err := s.readSettings(ctx)
	if err != nil {
		return "", err
	}
	var ts []Track
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	if len(req.TrackIDs) > 0 {
		ts, err = s.store.Tracks(rctx, req.TrackIDs)
	} else {
		ts, err = s.store.Unanalysed(rctx, maxAnalyseAll)
	}
	cancel()
	if err != nil {
		return "", fmt.Errorf("read tracks: %w", err)
	}
	t := newTally()
	total := newTrackTotal(len(ts))
	err = workerpool.ForEach(ctx, analyseWorkers, ts, func(ctx context.Context, tr Track) error {
		res := s.analyseTrack(ctx, tr, set.Standard, req.Force, t)
		done := s.publishAnalysed(id, total, t, tr, res)
		s.ops.Progress(id, operations.KindAnalyse, done, len(ts), tr.FilePath)
		return ctx.Err()
	})
	if err != nil {
		return "", fmt.Errorf("analysis stopped after %d of %d tracks: %w", t.done, len(ts), err)
	}
	return t.summary(outcomeAnalysed, outcomeRechecked, outcomeMissing, outcomeDeleted, outcomeUnreadable, outcomeFailed), nil
}

// analysis is the result of analyseTrack. Issues are set only when the
// analysis was stored, that is for the outcomes analysed and rechecked.
type analysis struct {
	outcome string
	// issues are the codes of the quality issues stored by the analysis.
	issues []string
}

// trackTotal is the number of tracks an operation reports in its
// track.analysed events. An import raises it as it registers tracks; an
// analysis fixes it at the tracks it started with. Its lock also covers the
// counting and the publishing of those events, so Done and Total reach the
// wire in order and the last event published carries the last total.
type trackTotal struct {
	mu sync.Mutex
	n  int
}

func newTrackTotal(n int) *trackTotal { return &trackTotal{n: n} }

// register counts one more track of the operation. It runs before the
// track is handed to the analysis, so the total is never below the number
// of tracks analysed.
func (c *trackTotal) register() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

// publishAnalysed counts the track as processed by operation opID, whatever
// its outcome, and sends its track.analysed event. The count and the event
// are made under the lock of total, so Done rises by one on the wire with
// every event. It returns the number of tracks processed, this one included.
func (s *Service) publishAnalysed(opID uint64, total *trackTotal, t *tally, tr Track, res analysis) int {
	total.mu.Lock()
	defer total.mu.Unlock()
	done := t.add(res.outcome, tr.FilePath, res.outcome == outcomeFailed)
	issues := res.issues
	if issues == nil {
		issues = []string{}
	}
	s.ops.Publish(ws.TrackAnalysedEvent{
		Type: ws.EventTrackAnalysed, Time: s.now().UTC(),
		Data: ws.TrackAnalysedData{
			TrackID: tr.ID, QualityIssues: issues, Outcome: res.outcome,
			OperationID: opID, Done: done, Total: total.n,
		},
	})
	return done
}

// analyseTrack analyses the file of one track and stores the result; a
// reset cue is recorded in t. The result tells the outcome and, when the
// analysis was stored, what it stored.
func (s *Service) analyseTrack(ctx context.Context, tr Track, std Standard, force bool, t *tally) analysis {
	sum := tr.Hashed
	if sum == "" {
		var err error
		sum, err = s.files.Checksum(ctx, tr.FilePath)
		switch {
		case errors.Is(err, media.ErrFileNotFound), errors.Is(err, media.ErrInvalidPath):
			dctx, cancel := context.WithTimeout(ctx, dbTimeout)
			defer cancel()
			if merr := s.store.MarkMissing(dctx, tr.ID, s.now().UTC()); merr != nil {
				if errors.Is(merr, library.ErrTrackNotFound) {
					return analysis{outcome: s.trackDeleted(ctx, tr)}
				}
				return analysis{outcome: s.trackFailed(ctx, tr, "mark file missing", merr)}
			}
			s.log.WarnContext(ctx, "analysis: track file missing", "track_id", tr.ID, "file_path", tr.FilePath)
			return analysis{outcome: outcomeMissing}
		case err != nil:
			return analysis{outcome: s.trackFailed(ctx, tr, "read file", err)}
		}
	}

	unchanged := tr.Checksum != nil && *tr.Checksum == sum
	a := Analysis{FilePath: tr.FilePath, Checksum: sum, AnalyzedAt: s.now().UTC()}
	outcome := outcomeAnalysed
	if unchanged && tr.AnalyzedAt != nil && !force {
		// Same file: new thresholds apply without decoding it again.
		a.Measurements = tr.Measurements
		outcome = outcomeRechecked
	} else {
		info, loud, aerr := s.analyse(ctx, tr.FilePath)
		if aerr != nil {
			if ctx.Err() != nil {
				return analysis{outcome: outcomeFailed}
			}
			s.log.WarnContext(ctx, "analysis: file not readable as audio", "track_id", tr.ID, "file_path", tr.FilePath, logging.KeyError, aerr)
			return analysis{outcome: outcomeUnreadable}
		}
		// A transcoded file keeps the bitrate of its source; a file
		// replaced on disk is an original again.
		var source *int
		if unchanged {
			source = tr.Measurements.SourceBitrate
		}
		a.Measurements = measurementsOf(info, loud, source)
	}
	a.Issues = Check(a.Measurements, std)

	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	reset, err := s.store.SaveAnalysis(dctx, tr.ID, a)
	if err != nil {
		if errors.Is(err, library.ErrTrackNotFound) {
			return analysis{outcome: s.trackDeleted(ctx, tr)}
		}
		return analysis{outcome: s.trackFailed(ctx, tr, "save analysis", err)}
	}
	s.cueReset(ctx, tr, reset, t)
	return analysis{outcome: outcome, issues: issueCodes(a.Issues)}
}

// issueCodes returns the codes of the quality issues.
func issueCodes(issues []library.QualityIssue) []string {
	out := make([]string, len(issues))
	for i, c := range issues {
		out[i] = string(c)
	}
	return out
}

// cueReset logs and records a track whose cue did not fit its new file.
func (s *Service) cueReset(ctx context.Context, tr Track, reset bool, t *tally) {
	if !reset {
		return
	}
	s.log.WarnContext(ctx, "the cue of the track did not fit its new file and was reset", "track_id", tr.ID, "file_path", tr.FilePath)
	t.resetCue(tr.FilePath)
}

// trackDeleted counts a track deleted from the library while the
// operation ran; that is the manager's choice, not a failure.
func (s *Service) trackDeleted(ctx context.Context, tr Track) string {
	s.log.InfoContext(ctx, "track deleted during the operation; skipped", "track_id", tr.ID, "file_path", tr.FilePath)
	return outcomeDeleted
}

func (s *Service) trackFailed(ctx context.Context, tr Track, step string, err error) string {
	if ctx.Err() == nil {
		s.log.ErrorContext(ctx, step+" failed", "track_id", tr.ID, "file_path", tr.FilePath, logging.KeyError, err)
	}
	return outcomeFailed
}
