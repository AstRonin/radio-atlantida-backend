package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
)

// ErrWaveformUnavailable is returned when the track file is missing or
// cannot be decoded.
var ErrWaveformUnavailable = errors.New("the track file has no waveform")

const (
	// WaveformBucketMS is the resolution of a waveform: a five-minute track
	// has 6,000 buckets (12 KB). Finer placement uses the number fields.
	WaveformBucketMS = 50
	// waveformDecodes bounds the ffmpeg runs of waveforms at a time. They
	// run beside a media operation (they only read the file), on the
	// machine that plays out; the cue editor opens one track at a time.
	waveformDecodes = 2
)

// Waveform is the min and max amplitude of every bucket of a track file
// (docs/phase-22-plan.md §3.5).
type Waveform struct {
	DurationMS int
	BucketMS   int
	// Peaks are pairs (min, max) per bucket, -127..127.
	Peaks []int8
	// Checksum is the file checksum the waveform was computed for; empty
	// when the track has none (never analysed), and then it is not stored.
	Checksum string
}

// MaxPeak is the largest absolute amplitude of the waveform, 0 to 127; the
// panel scales its drawing by it, so a quiet file is as readable as a loud
// one.
func (w Waveform) MaxPeak() int {
	peak := 0
	for _, p := range w.Peaks {
		peak = max(peak, abs(int(p)))
	}
	return peak
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// Waveform returns the waveform of a track file: the stored one while its
// checksum is the track's, else it is computed with ffmpeg and stored. At
// most waveformDecodes run at a time; a request waits for a slot until ctx
// ends. library.ErrTrackNotFound, ErrWaveformUnavailable,
// ErrToolsUnavailable.
func (s *Service) Waveform(ctx context.Context, trackID uint64) (Waveform, error) {
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	ts, err := s.store.Tracks(dctx, []uint64{trackID})
	cancel()
	if err != nil {
		return Waveform{}, fmt.Errorf("read track %d: %w", trackID, err)
	}
	if len(ts) == 0 {
		return Waveform{}, library.ErrTrackNotFound
	}
	tr := ts[0]
	sum := ""
	if tr.Checksum != nil {
		sum = *tr.Checksum
		w, ok, serr := s.stored(ctx, trackID)
		if serr != nil || (ok && w.Checksum == sum) {
			return w, serr
		}
	}
	if !s.ff.Available() {
		return Waveform{}, ErrToolsUnavailable
	}

	select {
	case s.decodes <- struct{}{}:
	case <-ctx.Done():
		return Waveform{}, fmt.Errorf("wait for a waveform slot: %w", ctx.Err())
	}
	peaks, duration, err := s.ff.Peaks(ctx, tr.FilePath, WaveformBucketMS)
	<-s.decodes
	switch {
	case errors.Is(err, media.ErrFileNotFound), errors.Is(err, media.ErrInvalidPath):
		return Waveform{}, fmt.Errorf("track %d: %w: file missing", trackID, ErrWaveformUnavailable)
	case err != nil && ctx.Err() != nil:
		return Waveform{}, fmt.Errorf("waveform of track %d: %w", trackID, ctx.Err())
	case err != nil:
		s.log.WarnContext(ctx, "waveform: file not readable as audio", "track_id", trackID, "file_path", tr.FilePath, logging.KeyError, err)
		return Waveform{}, fmt.Errorf("track %d: %w: %w", trackID, ErrWaveformUnavailable, err)
	}
	w := Waveform{DurationMS: duration, BucketMS: WaveformBucketMS, Peaks: peaks, Checksum: sum}
	if sum == "" {
		return w, nil
	}
	dctx, cancel = context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	switch err := s.store.SaveWaveform(dctx, trackID, w); {
	case errors.Is(err, library.ErrTrackNotFound):
		return Waveform{}, err
	case err != nil:
		// The waveform is good; the next request computes it again.
		s.log.ErrorContext(ctx, "store waveform failed", "track_id", trackID, logging.KeyError, err)
	}
	return w, nil
}

// stored returns the stored waveform of a track; false when there is none.
func (s *Service) stored(ctx context.Context, trackID uint64) (Waveform, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	w, ok, err := s.store.Waveform(ctx, trackID)
	if err != nil {
		return Waveform{}, false, fmt.Errorf("read waveform of track %d: %w", trackID, err)
	}
	return w, ok, nil
}
