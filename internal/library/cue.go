package library

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Errors of SetCue.
var (
	// ErrTrackOnAir: the track is queued in Liquidsoap or on air, so its
	// cut is not changed now (docs/phase-22-plan.md §2, decision 4).
	ErrTrackOnAir = errors.New("track is queued or on air")
	// ErrTrackNotAnalysed: the track has no duration, so its cue points
	// cannot be checked.
	ErrTrackNotAnalysed = errors.New("track has no duration")
)

const (
	// MinAirMS is the shortest part of a file that cue points may leave on
	// air.
	MinAirMS = 5000
	// MaxFadeMS is the longest fade at a cue point (ck_tracks_fades).
	MaxFadeMS = 5000
)

// Cue is the part of a track file that goes on air and its fades
// (docs/phase-22-plan.md §2). The file is never changed: Liquidsoap plays
// from InMS to OutMS. The zero Cue is the whole file without fades.
type Cue struct {
	// InMS is the start of the on-air part, from the start of the file.
	InMS int
	// OutMS is its end; nil is the end of the file.
	OutMS *int
	// FadeInMS and FadeOutMS are applied at the cue points; 0 is a hard
	// cut.
	FadeInMS  int
	FadeOutMS int
}

// Cuts reports whether c leaves out a part of the file.
func (c Cue) Cuts() bool { return c.InMS > 0 || c.OutMS != nil }

// AirDurationMS is the length of the on-air part of a file of durationMS.
func (c Cue) AirDurationMS(durationMS int) int {
	out := durationMS
	if c.OutMS != nil {
		out = *c.OutMS
	}
	return out - c.InMS
}

// sameCut reports whether c and o leave the same part of the file on air.
func (c Cue) sameCut(o Cue) bool {
	if c.InMS != o.InMS || (c.OutMS == nil) != (o.OutMS == nil) {
		return false
	}
	return c.OutMS == nil || *c.OutMS == *o.OutMS
}

// Check refuses a cue that does not fit a file of durationMS, one error per
// field. A cut must leave at least MinAirMS on air; the whole file may be
// shorter (a jingle). The same rule decides when an analysis resets the cue
// of a new file (internal/repository/ingest, cueInvalid).
func (c Cue) Check(durationMS int) error {
	errs := &validation.Error{}
	switch {
	case c.InMS < 0:
		errs.Add("cue_in_ms", validation.Range, "must not be negative")
	case c.InMS >= durationMS:
		errs.Add("cue_in_ms", validation.CueBeyondEnd, "must be before the end of the file (%d ms)", durationMS)
	}
	if c.OutMS != nil {
		switch {
		case *c.OutMS <= c.InMS:
			errs.Add("cue_out_ms", validation.CueOrder, "must be after cue_in_ms")
		case *c.OutMS > durationMS:
			errs.Add("cue_out_ms", validation.CueBeyondEnd, "must not be past the end of the file (%d ms)", durationMS)
		}
	}
	for _, f := range []struct {
		field string
		ms    int
	}{{"fade_in_ms", c.FadeInMS}, {"fade_out_ms", c.FadeOutMS}} {
		if f.ms < 0 || f.ms > MaxFadeMS {
			errs.Add(f.field, validation.Range, "must be 0 to %d", MaxFadeMS)
		}
	}
	if err := errs.Err(); err != nil {
		return err
	}
	air := c.AirDurationMS(durationMS)
	if c.Cuts() && air < MinAirMS {
		field := "cue_in_ms"
		if c.OutMS != nil {
			field = "cue_out_ms"
		}
		errs.Add(field, validation.AirTooShort, "leaves %d ms on air; at least %d ms", air, MinAirMS)
	}
	if c.FadeInMS+c.FadeOutMS > air {
		errs.Add("fade_out_ms", validation.FadesTooLong, "the fades together (%d ms) are longer than the %d ms on air",
			c.FadeInMS+c.FadeOutMS, air)
	}
	return errs.Err()
}

// CueState is what SetCue reads of a track under its row lock.
type CueState struct {
	// DurationMS is nil until the file is analysed or a duration entered.
	DurationMS *int
	Cue        Cue
	// Queued: a plan item of the track is in a Liquidsoap queue.
	Queued bool
}

// HourRange is a range of built plan hours: From is the start of the
// first, To the end of the last.
type HourRange struct {
	From, To time.Time
}

// OnAir tells which track is on air (*playout.Service).
type OnAir interface {
	// OnAirTrack returns the track on air; false between tracks.
	OnAirTrack() (uint64, bool)
}

// SetCueResult is the outcome of SetCue.
type SetCueResult struct {
	Track Track
	// Rebuild is the range of the plan hours that hold a planned item of
	// the track; set when the cut changed and there are such hours. The
	// caller rebuilds them (§7, a clock_build operation).
	Rebuild *HourRange
}

// SetCue stores the cue points and fades of a track (docs/phase-22-plan.md
// §3.3). It is refused for a track without a duration, and while the track
// is queued in Liquidsoap or on air. The reviewed mark is kept: editing the
// cue is reviewing (decision 6). Plan items keep the cut they were planned
// with; when the cut changed, the result names the hours to rebuild.
func (s *Service) SetCue(ctx context.Context, id uint64, c Cue) (SetCueResult, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var rebuild *HourRange
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		ts := s.tracks.WithTx(tx)
		st, err := ts.CueState(ctx, id)
		if err != nil {
			return err
		}
		if st.DurationMS == nil {
			return ErrTrackNotAnalysed
		}
		if err = c.Check(*st.DurationMS); err != nil {
			return err
		}
		if playing, ok := s.onAir.OnAirTrack(); st.Queued || (ok && playing == id) {
			return ErrTrackOnAir
		}
		if err = ts.SetCue(ctx, id, c); err != nil {
			return err
		}
		if st.Cue.sameCut(c) {
			return nil // fades are read when a request is pushed
		}
		rebuild, err = ts.PlannedHours(ctx, id)
		return err
	})
	if err != nil {
		return SetCueResult{}, err
	}
	t, err := s.tracks.Get(ctx, id)
	if err != nil {
		return SetCueResult{}, fmt.Errorf("read track %d: %w", id, err)
	}
	s.log.InfoContext(ctx, "track cue saved", "track_id", id, "cue_in_ms", c.InMS, "cue_out_ms", c.OutMS,
		"fade_in_ms", c.FadeInMS, "fade_out_ms", c.FadeOutMS, "rebuild", rebuild != nil)
	return SetCueResult{Track: t, Rebuild: rebuild}, nil
}
