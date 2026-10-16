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
	Folders(ctx context.Context) (ingest.Folders, error)
	Files(ctx context.Context, folder string) (ingest.FolderFiles, error)
	Import(ctx context.Context, req ingest.ImportRequest) (uint64, error)
	Analyse(ctx context.Context, req ingest.AnalyseRequest) (uint64, error)
	Transcode(ctx context.Context, req ingest.TranscodeRequest) (uint64, error)
	Waveform(ctx context.Context, trackID uint64) (ingest.Waveform, error)
}

type importFoldersOutput struct {
	Body dto.ImportFolders
}

type importFilesInput struct {
	Folder string `query:"folder" maxLength:"1024" doc:"Folder relative to the media root (music/rock); absent or empty: the media root."`
}

type importFilesOutput struct {
	Body dto.ImportFiles
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
		OperationID: "list-import-folders",
		Method:      http.MethodGet,
		Path:        "/api/import/folders",
		Summary:     "List the folders of the media root",
		Description: "Every folder under the media root, nested ones included, in path order, at most 500, with the audio files in it and its subfolders " +
			"and how many of them are new. The counts compare paths only; no file is read.",
		Tags: tags,
	}, func(ctx context.Context, _ *struct{}) (*importFoldersOutput, error) {
		fs, err := svc.Folders(ctx)
		if err != nil {
			return nil, err
		}
		out := &importFoldersOutput{Body: dto.ImportFolders{
			Root: importCounts(fs.Root), Folders: make([]dto.ImportFolder, len(fs.Folders)), More: fs.More,
		}}
		for i, f := range fs.Folders {
			out.Body.Folders[i] = dto.ImportFolder{Path: f.Path, ImportCounts: importCounts(f.Counts)}
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "list-import-files",
		Method:      http.MethodGet,
		Path:        "/api/import/files",
		Summary:     "List the files of a folder",
		Description: "The files of a folder of the media root and its subfolders, in path order, at most 1,000, with what an import would do with each. " +
			"A folder that is not a clean relative path, is hidden, or does not exist is refused per field (folder, invalid).",
		Tags:   tags,
		Errors: errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *importFilesInput) (*importFilesOutput, error) {
		fs, err := svc.Files(ctx, in.Folder)
		if err != nil {
			return nil, err
		}
		out := &importFilesOutput{Body: dto.ImportFiles{Folder: fs.Folder, Items: make([]dto.ImportFile, len(fs.Files)), More: fs.More}}
		for i, f := range fs.Files {
			item := dto.ImportFile{Path: f.Path, Size: f.Size, ModifiedAt: f.ModTime.UTC(), Status: string(f.Status)}
			if f.TrackID != 0 {
				item.TrackID = new(f.TrackID)
			}
			out.Body.Items[i] = item
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "start-import",
		Method:      http.MethodPost,
		Path:        "/api/import",
		Summary:     "Import a folder",
		Description: "Starts an import operation: every audio file of the folder and its subfolders that is not a track yet becomes one where it lies, " +
			"with the given type, playlists, and artist; nothing is moved, and tracks that exist already are not changed. " +
			"The title comes from the title tag, else from the file name; the artist from artist_id, else the artist tag, else the file name " +
			"(\"Title - Artist\" with exactly one hyphen). A file with the content of an existing track (a copy) or not readable as audio is left alone. " +
			mediaBusy,
		Tags:          tags,
		DefaultStatus: http.StatusAccepted,
		Errors:        errorsWith(http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable),
	}, func(ctx context.Context, in *importInput) (*operationAcceptedOutput, error) {
		id, err := svc.Import(ctx, ingest.ImportRequest{
			Folder: in.Body.Folder, TypeID: in.Body.TypeID, PlaylistIDs: in.Body.PlaylistIDs, ArtistID: in.Body.ArtistID,
		})
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

func importCounts(c ingest.Counts) dto.ImportCounts {
	return dto.ImportCounts{AudioFiles: c.AudioFiles, NewFiles: c.NewFiles}
}
