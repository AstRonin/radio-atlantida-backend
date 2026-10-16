package library

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

func TestCueCheck(t *testing.T) {
	t.Parallel()
	const duration = 60_000
	tests := []struct {
		name  string
		cue   Cue
		field string
		rule  validation.Rule
	}{
		{name: "whole file", cue: Cue{}},
		{name: "null cue out, cue in", cue: Cue{InMS: 10_000}},
		{name: "exactly 5000 ms on air", cue: Cue{InMS: 10_000, OutMS: new(15_000)}},
		{name: "cue out at the end", cue: Cue{OutMS: new(duration)}},
		{name: "fades fill the air", cue: Cue{InMS: 50_000, FadeInMS: 5000, FadeOutMS: 5000}},
		{name: "negative cue in", cue: Cue{InMS: -1}, field: "cue_in_ms", rule: validation.Range},
		{name: "cue in at the end", cue: Cue{InMS: duration}, field: "cue_in_ms", rule: validation.CueBeyondEnd},
		{name: "cue out before cue in", cue: Cue{InMS: 20_000, OutMS: new(20_000)}, field: "cue_out_ms", rule: validation.CueOrder},
		{name: "cue out past the end", cue: Cue{OutMS: new(duration + 1)}, field: "cue_out_ms", rule: validation.CueBeyondEnd},
		{name: "too short, cut at the end", cue: Cue{InMS: 10_000, OutMS: new(14_999)}, field: "cue_out_ms", rule: validation.AirTooShort},
		{name: "too short, cut at the start", cue: Cue{InMS: 55_001}, field: "cue_in_ms", rule: validation.AirTooShort},
		{name: "fade out of range", cue: Cue{FadeOutMS: 5001}, field: "fade_out_ms", rule: validation.Range},
		{name: "fade in negative", cue: Cue{FadeInMS: -1}, field: "fade_in_ms", rule: validation.Range},
		{name: "fades longer than the air", cue: Cue{InMS: 52_000, FadeInMS: 4000, FadeOutMS: 4001}, field: "fade_out_ms", rule: validation.FadesTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.cue.Check(duration)
			if tt.field == "" {
				require.NoError(t, err)
				return
			}
			var verr *validation.Error
			require.ErrorAs(t, err, &verr)
			require.Len(t, verr.Fields, 1)
			require.Equal(t, tt.field, verr.Fields[0].Field)
			require.Equal(t, tt.rule, verr.Fields[0].Rule)
		})
	}

	// A jingle shorter than 5 s plays whole: the minimum is for cuts.
	require.NoError(t, Cue{}.Check(3000))
	require.NoError(t, Cue{FadeOutMS: 1000}.Check(3000))
}

func TestAirDurationMS(t *testing.T) {
	t.Parallel()
	require.Nil(t, Track{}.AirDurationMS(), "unknown without a duration")
	tr := Track{DurationMS: new(60_000)}
	require.Equal(t, 60_000, *tr.AirDurationMS())
	tr.Cue = Cue{InMS: 5000, OutMS: new(45_000)}
	require.Equal(t, 40_000, *tr.AirDurationMS())
	tr.Cue.OutMS = nil
	require.Equal(t, 55_000, *tr.AirDurationMS())
}

// cueTracks is the store of SetCue.
type cueTracks struct {
	TrackStore // unused methods panic
	state      CueState
	stateErr   error
	saved      *Cue
	hours      *HourRange
	askedHours bool
}

func (f *cueTracks) WithTx(db.Tx) TrackStore { return f }

func (f *cueTracks) CueState(context.Context, uint64) (CueState, error) { return f.state, f.stateErr }

func (f *cueTracks) SetCue(_ context.Context, _ uint64, c Cue) error {
	f.saved = &c
	return nil
}

func (f *cueTracks) PlannedHours(context.Context, uint64) (*HourRange, error) {
	f.askedHours = true
	return f.hours, nil
}

func (f *cueTracks) Get(_ context.Context, id uint64) (Track, error) {
	t := Track{ID: id, DurationMS: f.state.DurationMS}
	if f.saved != nil {
		t.Cue = *f.saved
	}
	return t, nil
}

func newCueService(tracks *cueTracks, onAir fakeOnAir) *Service {
	var logs bytes.Buffer
	svc := newTrackServiceOn(tracks, &logs)
	svc.onAir = onAir
	return svc
}

func TestSetCue(t *testing.T) {
	t.Parallel()
	hours := &HourRange{From: time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC)}
	cut := Cue{InMS: 5000, OutMS: new(45_000), FadeInMS: 1000, FadeOutMS: 2000}

	t.Run("a new cut names the hours to rebuild", func(t *testing.T) {
		t.Parallel()
		f := &cueTracks{state: CueState{DurationMS: new(60_000)}, hours: hours}
		res, err := newCueService(f, 0).SetCue(t.Context(), 1, cut)
		require.NoError(t, err)
		require.Equal(t, cut, *f.saved)
		require.Equal(t, hours, res.Rebuild)
		require.Equal(t, cut, res.Track.Cue)
	})
	t.Run("fades only need no rebuild", func(t *testing.T) {
		t.Parallel()
		f := &cueTracks{state: CueState{DurationMS: new(60_000), Cue: Cue{InMS: 5000, OutMS: new(45_000)}}, hours: hours}
		res, err := newCueService(f, 0).SetCue(t.Context(), 1, cut)
		require.NoError(t, err)
		require.NotNil(t, f.saved)
		require.False(t, f.askedHours)
		require.Nil(t, res.Rebuild)
	})
	t.Run("not planned", func(t *testing.T) {
		t.Parallel()
		f := &cueTracks{state: CueState{DurationMS: new(60_000)}}
		res, err := newCueService(f, 0).SetCue(t.Context(), 1, cut)
		require.NoError(t, err)
		require.Nil(t, res.Rebuild)
	})
	t.Run("queued", func(t *testing.T) {
		t.Parallel()
		f := &cueTracks{state: CueState{DurationMS: new(60_000), Queued: true}}
		_, err := newCueService(f, 0).SetCue(t.Context(), 1, cut)
		require.ErrorIs(t, err, ErrTrackOnAir)
		require.Nil(t, f.saved)
	})
	t.Run("on air", func(t *testing.T) {
		t.Parallel()
		f := &cueTracks{state: CueState{DurationMS: new(60_000)}}
		_, err := newCueService(f, 1).SetCue(t.Context(), 1, cut)
		require.ErrorIs(t, err, ErrTrackOnAir)
		_, err = newCueService(f, 2).SetCue(t.Context(), 1, cut)
		require.NoError(t, err, "another track on air")
	})
	t.Run("without a duration", func(t *testing.T) {
		t.Parallel()
		f := &cueTracks{}
		_, err := newCueService(f, 0).SetCue(t.Context(), 1, cut)
		require.ErrorIs(t, err, ErrTrackNotAnalysed)
	})
	t.Run("unknown track", func(t *testing.T) {
		t.Parallel()
		f := &cueTracks{stateErr: ErrTrackNotFound}
		_, err := newCueService(f, 0).SetCue(t.Context(), 1, cut)
		require.ErrorIs(t, err, ErrTrackNotFound)
	})
	t.Run("a cue that does not fit", func(t *testing.T) {
		t.Parallel()
		f := &cueTracks{state: CueState{DurationMS: new(30_000)}}
		_, err := newCueService(f, 0).SetCue(t.Context(), 1, cut)
		var verr *validation.Error
		require.ErrorAs(t, err, &verr)
		require.Equal(t, "cue_out_ms", verr.Fields[0].Field)
		require.Nil(t, f.saved)
	})
}

func TestTracksMarkTheTrackOnAir(t *testing.T) {
	t.Parallel()
	f := &cueTracks{}
	tr, err := newCueService(f, 1).GetTrack(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, PlayoutOnAir, tr.PlayoutState)
	tr, err = newCueService(f, 2).GetTrack(t.Context(), 1)
	require.NoError(t, err)
	require.Empty(t, tr.PlayoutState)
}
