package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

type trackOutput struct {
	Body dto.Track
}

type trackPageOutput struct {
	Body dto.TrackPage
}

type listTracksInput struct {
	TypeID     uint64 `query:"type_id" minimum:"0" doc:"Only tracks of this type."`
	ArtistID   uint64 `query:"artist_id" minimum:"0" doc:"Only tracks of this artist."`
	PlaylistID uint64 `query:"playlist_id" minimum:"0" doc:"Only tracks in this playlist."`
	Query      string `query:"q" maxLength:"200" doc:"Only tracks whose title or artist name contains this text (case-insensitive)."`
	Quality    string `query:"quality" enum:"ok,issues,unanalysed" doc:"Only tracks analysed without issues, with issues, or never analysed."`
	Reviewed   string `query:"reviewed" enum:"true,false" doc:"Only tracks with (true) or without (false) the reviewed mark."`
	Sort       string `query:"sort" enum:"title,artist,type,duration,format,quality,reviewed" default:"title" doc:"Sort key; ties are ordered by title and ID. Tracks without the value are last in both orders (reviewed: by the time of the mark, tracks not reviewed last)."`
	Order      string `query:"order" enum:"asc,desc" default:"asc"`
	Offset     int    `query:"offset" minimum:"0" maximum:"1000000" default:"0" doc:"Tracks to skip in the sorted list."`
	Cursor     string `query:"cursor" maxLength:"2000" doc:"next_cursor of the previous page; only in the default order (title, asc) and without offset."`
	Limit      int    `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

type createTrackInput struct {
	Body dto.TrackBody
}

type updateTrackInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.TrackBody
}

type deleteTracksInput struct {
	Body dto.DeleteTracksBody
}

type deleteTracksOutput struct {
	Body dto.DeleteTracksResult
}

type reviewTracksInput struct {
	Body dto.ReviewTracksBody
}

type reviewTracksOutput struct {
	Body dto.ReviewTracksResult
}

type renameTracksInput struct {
	Body dto.RenameTracksBody
}

type renameTracksOutput struct {
	Body dto.RenameTracksResult
}

// previewWriteDeadline replaces the server's write timeout for one preview
// response. A paused player stops reading; when the deadline passes, the
// connection closes and the player resumes with a Range request.
const previewWriteDeadline = 30 * time.Minute

func registerTracks(api huma.API, log *slog.Logger, svc Library, rules PlayRules) {
	tags := []string{"Tracks"}

	register(api, log, huma.Operation{
		OperationID: "list-tracks",
		Method:      http.MethodGet,
		Path:        "/api/tracks",
		Summary:     "List tracks",
		Description: "Tracks matching the filters, sorted by `sort` and `order` (ties by title and ID), a page at a time: read the next page with `offset`, or with `next_cursor` as `cursor` in the default title order. `total` counts every matching track. The filters combine.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *listTracksInput) (*trackPageOutput, error) {
		f := library.TrackFilter{
			TypeID: in.TypeID, ArtistID: in.ArtistID, PlaylistID: in.PlaylistID,
			Query: in.Query, Quality: in.Quality,
			Sort: library.TrackSort(in.Sort), Desc: in.Order == "desc", Offset: in.Offset, Limit: in.Limit,
		}
		if in.Reviewed != "" {
			f.Reviewed = new(in.Reviewed == "true")
		}
		if in.Cursor != "" {
			c, err := decodeCursor(in.Cursor)
			if err != nil {
				return nil, validation.Field("query.cursor", validation.Format, "not a cursor returned by this API")
			}
			f.After = &c
		}
		page, err := svc.ListTracks(ctx, f)
		if err != nil {
			return nil, err
		}
		out := &trackPageOutput{Body: dto.TrackPage{Items: make([]dto.Track, 0, len(page.Items)), Total: page.Total}}
		for _, t := range page.Items {
			out.Body.Items = append(out.Body.Items, trackDTO(t))
		}
		if page.Next != nil {
			out.Body.NextCursor = encodeCursor(*page.Next)
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "get-track",
		Method:      http.MethodGet,
		Path:        "/api/tracks/{id}",
		Summary:     "Get a track",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*trackOutput, error) {
		t, err := svc.GetTrack(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &trackOutput{Body: trackDTO(t)}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "create-track",
		Method:        http.MethodPost,
		Path:          "/api/tracks",
		Summary:       "Register a track",
		Description:   "Registers a file by its path under the media root; the file may arrive later. When it exists, metadata left out is read from it with ffprobe. A path used by another track is a field error (taken).",
		Tags:          tags,
		DefaultStatus: http.StatusCreated,
		Errors:        errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *createTrackInput) (*trackOutput, error) {
		t, err := svc.CreateTrack(ctx, trackInput(in.Body))
		if err != nil {
			return nil, err
		}
		return &trackOutput{Body: trackDTO(t)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "update-track",
		Method:      http.MethodPut,
		Path:        "/api/tracks/{id}",
		Summary:     "Update a track",
		Description: "Replaces the metadata and the playlists of a track. Optional fields left out are cleared, then metadata is read from the file where it exists. " +
			"The title is trimmed and runs of white space become one space; control characters (line breaks, tabs) are refused (invalid_characters). " +
			"A new title, artist, or file path clears the reviewed mark; playlists, type, notes, and the quality override do not. " +
			"The title and artist go on air with requests pushed to Liquidsoap after the save; items already queued keep the old ones.",
		Tags:   tags,
		Errors: errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *updateTrackInput) (*trackOutput, error) {
		t, err := svc.UpdateTrack(ctx, in.ID, trackInput(in.Body))
		if err != nil {
			return nil, err
		}
		return &trackOutput{Body: trackDTO(t)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "delete-track",
		Method:      http.MethodDelete,
		Path:        "/api/tracks/{id}",
		Summary:     "Delete a track",
		Description: "Removes the track from the library (the file is not touched). Refused while a clock filler pool holds it (track_in_use, with used_by), and for a track with play history or in the track plan (track_has_history).",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*struct{}, error) {
		return nil, svc.DeleteTrack(ctx, in.ID)
	})

	register(api, log, huma.Operation{
		OperationID: "delete-tracks",
		Method:      http.MethodPost,
		Path:        "/api/tracks/delete",
		Summary:     "Delete several tracks",
		Description: "Deletes several tracks from the library (the files are not touched), each like DELETE /api/tracks/{id}. Refused tracks are listed in refused with the reason; the others are deleted. Runs to the end even when the client disconnects.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *deleteTracksInput) (*deleteTracksOutput, error) {
		res, err := svc.DeleteTracks(ctx, in.Body.TrackIDs)
		if err != nil {
			return nil, err
		}
		out := dto.DeleteTracksResult{Deleted: res.Deleted, Refused: make([]dto.TrackRefusal, len(res.Refused))}
		for i, r := range res.Refused {
			out.Refused[i] = dto.TrackRefusal{TrackID: r.TrackID, Code: string(trackRefusalCode(r.Err)), Title: r.Title}
			for _, u := range r.UsedBy {
				out.Refused[i].UsedBy = append(out.Refused[i].UsedBy, dto.EntityRef{Kind: u.Kind, ID: u.ID, Name: u.Name})
			}
		}
		return &deleteTracksOutput{Body: out}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "review-tracks",
		Method:      http.MethodPost,
		Path:        "/api/tracks/review",
		Summary:     "Mark tracks reviewed",
		Description: "Sets or clears the reviewed mark of several tracks in one statement. A track already reviewed keeps the time of its first mark. Tracks that do not exist are listed in not_found.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *reviewTracksInput) (*reviewTracksOutput, error) {
		res, err := svc.ReviewTracks(ctx, in.Body.TrackIDs, in.Body.Reviewed)
		if err != nil {
			return nil, err
		}
		return &reviewTracksOutput{Body: dto.ReviewTracksResult{Updated: res.Updated, NotFound: res.NotFound}}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "rename-tracks",
		Method:      http.MethodPost,
		Path:        "/api/tracks/rename",
		Summary:     "Find and replace in titles",
		Description: "Replaces every occurrence of find in the titles of the tracks (literal text; case ignored unless match_case), then trims the title and collapses runs of white space. " +
			"With dry_run true it returns the preview and writes nothing; with false it writes the changed titles in one transaction, computed again from the stored titles, and clears their reviewed mark. " +
			"A title that would become empty or longer than 500 characters is refused, not cut.",
		Tags:   tags,
		Errors: errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *renameTracksInput) (*renameTracksOutput, error) {
		b := in.Body
		res, err := svc.RenameTracks(ctx, library.RenameRequest{
			TrackIDs: b.TrackIDs, Find: b.Find, Replace: b.Replace, MatchCase: b.MatchCase, DryRun: b.DryRun,
		})
		if err != nil {
			return nil, err
		}
		out := dto.RenameTracksResult{
			Changed:   make([]dto.TitleChange, len(res.Changed)),
			Unchanged: res.Unchanged,
			Refused:   make([]dto.RenameRefusal, len(res.Refused)),
		}
		for i, c := range res.Changed {
			out.Changed[i] = dto.TitleChange{TrackID: c.TrackID, OldTitle: c.OldTitle, NewTitle: c.NewTitle}
		}
		for i, r := range res.Refused {
			out.Refused[i] = dto.RenameRefusal{TrackID: r.TrackID, Code: string(renameRefusalCode(r.Err))}
		}
		return &renameTracksOutput{Body: out}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "set-track-play-rules",
		Method:      http.MethodPut,
		Path:        "/api/tracks/{id}/play-rules",
		Summary:     "Set the play rules of a track",
		Description: "Rules of the track itself. The rules of its playlists apply too; all of them must pass.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *setPlayRulesInput) (*playRuleIDsOutput, error) {
		ids, err := rules.SetTrackRules(ctx, in.ID, in.Body.RuleIDs)
		if err != nil {
			return nil, err
		}
		return &playRuleIDsOutput{Body: dto.PlayRuleIDs{RuleIDs: emptyIDs(ids)}}, nil
	})

	audio := &huma.Response{
		Description: "The audio file, or the requested range of it.",
		Content:     map[string]*huma.MediaType{"audio/*": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}},
	}
	register(api, log, huma.Operation{
		OperationID: "get-track-audio",
		Method:      http.MethodGet,
		Path:        "/api/tracks/{id}/audio",
		Summary:     "Stream a track for preview",
		Description: "Streams the file of the track for the preview player in the panel (§6.6). Supports Range requests, so seeking works. " +
			"The preview plays in the browser only: it never reaches the air or the play log. A missing file is track_file_not_found.",
		Tags:     tags,
		Metadata: map[string]any{Streaming: true},
		Responses: map[string]*huma.Response{
			"200": audio,
			"206": {Description: "Partial content: the requested range.", Content: audio.Content},
			"416": {Description: "The requested range is outside the file."},
		},
		Errors: errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*huma.StreamResponse, error) {
		a, err := svc.TrackAudio(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			defer func() { _ = a.File.Close() }()
			r, w := humachi.Unwrap(hctx)
			// The server's write timeout is sized for JSON responses.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(previewWriteDeadline))
			w.Header().Set("Content-Type", a.ContentType)
			// ServeContent answers Range, If-Range, and conditional requests.
			http.ServeContent(w, r, a.Name, a.ModTime, a.File)
		}}, nil
	})
}

var errBadCursor = errors.New("cursor has no track ID")

// trackCursor is the JSON inside an opaque cursor.
type trackCursor struct {
	Title string `json:"t"`
	ID    uint64 `json:"i"`
}

func encodeCursor(c library.TrackCursor) string {
	b, _ := json.Marshal(trackCursor{Title: c.Title, ID: c.ID}) // a string and an int always encode
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (library.TrackCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return library.TrackCursor{}, err
	}
	var c trackCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return library.TrackCursor{}, err
	}
	if c.ID < 1 {
		return library.TrackCursor{}, errBadCursor
	}
	return library.TrackCursor{Title: c.Title, ID: c.ID}, nil
}
