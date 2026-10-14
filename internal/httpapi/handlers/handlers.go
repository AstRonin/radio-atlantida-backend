// Package handlers registers the API operations. Handlers are thin: decode
// (done by huma from the DTO types), validate, call the domain service, map
// the result to a response DTO.
package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

// OperationStore reads operations.
type OperationStore interface {
	Get(ctx context.Context, id uint64) (operations.Operation, error)
	List(ctx context.Context, f operations.ListFilter) ([]operations.Operation, error)
}

// LogFiles reads the daily log files.
type LogFiles interface {
	List() ([]logging.FileInfo, error)
	Read(ctx context.Context, name string, f logging.Filter) (logging.Page, error)
}

// Deps are the services behind the handlers. When only the OpenAPI document
// is built, they may be nil: no handler runs.
type Deps struct {
	// Log is the root logger.
	Log        *slog.Logger
	Operations OperationStore
	Logs       LogFiles
	Settings   Settings
	Library    Library
	PlayRules  PlayRules
	Separation Separation
	Clocks     Clocks
	Grid       Grid
	Rotation   Rotation
	Playout    Playout
	// Now is the clock of the live status.
	Now func() time.Time
}

// Register adds every operation to api.
func Register(api huma.API, d Deps) {
	log := logging.For(d.Log, logging.ComponentAPI)
	registerOperations(api, log, d.Operations)
	registerLogs(api, log, d.Logs)
	registerStatus(api, log, d.Now)
	registerSettings(api, log, d.Settings)
	registerPlaylistTypes(api, log, d.Library)
	registerPlaylists(api, log, d.Library, d.PlayRules)
	registerArtists(api, log, d.Library)
	registerTracks(api, log, d.Library, d.PlayRules)
	registerPlayRules(api, log, d.PlayRules)
	registerSeparationRules(api, log, d.Separation)
	registerClocks(api, log, d.Clocks, d.Rotation)
	registerGrid(api, log, d.Grid, d.Rotation)
	registerRotation(api, log, d.Rotation)
	registerPlayout(api, log, d.Playout)
}

// Streaming is the operation metadata key of routes that stream a response
// (track preview). The router serves them without the request timeout.
const Streaming = "streaming"

// IsStreaming reports whether op streams its response.
func IsStreaming(op *huma.Operation) bool {
	v, _ := op.Metadata[Streaming].(bool)
	return v
}

// register adds one operation. Errors returned by fn go through problemFor.
func register[I, O any](api huma.API, log *slog.Logger, op huma.Operation, fn func(context.Context, *I) (*O, error)) {
	huma.Register(api, op, func(ctx context.Context, in *I) (*O, error) {
		out, err := fn(ctx, in)
		if err != nil {
			return nil, problemFor(ctx, log, err)
		}
		return out, nil
	})
}

// commonErrors are documented on every operation.
var commonErrors = []int{http.StatusInternalServerError}

// errorsWith documents the given statuses and the common errors.
func errorsWith(statuses ...int) []int { return append(statuses, commonErrors...) }

// idInput addresses a resource by its ID.
type idInput struct {
	ID uint64 `path:"id" minimum:"1"`
}

// emptyIDs returns ids, or an empty list instead of nil, so the API sends []
// rather than null.
func emptyIDs(ids []uint64) []uint64 {
	if ids == nil {
		return []uint64{}
	}
	return ids
}
