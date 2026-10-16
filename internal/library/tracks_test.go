package library

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// fakeBulkTracks keeps tracks and what refers to them.
type fakeBulkTracks struct {
	TrackStore // unused methods panic
	// tracks are the existing tracks; a track with blockers is kept.
	tracks map[uint64]TrackBlockers
	// deleteErrs fail the delete statements in turn.
	deleteErrs  []error
	blockersErr error
	deleted     []uint64
	calls       int
	// ctxErrs records the context error of every delete statement.
	ctxErrs []error
}

func (f *fakeBulkTracks) DeleteUnreferenced(ctx context.Context, ids []uint64) ([]uint64, error) {
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	f.calls++
	if len(f.deleteErrs) > 0 {
		err := f.deleteErrs[0]
		f.deleteErrs = f.deleteErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	var out []uint64
	// Unordered like RETURNING.
	for _, id := range slices.Backward(ids) {
		b, ok := f.tracks[id]
		if ok && !b.History && len(b.UsedBy) == 0 {
			delete(f.tracks, id)
			out = append(out, id)
		}
	}
	f.deleted = append(f.deleted, out...)
	return out, nil
}

func (f *fakeBulkTracks) Blockers(_ context.Context, ids []uint64, _ int) (map[uint64]TrackBlockers, error) {
	if f.blockersErr != nil {
		return nil, f.blockersErr
	}
	out := map[uint64]TrackBlockers{}
	for _, id := range ids {
		if b, ok := f.tracks[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

func TestDeleteTracksReportsEachTrack(t *testing.T) {
	t.Parallel()
	pool := []Ref{{Kind: KindClock, ID: 3, Name: "Morning"}}
	tracks := &fakeBulkTracks{tracks: map[uint64]TrackBlockers{
		1: {Title: "One"},
		2: {Title: "Two", UsedBy: pool},
		3: {Title: "Three", History: true},
		5: {Title: "Five"},
		// History and pool: the pool is named, as by DeleteTrack.
		6: {Title: "Six", History: true, UsedBy: pool},
	}}
	logs := &bytes.Buffer{}
	res, err := newTrackServiceOn(tracks, logs).DeleteTracks(t.Context(), []uint64{5, 2, 3, 4, 6, 1})
	require.NoError(t, err)
	require.Equal(t, []uint64{5, 1}, res.Deleted, "request order")
	require.Equal(t, []TrackRefusal{
		{TrackID: 2, Title: "Two", Err: ErrTrackInUse, UsedBy: pool},
		{TrackID: 3, Title: "Three", Err: ErrTrackHasHistory},
		{TrackID: 4, Err: ErrTrackNotFound},
		{TrackID: 6, Title: "Six", Err: ErrTrackInUse, UsedBy: pool},
	}, res.Refused)
	require.Equal(t, 1, tracks.calls, "one delete statement")
	require.Contains(t, logs.String(), `"msg":"tracks deleted"`)
	require.Contains(t, logs.String(), `"requested":6,"deleted":2,"refused":4`)
}

func TestDeleteTracksAllDeleted(t *testing.T) {
	t.Parallel()
	tracks := &fakeBulkTracks{tracks: map[uint64]TrackBlockers{1: {}, 2: {}, 3: {}}}
	res, err := newTrackServiceOn(tracks, &bytes.Buffer{}).DeleteTracks(t.Context(), []uint64{1, 2, 3})
	require.NoError(t, err)
	require.Equal(t, []uint64{1, 2, 3}, res.Deleted)
	require.NotNil(t, res.Refused)
	require.Empty(t, res.Refused)
}

func TestDeleteTracksOutlivesTheCaller(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tracks := &fakeBulkTracks{tracks: map[uint64]TrackBlockers{1: {}, 2: {}}}
	res, err := newTrackServiceOn(tracks, &bytes.Buffer{}).DeleteTracks(ctx, []uint64{1, 2})
	require.NoError(t, err)
	require.Equal(t, []uint64{1, 2}, res.Deleted)
	require.Equal(t, []error{nil}, tracks.ctxErrs, "the delete ran on a live context")
}

// A reference added while the statement runs fails it; the next attempt
// keeps the newly referenced track.
func TestDeleteTracksRetriesAfterNewReference(t *testing.T) {
	t.Parallel()
	tracks := &fakeBulkTracks{
		tracks:     map[uint64]TrackBlockers{1: {}, 2: {Title: "Two", History: true}},
		deleteErrs: []error{ErrTrackHasHistory},
	}
	res, err := newTrackServiceOn(tracks, &bytes.Buffer{}).DeleteTracks(t.Context(), []uint64{1, 2})
	require.NoError(t, err)
	require.Equal(t, 2, tracks.calls)
	require.Equal(t, []uint64{1}, res.Deleted)
	require.Equal(t, []TrackRefusal{{TrackID: 2, Title: "Two", Err: ErrTrackHasHistory}}, res.Refused)
}

func TestDeleteTracksErrors(t *testing.T) {
	t.Parallel()
	lost := errors.New("connection reset")
	tests := []struct {
		name        string
		deleteErrs  []error
		blockersErr error
		calls       int
		deleted     int
		cause       error
	}{
		{"delete fails", []error{lost}, nil, 1, 0, lost},
		{"new references every time", []error{ErrInUse, ErrInUse, ErrInUse}, nil, deleteAttempts, 0, ErrInUse},
		{"blockers fail", nil, lost, 1, 1, lost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracks := &fakeBulkTracks{
				tracks:     map[uint64]TrackBlockers{1: {}, 2: {History: true}},
				deleteErrs: tt.deleteErrs, blockersErr: tt.blockersErr,
			}
			_, err := newTrackServiceOn(tracks, &bytes.Buffer{}).DeleteTracks(t.Context(), []uint64{1, 2})
			var stopped *DeleteTracksError
			require.ErrorAs(t, err, &stopped)
			require.Equal(t, tt.deleted, stopped.Deleted)
			require.Equal(t, 2, stopped.Requested)
			require.ErrorIs(t, err, tt.cause)
			require.Equal(t, tt.calls, tracks.calls)
		})
	}
}

func TestDeleteTracksLimits(t *testing.T) {
	t.Parallel()
	tooMany := make([]uint64, MaxDeleteTracks+1)
	for i := range tooMany {
		tooMany[i] = uint64(i + 1)
	}
	tests := []struct {
		name string
		ids  []uint64
		rule validation.Rule
	}{
		{"empty", nil, validation.Range},
		{"too many", tooMany, validation.Range},
		{"duplicates", []uint64{1, 2, 1}, validation.Invalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracks := &fakeBulkTracks{}
			_, err := newTrackServiceOn(tracks, &bytes.Buffer{}).DeleteTracks(t.Context(), tt.ids)
			var verr *validation.Error
			require.ErrorAs(t, err, &verr)
			require.Len(t, verr.Fields, 1)
			require.Equal(t, "track_ids", verr.Fields[0].Field)
			require.Equal(t, tt.rule, verr.Fields[0].Rule)
			require.Zero(t, tracks.calls, "nothing deleted")
		})
	}
}
