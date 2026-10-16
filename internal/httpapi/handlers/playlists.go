package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

type playlistOutput struct {
	Body dto.Playlist
}

type playlistSavedOutput struct {
	Body dto.PlaylistSaved
}

type playlistListOutput struct {
	Body dto.PlaylistList
}

type listPlaylistsInput struct {
	TypeID uint64 `query:"type_id" minimum:"0" doc:"Only playlists of this type."`
}

type createPlaylistInput struct {
	Body dto.PlaylistBody
}

type updatePlaylistInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.PlaylistBody
}

type setPlaylistTracksInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.SetPlaylistTracks
}

type setPlayRulesInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.SetPlayRules
}

type playRuleIDsOutput struct {
	Body dto.PlayRuleIDs
}

func registerPlaylists(api huma.API, log *slog.Logger, svc Library, rules PlayRules, rot Rotation) {
	tags := []string{"Playlists"}

	register(api, log, huma.Operation{
		OperationID: "list-playlists",
		Method:      http.MethodGet,
		Path:        "/api/playlists",
		Summary:     "List playlists",
		Description: "Playlists with their type and track count, in name order.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *listPlaylistsInput) (*playlistListOutput, error) {
		ps, err := svc.ListPlaylists(ctx, in.TypeID)
		if err != nil {
			return nil, err
		}
		out := &playlistListOutput{Body: dto.PlaylistList{Items: make([]dto.Playlist, 0, len(ps))}}
		for _, p := range ps {
			out.Body.Items = append(out.Body.Items, playlistDTO(p))
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "get-playlist",
		Method:      http.MethodGet,
		Path:        "/api/playlists/{id}",
		Summary:     "Get a playlist",
		Description: "The tracks of a playlist are listed by GET /api/tracks?playlist_id=.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*playlistOutput, error) {
		p, err := svc.GetPlaylist(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &playlistOutput{Body: playlistDTO(p)}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "create-playlist",
		Method:        http.MethodPost,
		Path:          "/api/playlists",
		Summary:       "Create a playlist",
		Tags:          tags,
		DefaultStatus: http.StatusCreated,
		Errors:        errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *createPlaylistInput) (*playlistOutput, error) {
		p, err := svc.CreatePlaylist(ctx, playlistInput(in.Body))
		if err != nil {
			return nil, err
		}
		return &playlistOutput{Body: playlistDTO(p)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "update-playlist",
		Method:      http.MethodPut,
		Path:        "/api/playlists/{id}",
		Summary:     "Update a playlist",
		Description: "Renames the playlist, changes its type, or its placement rule in clocks. A changed rule rebuilds the planned hours of the clocks that use the playlist (build_operation_id).",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *updatePlaylistInput) (*playlistSavedOutput, error) {
		r, err := svc.UpdatePlaylist(ctx, in.ID, playlistInput(in.Body))
		if err != nil {
			return nil, err
		}
		out := &playlistSavedOutput{Body: dto.PlaylistSaved{Playlist: playlistDTO(r.Playlist)}}
		if r.RuleChanged {
			out.Body.BuildOperationID = startRebuild(ctx, log, rot)
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "delete-playlist",
		Method:      http.MethodDelete,
		Path:        "/api/playlists/{id}",
		Summary:     "Delete a playlist",
		Description: "Its tracks stay in the library. Refused while a clock uses the playlist as a clock playlist or an insert source (playlist_in_use, with used_by naming the clocks).",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*struct{}, error) {
		return nil, svc.DeletePlaylist(ctx, in.ID)
	})

	register(api, log, huma.Operation{
		OperationID: "set-playlist-tracks",
		Method:      http.MethodPut,
		Path:        "/api/playlists/{id}/tracks",
		Summary:     "Set the tracks of a playlist",
		Description: "Replaces the membership: listed tracks are in the playlist, others are removed from it. A playlist is a pool, so the order is not stored.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *setPlaylistTracksInput) (*playlistOutput, error) {
		p, err := svc.SetPlaylistTracks(ctx, in.ID, in.Body.TrackIDs)
		if err != nil {
			return nil, err
		}
		return &playlistOutput{Body: playlistDTO(p)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "set-playlist-play-rules",
		Method:      http.MethodPut,
		Path:        "/api/playlists/{id}/play-rules",
		Summary:     "Set the play rules of a playlist",
		Description: "The rules apply to every track of the playlist, each track checked on its own.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *setPlayRulesInput) (*playRuleIDsOutput, error) {
		ids, err := rules.SetPlaylistRules(ctx, in.ID, in.Body.RuleIDs)
		if err != nil {
			return nil, err
		}
		return &playRuleIDsOutput{Body: dto.PlayRuleIDs{RuleIDs: emptyIDs(ids)}}, nil
	})
}

func playlistInput(b dto.PlaylistBody) library.PlaylistInput {
	return library.PlaylistInput{Name: b.Name, TypeID: b.TypeID, ClockRule: b.ClockRule, ClockCount: b.ClockCount}
}
