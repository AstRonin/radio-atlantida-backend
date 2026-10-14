package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/ingest"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// Ingest imports, analyses, and transcodes track files (*ingest.Service).
type Ingest interface {
	Inbox(ctx context.Context) (ingest.Inbox, error)
	Import(ctx context.Context, req ingest.ImportRequest) (uint64, error)
	Analyse(ctx context.Context, req ingest.AnalyseRequest) (uint64, error)
	Transcode(ctx context.Context, req ingest.TranscodeRequest) (uint64, error)
}

type inboxOutput struct {
	Body dto.Inbox
}

type importInput struct {
	Body dto.ImportBody
}

type analyseInput struct {
	Body dto.AnalyseBody
}

type transcodeInput struct {
	Body dto.TranscodeBody
}

// mediaBusy is the documented refusal of the three media operations.
const mediaBusy = "Refused with media_operation_running while another import, analysis, or transcode runs, and with media_tools_unavailable when ffmpeg is missing."

func registerIngest(api huma.API, log *slog.Logger, svc Ingest) {
	tags := []string{"Import"}

	register(api, log, huma.Operation{
		OperationID: "get-import-inbox",
		Method:      http.MethodGet,
		Path:        "/api/import/inbox",
		Summary:     "List the inbox",
		Description: "The files waiting in the inbox folder under the media root (setting import.inbox_dir), at most 1,000.",
		Tags:        tags,
	}, func(ctx context.Context, _ *struct{}) (*inboxOutput, error) {
		in, err := svc.Inbox(ctx)
		if err != nil {
			return nil, err
		}
		out := &inboxOutput{Body: dto.Inbox{Dir: in.Dir, More: in.More, Items: make([]dto.InboxFile, len(in.Files))}}
		for i, f := range in.Files {
			out.Body.Items[i] = dto.InboxFile{Path: f.Path, Size: f.Size, ModifiedAt: f.ModTime.UTC()}
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "start-import",
		Method:      http.MethodPost,
		Path:        "/api/import",
		Summary:     "Import the inbox",
		Description: "Starts an import operation: every audio file of the inbox is analysed, moved to the library folder (setting import.library_dir), " +
			"and registered as a track with the given type and playlists. Title and artist come from the file's tags, else from its name. " +
			"A file already in the library (same checksum) or not readable as audio stays in the inbox. " + mediaBusy,
		Tags:          tags,
		DefaultStatus: http.StatusAccepted,
		Errors:        errorsWith(http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable),
	}, func(ctx context.Context, in *importInput) (*operationAcceptedOutput, error) {
		id, err := svc.Import(ctx, ingest.ImportRequest{TypeID: in.Body.TypeID, PlaylistIDs: in.Body.PlaylistIDs})
		if err != nil {
			return nil, err
		}
		return &operationAcceptedOutput{Body: dto.OperationAccepted{OperationID: id}}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "analyse-tracks",
		Method:      http.MethodPost,
		Path:        "/api/tracks/analyse",
		Summary:     "Analyse track files",
		Description: "Starts an analysis operation: reads format, bitrate, sample rate, channels, duration, and loudness of the files and checks them against the broadcast standard (quality_issues). " +
			"A missing file gets file_missing. " + mediaBusy,
		Tags:          []string{"Tracks"},
		DefaultStatus: http.StatusAccepted,
		Errors:        errorsWith(http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable),
	}, func(ctx context.Context, in *analyseInput) (*operationAcceptedOutput, error) {
		id, err := svc.Analyse(ctx, ingest.AnalyseRequest{TrackIDs: in.Body.TrackIDs, Force: in.Body.Force})
		if err != nil {
			return nil, err
		}
		return &operationAcceptedOutput{Body: dto.OperationAccepted{OperationID: id}}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "transcode-tracks",
		Method:      http.MethodPost,
		Path:        "/api/tracks/transcode",
		Summary:     "Fix track files",
		Description: "Starts a transcode operation (Fix loudness, Optimize track). A track is skipped when the fix removes none of its issues, while it is queued, on air, " +
			"or planned to start within playout.prepush_minutes + 5 minutes, and when the target path belongs to another track. " + mediaBusy,
		Tags:          []string{"Tracks"},
		DefaultStatus: http.StatusAccepted,
		Errors:        errorsWith(http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable),
	}, func(ctx context.Context, in *transcodeInput) (*operationAcceptedOutput, error) {
		id, err := svc.Transcode(ctx, ingest.TranscodeRequest{
			TrackIDs: in.Body.TrackIDs, Fix: library.Fix(in.Body.Fix), Output: ingest.OutputMode(in.Body.Output),
		})
		if err != nil {
			return nil, err
		}
		return &operationAcceptedOutput{Body: dto.OperationAccepted{OperationID: id}}, nil
	})
}
