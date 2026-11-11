package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

type artistOutput struct {
	Body dto.Artist
}

type artistListOutput struct {
	Body dto.ArtistList
}

type listArtistsInput struct {
	Query  string `query:"q" maxLength:"200" doc:"Only artists whose name contains this text (case-insensitive)."`
	Sort   string `query:"sort" enum:"name,track_count" default:"name" doc:"Sort key; ties are ordered by name and ID ascending."`
	Order  string `query:"order" enum:"asc,desc" default:"asc"`
	Offset int    `query:"offset" minimum:"0" maximum:"1000000" default:"0" doc:"Artists to skip in the sorted list."`
	Limit  int    `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

type createArtistInput struct {
	Body dto.ArtistBody
}

type renameArtistInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.ArtistBody
}

func registerArtists(api huma.API, log *slog.Logger, svc Library) {
	tags := []string{"Artists"}

	register(api, log, huma.Operation{
		OperationID: "list-artists",
		Method:      http.MethodGet,
		Path:        "/api/artists",
		Summary:     "List artists",
		Description: "Artists with their track count, sorted by `sort` and `order` (ties by name and ID ascending), a page at a time: read the next page with `offset`. `total` counts every artist matching `q`, on every page. No cursor.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *listArtistsInput) (*artistListOutput, error) {
		page, err := svc.ListArtists(ctx, library.ArtistFilter{
			Query: in.Query, Sort: library.ArtistSort(in.Sort), Desc: in.Order == "desc", Offset: in.Offset, Limit: in.Limit,
		})
		if err != nil {
			return nil, err
		}
		out := &artistListOutput{Body: dto.ArtistList{Items: make([]dto.Artist, 0, len(page.Items)), Total: page.Total}}
		for _, a := range page.Items {
			out.Body.Items = append(out.Body.Items, artistDTO(a))
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "get-artist",
		Method:      http.MethodGet,
		Path:        "/api/artists/{id}",
		Summary:     "Get an artist",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*artistOutput, error) {
		a, err := svc.GetArtist(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &artistOutput{Body: artistDTO(a)}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "create-artist",
		Method:        http.MethodPost,
		Path:          "/api/artists",
		Summary:       "Create an artist",
		Tags:          tags,
		DefaultStatus: http.StatusCreated,
		Errors:        errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *createArtistInput) (*artistOutput, error) {
		a, err := svc.CreateArtist(ctx, in.Body.Name)
		if err != nil {
			return nil, err
		}
		return &artistOutput{Body: artistDTO(a)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "rename-artist",
		Method:      http.MethodPut,
		Path:        "/api/artists/{id}",
		Summary:     "Rename an artist",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *renameArtistInput) (*artistOutput, error) {
		a, err := svc.RenameArtist(ctx, in.ID, in.Body.Name)
		if err != nil {
			return nil, err
		}
		return &artistOutput{Body: artistDTO(a)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "delete-artist",
		Method:      http.MethodDelete,
		Path:        "/api/artists/{id}",
		Summary:     "Delete an artist",
		Description: "Refused while tracks refer to the artist (artist_in_use, with used_by), and for an artist with play history (artist_has_history).",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*struct{}, error) {
		return nil, svc.DeleteArtist(ctx, in.ID)
	})
}
