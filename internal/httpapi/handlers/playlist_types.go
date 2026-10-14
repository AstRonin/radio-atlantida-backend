package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
)

type playlistTypeOutput struct {
	Body dto.PlaylistType
}

type playlistTypeListOutput struct {
	Body dto.PlaylistTypeList
}

type createPlaylistTypeInput struct {
	Body dto.CreatePlaylistType
}

type renamePlaylistTypeInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.RenamePlaylistType
}

func registerPlaylistTypes(api huma.API, log *slog.Logger, svc Library) {
	tags := []string{"Playlist types"}

	register(api, log, huma.Operation{
		OperationID: "list-playlist-types",
		Method:      http.MethodGet,
		Path:        "/api/playlist-types",
		Summary:     "List playlist types",
		Description: "System and custom types with their usage counts, in name order.",
		Tags:        tags,
		Errors:      commonErrors,
	}, func(ctx context.Context, _ *struct{}) (*playlistTypeListOutput, error) {
		ts, err := svc.ListTypes(ctx)
		if err != nil {
			return nil, err
		}
		out := &playlistTypeListOutput{Body: dto.PlaylistTypeList{Items: make([]dto.PlaylistType, 0, len(ts))}}
		for _, t := range ts {
			out.Body.Items = append(out.Body.Items, playlistTypeDTO(t))
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "get-playlist-type",
		Method:      http.MethodGet,
		Path:        "/api/playlist-types/{id}",
		Summary:     "Get a playlist type",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*playlistTypeOutput, error) {
		t, err := svc.GetType(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &playlistTypeOutput{Body: playlistTypeDTO(t)}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "create-playlist-type",
		Method:        http.MethodPost,
		Path:          "/api/playlist-types",
		Summary:       "Create a playlist type",
		Description:   "Adds a custom type. The code is a unique slug and cannot be changed later. A taken code or name is a field error (taken).",
		Tags:          tags,
		DefaultStatus: http.StatusCreated,
		Errors:        errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *createPlaylistTypeInput) (*playlistTypeOutput, error) {
		t, err := svc.CreateType(ctx, in.Body.Code, in.Body.Name)
		if err != nil {
			return nil, err
		}
		return &playlistTypeOutput{Body: playlistTypeDTO(t)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "rename-playlist-type",
		Method:      http.MethodPut,
		Path:        "/api/playlist-types/{id}",
		Summary:     "Rename a playlist type",
		Description: "Changes the display name; system types can be renamed too. The code does not change.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *renamePlaylistTypeInput) (*playlistTypeOutput, error) {
		t, err := svc.RenameType(ctx, in.ID, in.Body.Name)
		if err != nil {
			return nil, err
		}
		return &playlistTypeOutput{Body: playlistTypeDTO(t)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "delete-playlist-type",
		Method:      http.MethodDelete,
		Path:        "/api/playlist-types/{id}",
		Summary:     "Delete a playlist type",
		Description: "Deletes a custom type. Refused for a system type (system_type_not_deletable) and for a type that playlists or tracks have (playlist_type_in_use, with used_by listing them).",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*struct{}, error) {
		return nil, svc.DeleteType(ctx, in.ID)
	})
}
