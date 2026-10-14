package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/handlers"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

const (
	apiTitle       = "Radio Atlantida API"
	apiDescription = "REST API of the Radio Atlantida backend. No authentication: the API is reachable only " +
		"from localhost or a private network. All times are ISO 8601 UTC. Errors are RFC 9457 problem " +
		"details with stable codes. UI events are sent over the WebSocket at GET /api/ws; their payloads " +
		"are the schemas Event, ErrorAlertEvent, and Operation*Event."
)

var installErrorHook sync.Once

// newAPI creates the huma API. Operations are registered on timed, except
// streaming ones (handlers.IsStreaming), which go on untimed. Every
// operation is generated from Go types, so the OpenAPI document always
// matches the code (§6.4).
func newAPI(timed, untimed chi.Router, version string) huma.API {
	// huma builds its own errors (decoding, validation) through this
	// package-level hook. It is set once, before the first API exists, and
	// never changed; every API of the process uses the same problem format.
	installErrorHook.Do(func() {
		huma.NewErrorWithContext = handlers.NewHumaError
		huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
			return handlers.NewHumaError(nil, status, msg, errs...)
		}
	})

	cfg := huma.DefaultConfig(apiTitle, version)
	cfg.Info.Description = apiDescription
	// The document is published by `backend openapi` and CI, not served:
	// no /openapi, /docs, or /schemas routes, and no $schema links in bodies.
	cfg.OpenAPIPath = ""
	cfg.DocsPath = ""
	cfg.SchemasPath = ""
	cfg.CreateHooks = nil

	api := huma.NewAPI(cfg, routeAdapter{
		Adapter: humachi.NewAdapter(timed),
		untimed: humachi.NewAdapter(untimed),
	})
	registerEventSchemas(api.OpenAPI().Components.Schemas)
	documentWebSocket(api.OpenAPI())
	return api
}

// routeAdapter registers streaming operations without the request timeout:
// the timeout would answer 504 in the middle of a long response. Both
// routers share one tree, so ServeHTTP of either serves every route.
type routeAdapter struct {
	huma.Adapter
	untimed huma.Adapter
}

func (a routeAdapter) Handle(op *huma.Operation, handler func(huma.Context)) {
	if handlers.IsStreaming(op) {
		a.untimed.Handle(op, handler)
		return
	}
	a.Adapter.Handle(op, handler)
}

// documentWebSocket lists GET /api/ws in the document. The route itself is a
// plain chi handler (the hub), because huma does not upgrade connections.
func documentWebSocket(doc *huma.OpenAPI) {
	doc.AddOperation(&huma.Operation{
		OperationID: "events-websocket",
		Method:      http.MethodGet,
		Path:        wsPath,
		Summary:     "UI event stream (WebSocket)",
		Description: "Upgrades to a WebSocket. The server sends one JSON text message per event (schema Event); " +
			"the panel sends nothing. A client that falls behind is disconnected and should reconnect and " +
			"catch up through GET /api/operations.",
		Tags: []string{"Events"},
		Responses: map[string]*huma.Response{
			"101": {
				Description: "Switching Protocols: the connection is a WebSocket. Each message is one Event.",
				Content: map[string]*huma.MediaType{
					"application/json": {Schema: &huma.Schema{Ref: "#/components/schemas/Event"}},
				},
			},
			"403": {Description: "Cross-origin request from an origin that is not allowed."},
		},
	})
}

// registerEventSchemas adds the WebSocket events to the document. No
// REST operation returns them, so they are added explicitly: each with its
// type property limited to its event types, and an Event union with a
// discriminator on type, referenced by the documented WebSocket route (huma
// drops unreferenced schemas from the document).
func registerEventSchemas(reg huma.Registry) {
	var completed, failed, progress []string
	for _, k := range operations.Kinds() {
		completed = append(completed, k.CompletedEvent())
		failed = append(failed, k.FailedEvent())
		progress = append(progress, k.ProgressEvent())
	}
	events := []struct {
		typ   reflect.Type
		types []string
	}{
		{reflect.TypeFor[ws.ErrorAlertEvent](), []string{ws.EventErrorAlert}},
		{reflect.TypeFor[ws.OperationCompletedEvent](), completed},
		{reflect.TypeFor[ws.OperationFailedEvent](), failed},
		{reflect.TypeFor[ws.OperationProgressEvent](), progress},
		{reflect.TypeFor[ws.TrackPlanWarningEvent](), []string{ws.EventTrackPlanWarning}},
		{reflect.TypeFor[ws.FillerRequestCreatedEvent](), []string{ws.EventFillerRequestCreated}},
	}

	union := &huma.Schema{
		Description:   "Any event sent over the WebSocket at GET /api/ws.",
		Discriminator: &huma.Discriminator{PropertyName: "type", Mapping: map[string]string{}},
	}
	for _, e := range events {
		ref := reg.Schema(e.typ, true, "")
		name := e.typ.Name()
		s := reg.Map()[name]
		s.Properties["type"].Enum = toAny(e.types)
		union.OneOf = append(union.OneOf, ref)
		for _, t := range e.types {
			union.Discriminator.Mapping[t] = ref.Ref
		}
	}
	reg.Map()["Event"] = union
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// OpenAPI returns the OpenAPI document (JSON) of the API. It needs no
// database or other service.
func OpenAPI(version string) ([]byte, error) {
	r := chi.NewRouter()
	api := newAPI(r, r, version)
	handlers.Register(api, handlers.Deps{Log: discardLogger()})
	doc, err := json.MarshalIndent(api.OpenAPI(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode openapi: %w", err)
	}
	return append(doc, '\n'), nil
}
