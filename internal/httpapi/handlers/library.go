package handlers

import (
	"context"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// Library manages playlist types, playlists, artists, and tracks
// (*library.Service).
type Library interface {
	ListTypes(ctx context.Context) ([]library.PlaylistType, error)
	GetType(ctx context.Context, id int64) (library.PlaylistType, error)
	CreateType(ctx context.Context, code, name string) (library.PlaylistType, error)
	RenameType(ctx context.Context, id int64, name string) (library.PlaylistType, error)
	DeleteType(ctx context.Context, id int64) error

	ListPlaylists(ctx context.Context, typeID int64) ([]library.Playlist, error)
	GetPlaylist(ctx context.Context, id int64) (library.Playlist, error)
	CreatePlaylist(ctx context.Context, in library.PlaylistInput) (library.Playlist, error)
	UpdatePlaylist(ctx context.Context, id int64, in library.PlaylistInput) (library.Playlist, error)
	DeletePlaylist(ctx context.Context, id int64) error
	SetPlaylistTracks(ctx context.Context, id int64, trackIDs []int64) (library.Playlist, error)

	ListArtists(ctx context.Context, query string) ([]library.Artist, error)
	GetArtist(ctx context.Context, id int64) (library.Artist, error)
	CreateArtist(ctx context.Context, name string) (library.Artist, error)
	RenameArtist(ctx context.Context, id int64, name string) (library.Artist, error)
	DeleteArtist(ctx context.Context, id int64) error

	ListTracks(ctx context.Context, f library.TrackFilter) (library.TrackPage, error)
	GetTrack(ctx context.Context, id int64) (library.Track, error)
	CreateTrack(ctx context.Context, in library.TrackInput) (library.Track, error)
	UpdateTrack(ctx context.Context, id int64, in library.TrackInput) (library.Track, error)
	DeleteTrack(ctx context.Context, id int64) error
	TrackAudio(ctx context.Context, id int64) (library.Audio, error)
}

func typeRefDTO(t library.TypeRef) dto.TypeRef {
	return dto.TypeRef{ID: t.ID, Code: t.Code, Name: t.Name}
}

func playlistTypeDTO(t library.PlaylistType) dto.PlaylistType {
	return dto.PlaylistType{
		ID: t.ID, Code: t.Code, Name: t.Name, IsSystem: t.IsSystem,
		PlaylistCount: t.PlaylistCount, TrackCount: t.TrackCount, CreatedAt: t.CreatedAt.UTC(),
	}
}

func playlistDTO(p library.Playlist) dto.Playlist {
	return dto.Playlist{
		ID: p.ID, Name: p.Name, Type: typeRefDTO(p.Type), TrackCount: p.TrackCount,
		PlayRuleIDs: emptyIDs(p.PlayRuleIDs), CreatedAt: p.CreatedAt.UTC(),
	}
}

func artistDTO(a library.Artist) dto.Artist {
	return dto.Artist{ID: a.ID, Name: a.Name, TrackCount: a.TrackCount}
}

func trackDTO(t library.Track) dto.Track {
	out := dto.Track{
		ID: t.ID, Title: t.Title, DurationMS: t.DurationMS, FilePath: t.FilePath,
		Format: t.Format, Bitrate: t.Bitrate, SampleRate: t.SampleRate,
		QualityOK: t.QualityOK, QualityNotes: t.QualityNotes,
		Type: typeRefDTO(t.Type), FileChecksum: t.FileChecksum,
		PlaylistIDs: emptyIDs(t.PlaylistIDs), PlayRuleIDs: emptyIDs(t.PlayRuleIDs),
		CreatedAt: t.CreatedAt.UTC(),
	}
	if t.Artist != nil {
		out.Artist = &dto.ArtistRef{ID: t.Artist.ID, Name: t.Artist.Name}
	}
	return out
}

func trackInput(b dto.TrackBody) library.TrackInput {
	return library.TrackInput{
		Title: b.Title, ArtistID: b.ArtistID, FilePath: b.FilePath, TypeID: b.TypeID,
		DurationMS: b.DurationMS, Format: b.Format, Bitrate: b.Bitrate, SampleRate: b.SampleRate,
		QualityOK: b.QualityOK, QualityNotes: b.QualityNotes, PlaylistIDs: b.PlaylistIDs,
	}
}
