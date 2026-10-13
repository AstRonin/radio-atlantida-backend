package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

type getOperationInput struct {
	ID int64 `path:"id" minimum:"1"`
}

type operationOutput struct {
	Body dto.Operation
}

type listOperationsInput struct {
	Since   time.Time `query:"since" doc:"Only operations started or finished at or after this UTC time."`
	AfterID int64     `query:"after_id" minimum:"0" doc:"Continue after this operation ID (the last ID of the previous page)."`
	Limit   int       `query:"limit" minimum:"1" maximum:"500" default:"100"`
}

type operationListOutput struct {
	Body dto.OperationList
}

func registerOperations(api huma.API, log *slog.Logger, store OperationStore) {
	register(api, log, huma.Operation{
		OperationID: "get-operation",
		Method:      http.MethodGet,
		Path:        "/api/operations/{id}",
		Summary:     "Get an operation",
		Description: "State of one asynchronous operation. A panel that missed the terminal WebSocket event reads the stored outcome here.",
		Tags:        []string{"Operations"},
		Errors:      append([]int{http.StatusNotFound, http.StatusUnprocessableEntity}, commonErrors...),
	}, func(ctx context.Context, in *getOperationInput) (*operationOutput, error) {
		op, err := store.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &operationOutput{Body: operationDTO(op)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "list-operations",
		Method:      http.MethodGet,
		Path:        "/api/operations",
		Summary:     "List operations",
		Description: "Operations in ID order, for a panel that reconnects and catches up on events it missed. Page with after_id.",
		Tags:        []string{"Operations"},
		Errors:      append([]int{http.StatusUnprocessableEntity}, commonErrors...),
	}, func(ctx context.Context, in *listOperationsInput) (*operationListOutput, error) {
		ops, err := store.List(ctx, operations.ListFilter{Since: in.Since, AfterID: in.AfterID, Limit: in.Limit})
		if err != nil {
			return nil, err
		}
		out := &operationListOutput{Body: dto.OperationList{Items: make([]dto.Operation, 0, len(ops))}}
		for _, op := range ops {
			out.Body.Items = append(out.Body.Items, operationDTO(op))
		}
		return out, nil
	})
}

func operationDTO(op operations.Operation) dto.Operation {
	d := dto.Operation{
		ID:        op.ID,
		Kind:      op.Kind,
		Status:    string(op.Status),
		StartedAt: op.StartedAt.UTC(),
	}
	if op.FinishedAt != nil {
		t := op.FinishedAt.UTC()
		d.FinishedAt = &t
	}
	if op.ErrorMessage != nil {
		d.ErrorMessage = *op.ErrorMessage
	}
	if op.ResultSummary != nil {
		d.ResultSummary = *op.ResultSummary
	}
	return d
}
