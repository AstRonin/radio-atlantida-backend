package library

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

// usesTracks answers the three lookups of FileInUse; the rest of TrackStore
// panics if called.
type usesTracks struct {
	TrackStore // unused methods panic
	users      []Ref
	planned    *HourRange
	missed     *HourRange
}

func (f *usesTracks) WithTx(db.Tx) TrackStore { return f }

func (f *usesTracks) Users(context.Context, uint64, int) ([]Ref, error) { return f.users, nil }

func (f *usesTracks) PlannedHours(context.Context, uint64) (*HourRange, error) {
	return f.planned, nil
}

func (f *usesTracks) MissedHours(context.Context, uint64) (*HourRange, error) {
	return f.missed, nil
}

// TrackInUse is true exactly when FileInUse refuses the file change.
func TestTrackInUseMatchesFileInUse(t *testing.T) {
	t.Parallel()
	hours := &HourRange{From: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)}
	missed := &HourRange{From: time.Date(2026, 10, 27, 6, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 27, 7, 0, 0, 0, time.UTC)}
	clock := []Ref{{Kind: KindClock, ID: 3, Name: "Morning"}}
	tests := []struct {
		name    string
		users   []Ref
		planned *HourRange
		missed  *HourRange
		want    bool
	}{
		{"unused", nil, nil, nil, false},
		{"clock insert", clock, nil, nil, true},
		{"plan item", nil, hours, nil, true},
		{"clock insert and plan item", clock, hours, nil, true},
		{"open missed item", nil, nil, missed, true},
		{"open missed item and clock insert", clock, nil, missed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := newTrackServiceOn(&usesTracks{users: tt.users, planned: tt.planned, missed: tt.missed}, &bytes.Buffer{})
			got, err := svc.TrackInUse(t.Context(), 1)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)

			refused := svc.FileInUse(t.Context(), 1)
			require.Equal(t, tt.want, refused != nil, "replace refuses the same tracks")
			if refused != nil {
				require.ErrorIs(t, refused, ErrTrackInUse)
			}
		})
	}
}

// A failed lookup is an error, not a false answer.
func TestTrackInUseReportsLookupErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	tracks := &failingUses{err: boom}
	_, err := newTrackServiceOn(tracks, &bytes.Buffer{}).TrackInUse(t.Context(), 1)
	require.ErrorIs(t, err, boom)

	missedFails := &failingMissed{usesTracks: &usesTracks{}, err: boom}
	_, err = newTrackServiceOn(missedFails, &bytes.Buffer{}).TrackInUse(t.Context(), 1)
	require.ErrorIs(t, err, boom)
	require.ErrorIs(t, newTrackServiceOn(missedFails, &bytes.Buffer{}).FileInUse(t.Context(), 1), boom)
}

type failingUses struct {
	TrackStore
	err error
}

func (f *failingUses) WithTx(db.Tx) TrackStore { return f }

func (f *failingUses) Users(context.Context, uint64, int) ([]Ref, error) { return nil, f.err }

// failingMissed fails only the missed lookup.
type failingMissed struct {
	*usesTracks
	err error
}

func (f *failingMissed) WithTx(db.Tx) TrackStore { return f }

func (f *failingMissed) MissedHours(context.Context, uint64) (*HourRange, error) { return nil, f.err }
