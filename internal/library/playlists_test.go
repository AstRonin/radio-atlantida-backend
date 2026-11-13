package library

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeEmergency is the setting playout.emergency_playlist_id.
type fakeEmergency struct {
	id  *uint64
	err error
}

func (f fakeEmergency) EmergencyPlaylistID(context.Context) (*uint64, error) { return f.id, f.err }

type fakePlaylists struct {
	PlaylistStore // unused methods panic

	names     map[uint64]string
	users     []Ref
	usersErr  error
	deleteErr error
	deleted   []uint64
}

func (f *fakePlaylists) Get(_ context.Context, id uint64) (Playlist, error) {
	n, ok := f.names[id]
	if !ok {
		return Playlist{}, ErrPlaylistNotFound
	}
	return Playlist{ID: id, Name: n}, nil
}

func (f *fakePlaylists) Users(context.Context, uint64, int) ([]Ref, error) {
	return f.users, f.usersErr
}

func (f *fakePlaylists) Delete(_ context.Context, id uint64) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func TestDeletePlaylistEmergency(t *testing.T) {
	t.Parallel()
	const id = uint64(7)
	boom := errors.New("boom")
	clock := Ref{Kind: KindClock, ID: 2, Name: "Morning"}
	named := Ref{Kind: KindEmergencyPlaylist, ID: id, Name: "Reserve"}
	tests := []struct {
		name      string
		emergency fakeEmergency
		store     fakePlaylists
		usedBy    []Ref // a refusal naming these
		wantErr   error // otherwise this error, nil for a delete
		deleted   []uint64
	}{
		{
			name:      "the emergency playlist is refused and names itself",
			emergency: fakeEmergency{id: new(id)},
			usedBy:    []Ref{named},
		},
		{
			name:      "another playlist is deleted",
			emergency: fakeEmergency{id: new(uint64(8))},
			deleted:   []uint64{id},
		},
		{
			name:    "no emergency playlist: deleted",
			deleted: []uint64{id},
		},
		{
			name:   "a playlist used by a clock is refused as before",
			store:  fakePlaylists{users: []Ref{clock}, deleteErr: ErrInUse},
			usedBy: []Ref{clock},
		},
		{
			name:      "a clock and the emergency setting: both are named",
			emergency: fakeEmergency{id: new(id)},
			store:     fakePlaylists{users: []Ref{clock}},
			usedBy:    []Ref{named, clock},
		},
		{
			name:      "reading the setting fails: nothing is deleted",
			emergency: fakeEmergency{err: boom},
			wantErr:   boom,
		},
		{
			name:      "listing the clocks of the emergency playlist fails: nothing is deleted",
			emergency: fakeEmergency{id: new(id)},
			store:     fakePlaylists{usersErr: boom},
			wantErr:   boom,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := tt.store
			store.names = map[uint64]string{id: "Reserve"}
			svc := NewService(fakeTx{}, nil, &store, nil, nil, nil, nil, nil, fakeOnAir(0), tt.emergency, slog.New(slog.DiscardHandler))

			err := svc.DeletePlaylist(t.Context(), id)

			require.Equal(t, tt.deleted, store.deleted)
			if tt.usedBy != nil {
				var inUse *InUseError
				require.ErrorAs(t, err, &inUse)
				require.ErrorIs(t, err, ErrPlaylistInUse)
				require.Equal(t, tt.usedBy, inUse.UsedBy)
				return
			}
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				require.NotErrorIs(t, err, ErrPlaylistInUse)
				return
			}
			require.NoError(t, err)
		})
	}
}

// A setting left pointing at a deleted playlist is not a refusal: there is
// nothing to protect, and the panel must learn that the playlist is gone.
func TestDeletePlaylistEmergencySettingDangling(t *testing.T) {
	t.Parallel()
	store := fakePlaylists{}
	svc := NewService(fakeTx{}, nil, &store, nil, nil, nil, nil, nil, fakeOnAir(0),
		fakeEmergency{id: new(uint64(9))}, slog.New(slog.DiscardHandler))

	require.ErrorIs(t, svc.DeletePlaylist(t.Context(), 9), ErrPlaylistNotFound)
	require.Empty(t, store.deleted)
}
