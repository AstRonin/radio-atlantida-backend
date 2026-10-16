package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

const dateLayout = "2006-01-02"

type logFilesOutput struct {
	Body dto.LogFileList
}

type readLogInput struct {
	Name      string `path:"name" pattern:"^app-\\d{4}-\\d{2}-\\d{2}\\.log$" example:"app-2026-10-13.log"`
	Level     string `query:"level" enum:"DEBUG,INFO,WARN,ERROR" doc:"Minimum level: WARN returns WARN and ERROR entries."`
	Component string `query:"component" enum:"api,db,rotation,liquidsoap,icecast,webrtc,retention,outage,reconcile,media"`
	Limit     int    `query:"limit" minimum:"1" maximum:"1000" default:"100"`
	Offset    int    `query:"offset" minimum:"0" maximum:"100000" default:"0"`
}

type logEntriesOutput struct {
	Body dto.LogEntryPage
}

func registerLogs(api huma.API, log *slog.Logger, files LogFiles) {
	register(api, log, huma.Operation{
		OperationID: "list-log-files",
		Method:      http.MethodGet,
		Path:        "/api/logs/files",
		Summary:     "List log files",
		Description: "Daily log files (one per UTC date), newest first.",
		Tags:        []string{"Logs"},
		Errors:      commonErrors,
	}, func(context.Context, *struct{}) (*logFilesOutput, error) {
		list, err := files.List()
		if err != nil {
			return nil, err
		}
		out := &logFilesOutput{Body: dto.LogFileList{Items: make([]dto.LogFile, 0, len(list))}}
		for _, f := range list {
			out.Body.Items = append(out.Body.Items, dto.LogFile{
				Name: f.Name, Date: f.Date.Format(dateLayout), SizeBytes: f.Size,
			})
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "read-log-file",
		Method:      http.MethodGet,
		Path:        "/api/logs/files/{name}",
		Summary:     "Read a log file",
		Description: "Entries of one daily log file, newest first, with optional filters.",
		Tags:        []string{"Logs"},
		Errors:      append([]int{http.StatusNotFound, http.StatusUnprocessableEntity}, commonErrors...),
	}, func(ctx context.Context, in *readLogInput) (*logEntriesOutput, error) {
		f := logging.Filter{Component: in.Component, Offset: in.Offset, Limit: in.Limit}
		if in.Level != "" {
			var l slog.Level
			if err := l.UnmarshalText([]byte(in.Level)); err != nil {
				return nil, err // excluded by the enum
			}
			f.MinLevel = &l
		}
		page, err := files.Read(ctx, in.Name, f)
		if err != nil {
			return nil, err
		}
		out := &logEntriesOutput{Body: dto.LogEntryPage{Items: make([]dto.LogEntry, 0, len(page.Entries)), Total: page.Total}}
		for _, e := range page.Entries {
			out.Body.Items = append(out.Body.Items, dto.LogEntry{
				Time: e.Time, Level: e.Level, Message: e.Message, Component: e.Component,
				RequestID: e.RequestID, Error: e.Error, Attrs: e.Attrs,
			})
		}
		return out, nil
	})
}
