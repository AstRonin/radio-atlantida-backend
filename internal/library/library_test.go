package library

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// fakeTx runs fn without a database; the fake stores ignore the Tx.
type fakeTx struct{}

func (fakeTx) WithTx(ctx context.Context, fn func(context.Context, db.Tx) error) error {
	return fn(ctx, db.Tx{})
}

type fakeTracks struct {
	TrackStore // unused methods panic
	created    TrackInput
	deleteErr  error
	users      []Ref
}

func (f *fakeTracks) WithTx(db.Tx) TrackStore { return f }

func (f *fakeTracks) Create(_ context.Context, in TrackInput) (uint64, error) {
	f.created = in
	return 1, nil
}

func (f *fakeTracks) SetPlaylists(context.Context, uint64, []uint64) error { return nil }

func (f *fakeTracks) Get(context.Context, uint64) (Track, error) { return Track{ID: 1}, nil }

func (f *fakeTracks) Delete(context.Context, uint64) error { return f.deleteErr }

func (f *fakeTracks) Users(context.Context, uint64, int) ([]Ref, error) { return f.users, nil }

type fakeProber struct {
	info media.Info
	err  error
}

func (f fakeProber) Probe(context.Context, string) (media.Info, error) { return f.info, f.err }

// fakeOnAir names the track on air; zero is none.
type fakeOnAir uint64

func (f fakeOnAir) OnAirTrack() (uint64, bool) { return uint64(f), f != 0 }

func newTrackService(tracks *fakeTracks, prober Prober, logs *bytes.Buffer) *Service {
	return NewService(fakeTx{}, nil, nil, nil, tracks, nil, prober, fakeOnAir(0), slog.New(slog.NewJSONHandler(logs, nil)))
}

func newTrackServiceOn(tracks TrackStore, logs *bytes.Buffer) *Service {
	return NewService(fakeTx{}, nil, nil, nil, tracks, nil, fakeProber{}, fakeOnAir(0), slog.New(slog.NewJSONHandler(logs, nil)))
}

func TestCreateTrackFillsMetadataFromFile(t *testing.T) {
	t.Parallel()
	probed := media.Info{DurationMS: new(181000), Format: new(media.FormatMP3), Bitrate: new(128000), SampleRate: new(44100)}

	tests := []struct {
		name   string
		prober fakeProber
		in     TrackInput
		want   TrackInput
		warns  bool
	}{
		{
			name:   "empty fields come from the file",
			prober: fakeProber{info: probed},
			in:     TrackInput{Title: " Song ", FilePath: "a.mp3", TypeID: 1},
			want: TrackInput{
				Title: "Song", FilePath: "a.mp3", TypeID: 1,
				DurationMS: probed.DurationMS, Format: probed.Format, Bitrate: probed.Bitrate, SampleRate: probed.SampleRate,
			},
		},
		{
			name:   "entered values win",
			prober: fakeProber{info: probed},
			in:     TrackInput{Title: "Song", FilePath: "a.mp3", TypeID: 1, DurationMS: new(5000)},
			want: TrackInput{
				Title: "Song", FilePath: "a.mp3", TypeID: 1,
				DurationMS: new(5000), Format: probed.Format, Bitrate: probed.Bitrate, SampleRate: probed.SampleRate,
			},
		},
		{
			name:   "no file yet",
			prober: fakeProber{err: media.ErrFileNotFound},
			in:     TrackInput{Title: "Song", FilePath: "a.mp3", TypeID: 1},
			want:   TrackInput{Title: "Song", FilePath: "a.mp3", TypeID: 1},
		},
		{
			name:   "no ffprobe",
			prober: fakeProber{err: media.ErrProbeUnavailable},
			in:     TrackInput{Title: "Song", FilePath: "a.mp3", TypeID: 1},
			want:   TrackInput{Title: "Song", FilePath: "a.mp3", TypeID: 1},
		},
		{
			name:   "unreadable file is registered with a warning",
			prober: fakeProber{err: errors.New("ffprobe a.mp3: exit status 1: invalid data")},
			in:     TrackInput{Title: "Song", FilePath: "a.mp3", TypeID: 1},
			want:   TrackInput{Title: "Song", FilePath: "a.mp3", TypeID: 1},
			warns:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracks := &fakeTracks{}
			var logs bytes.Buffer
			_, err := newTrackService(tracks, tt.prober, &logs).CreateTrack(t.Context(), tt.in)
			require.NoError(t, err)
			require.Equal(t, tt.want, tracks.created)
			require.Equal(t, tt.warns, bytes.Contains(logs.Bytes(), []byte(`"level":"WARN"`)), logs.String())
		})
	}
}

func TestCreateTrackRefusesBadInput(t *testing.T) {
	t.Parallel()
	tracks := &fakeTracks{}
	_, err := newTrackService(tracks, fakeProber{}, &bytes.Buffer{}).
		CreateTrack(t.Context(), TrackInput{Title: " ", FilePath: "../a.mp3", TypeID: 1})
	var ve *validation.Error
	require.ErrorAs(t, err, &ve)
	require.Len(t, ve.Fields, 2)
	require.Equal(t, "title", ve.Fields[0].Field)
	require.Equal(t, "file_path", ve.Fields[1].Field)
	require.Equal(t, validation.Format, ve.Fields[1].Rule)
	require.Equal(t, TrackInput{}, tracks.created, "nothing written")
}

func TestDeleteTrackRefusals(t *testing.T) {
	t.Parallel()
	pool := []Ref{{Kind: KindClock, ID: 3, Name: "Morning"}}
	tests := []struct {
		name      string
		deleteErr error
		users     []Ref
		want      error
		usedBy    []Ref
	}{
		{"deleted", nil, nil, nil, nil},
		{"in a filler pool", ErrInUse, pool, ErrTrackInUse, pool},
		{"history only", ErrTrackHasHistory, nil, ErrTrackHasHistory, nil},
		// The database reports one foreign key; the pool is what the
		// manager can fix, so it is named.
		{"history and pool", ErrTrackHasHistory, pool, ErrTrackInUse, pool},
		{"other error", errors.New("connection reset"), nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracks := &fakeTracks{deleteErr: tt.deleteErr, users: tt.users}
			err := newTrackService(tracks, fakeProber{}, &bytes.Buffer{}).DeleteTrack(t.Context(), 1)
			switch {
			case tt.deleteErr == nil:
				require.NoError(t, err)
			case tt.want == nil:
				require.ErrorIs(t, err, tt.deleteErr)
			default:
				require.ErrorIs(t, err, tt.want)
			}
			var inUse *InUseError
			if errors.As(err, &inUse) {
				require.Equal(t, tt.usedBy, inUse.UsedBy)
			} else {
				require.Nil(t, tt.usedBy)
			}
		})
	}
}

type fakeTypes struct {
	TypeStore
	t       PlaylistType
	deleted bool
}

func (f *fakeTypes) Get(context.Context, uint64) (PlaylistType, error) { return f.t, nil }

func (f *fakeTypes) Delete(context.Context, uint64) error {
	f.deleted = true
	return nil
}

func TestDeleteSystemTypeIsRefused(t *testing.T) {
	t.Parallel()
	types := &fakeTypes{t: PlaylistType{ID: 1, Code: "music", IsSystem: true}}
	svc := NewService(fakeTx{}, types, nil, nil, nil, nil, nil, fakeOnAir(0), slog.New(slog.DiscardHandler))

	require.ErrorIs(t, svc.DeleteType(t.Context(), 1), ErrSystemType)
	require.False(t, types.deleted)
}
