package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/logsearch"
)

type logEntriesOutput struct {
	Body dto.LogEntryPage
}

// listLogsInput is the query of GET /api/logs. Enum and limit rules are
// schema rules (huma refuses them with the query name); the rest is checked
// by logsearch.Query.Validate.
type listLogsInput struct {
	From      time.Time `query:"from" required:"true" doc:"Start of the search, inclusive (RFC 3339)."`
	To        time.Time `query:"to" required:"true" doc:"End of the search, exclusive. Must be after from."`
	Level     string    `query:"level" enum:"DEBUG,INFO,WARN,ERROR" doc:"Minimum level: WARN returns WARN and ERROR lines."`
	Component string    `query:"component" enum:"api,db,rotation,liquidsoap,icecast,webrtc,retention,outage,reconcile,media" doc:"Only lines of this component."`
	Search    string    `query:"search" minLength:"1" maxLength:"200" doc:"Text the message must contain (1 to 200 characters, case-insensitive, plain text)."`
	RequestID string    `query:"request_id" doc:"Only lines of this request ID."`
	Limit     int       `query:"limit" minimum:"1" maximum:"500" default:"100" doc:"Page size."`
	Cursor    string    `query:"cursor" doc:"next_cursor of the previous page; empty for the first page."`
}

func registerLogs(api huma.API, log *slog.Logger, search LogSearch) {
	register(api, log, huma.Operation{
		OperationID: "list-logs",
		Method:      http.MethodGet,
		Path:        "/api/logs",
		Summary:     "Search log lines",
		Description: "Log lines of the backend, newest first, read from Loki. Filters are combined. " +
			"The page has next_cursor while older lines match; total counts all matches over the range when it can.",
		Tags:   []string{"Logs"},
		Errors: errorsWith(http.StatusUnprocessableEntity, http.StatusServiceUnavailable),
	}, func(ctx context.Context, in *listLogsInput) (*logEntriesOutput, error) {
		page, err := search.Search(ctx, logsearch.Query{
			From: in.From, To: in.To, MinLevel: in.Level, Component: in.Component,
			Search: in.Search, RequestID: in.RequestID, Limit: in.Limit, Cursor: in.Cursor,
		})
		if err != nil {
			return nil, err
		}
		out := &logEntriesOutput{Body: dto.LogEntryPage{
			Items: make([]dto.LogEntry, 0, len(page.Items)), NextCursor: page.NextCursor, Total: page.Total,
		}}
		for _, e := range page.Items {
			out.Body.Items = append(out.Body.Items, dto.LogEntry{
				Time: e.Time, Level: e.Level, Message: e.Message, Component: e.Component,
				RequestID: e.RequestID, Error: e.Error, Attrs: e.Attrs,
			})
		}
		return out, nil
	})
}
