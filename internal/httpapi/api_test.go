package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/broadcast"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/handlers"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

type fakeOperations struct {
	ops    map[uint64]operations.Operation
	err    error
	filter operations.ListFilter
}

func (f *fakeOperations) Get(_ context.Context, id uint64) (operations.Operation, error) {
	if f.err != nil {
		return operations.Operation{}, f.err
	}
	op, ok := f.ops[id]
	if !ok {
		return operations.Operation{}, operations.ErrNotFound
	}
	return op, nil
}

func (f *fakeOperations) List(_ context.Context, filter operations.ListFilter) ([]operations.Operation, error) {
	f.filter = filter
	var out []operations.Operation
	for _, op := range f.ops {
		out = append(out, op)
	}
	return out, f.err
}

func newAPIRouter(t *testing.T, ops *fakeOperations, logRoot string) (http.Handler, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(logging.NewHandler(logs, slog.LevelDebug))
	return NewRouter(Deps{
		Log:        log,
		DB:         fakePinger{},
		Operations: ops,
		Logs:       logging.NewFileReader(logRoot),
		Events:     ws.NewHub(log),
		Now:        func() time.Time { return testNow },
	}), logs
}

// testNow is the router's clock in tests; not UTC, so the API's conversion shows.
var testNow = time.Date(2026, 10, 13, 10, 44, 5, 0, time.FixedZone("AMT", 4*60*60))

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, target, nil))
	return rec
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) dto.Problem {
	t.Helper()
	require.Equal(t, dto.ContentTypeProblem, rec.Header().Get("Content-Type"))
	var p dto.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, rec.Code, p.Status)
	require.Equal(t, rec.Header().Get(requestIDHeader), p.RequestID)
	return p
}

func TestGetOperation(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 10, 13, 6, 44, 0, 0, time.UTC)
	finished := started.Add(time.Minute)
	summary := "deleted 3 play log rows and 1 operations"
	ops := &fakeOperations{ops: map[uint64]operations.Operation{
		12: {ID: 12, Kind: "retention", Status: operations.StatusCompleted, StartedAt: started, FinishedAt: &finished, ResultSummary: &summary},
	}}
	h, _ := newAPIRouter(t, ops, t.TempDir())

	rec := do(t, h, http.MethodGet, "/api/operations/12")

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"id":12,"kind":"retention","status":"completed",
		"started_at":"2026-10-13T06:44:00Z","finished_at":"2026-10-13T06:45:00Z",
		"result_summary":"deleted 3 play log rows and 1 operations"}`, rec.Body.String())
}

// Without its sources, everything but the time is unknown, and unknown is
// an explicit null (the update script treats it as on air).
func TestLiveStatusUnknown(t *testing.T) {
	t.Parallel()
	h, _ := newAPIRouter(t, &fakeOperations{}, t.TempDir())

	rec := do(t, h, http.MethodGet, "/api/status/live")

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"on_air":null,"open_outage_id":null,"current_clock":null,
		"time":"2026-10-13T06:44:05Z"}`, rec.Body.String())
}

func TestOperationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		target     string
		err        error
		wantStatus int
		wantCode   errcodes.Problem
		wantFields []dto.FieldError
	}{
		{
			name: "not found", target: "/api/operations/7",
			wantStatus: http.StatusNotFound, wantCode: errcodes.OperationNotFound,
		},
		{
			name: "id out of range", target: "/api/operations/0",
			wantStatus: http.StatusUnprocessableEntity, wantCode: errcodes.ValidationFailed,
			wantFields: []dto.FieldError{{Field: "path.id", Code: errcodes.Range, Message: "expected number >= 1"}},
		},
		{
			name: "limit and since invalid", target: "/api/operations?limit=501&since=yesterday",
			wantStatus: http.StatusUnprocessableEntity, wantCode: errcodes.ValidationFailed,
			wantFields: []dto.FieldError{
				{Field: "query.since", Code: errcodes.Format, Message: "invalid date/time for format 2006-01-02T15:04:05.999999999Z07:00"},
				{Field: "query.limit", Code: errcodes.Range, Message: "expected number <= 500"},
			},
		},
		{
			name: "unexpected error", target: "/api/operations/7", err: errors.New("connection reset"),
			wantStatus: http.StatusInternalServerError, wantCode: errcodes.InternalError,
		},
		{
			name: "unknown route", target: "/api/nothing",
			wantStatus: http.StatusNotFound, wantCode: errcodes.NotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newAPIRouter(t, &fakeOperations{err: tt.err}, t.TempDir())

			rec := do(t, h, http.MethodGet, tt.target)

			require.Equal(t, tt.wantStatus, rec.Code, rec.Body.String())
			p := decodeProblem(t, rec)
			require.Equal(t, tt.wantCode, p.Code)
			require.ElementsMatch(t, tt.wantFields, p.Errors)
			require.NotContains(t, rec.Body.String(), "connection reset")
		})
	}
}

func TestMethodNotAllowedIsProblem(t *testing.T) {
	t.Parallel()
	h, _ := newAPIRouter(t, &fakeOperations{}, t.TempDir())

	rec := do(t, h, http.MethodDelete, "/api/operations/1")

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.Equal(t, errcodes.MethodNotAllowed, decodeProblem(t, rec).Code)
}

func TestUnexpectedErrorLoggedOnceWithRequestID(t *testing.T) {
	t.Parallel()
	h, logs := newAPIRouter(t, &fakeOperations{err: errors.New("connection reset")}, t.TempDir())

	rec := do(t, h, http.MethodGet, "/api/operations/7")

	var errorEntries []map[string]any
	for _, e := range logs.entries(t) {
		if e["level"] == "ERROR" {
			errorEntries = append(errorEntries, e)
		}
	}
	require.Len(t, errorEntries, 1)
	require.Equal(t, "api", errorEntries[0]["component"])
	require.Equal(t, rec.Header().Get(requestIDHeader), errorEntries[0]["request_id"])
}

func TestListOperationsDefaults(t *testing.T) {
	t.Parallel()
	ops := &fakeOperations{}
	h, _ := newAPIRouter(t, ops, t.TempDir())

	rec := do(t, h, http.MethodGet, "/api/operations?since=2026-10-13T00:00:00Z&after_id=5")

	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"items":[]}`, rec.Body.String())
	require.Equal(t, operations.ListFilter{
		Since: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC), AfterID: 5, Limit: 100,
	}, ops.filter)
}

func TestLogsEndpoints(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lines := []string{
		`{"time":"2026-10-13T06:00:00Z","level":"INFO","msg":"first","component":"api"}`,
		`{"time":"2026-10-13T06:01:00Z","level":"ERROR","msg":"second","component":"db","err":"down","pool":"main"}`,
		`{"time":"2026-10-13T06:02:00Z","level":"WARN","msg":"third","component":"api"}`,
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "app-2026-10-13.log"), []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "app-2026-10-12.log"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "other.txt"), nil, 0o600))
	h, _ := newAPIRouter(t, &fakeOperations{}, root)

	rec := do(t, h, http.MethodGet, "/api/logs/files")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"items":[
		{"name":"app-2026-10-13.log","date":"2026-10-13","size_bytes":`+itoa(len(strings.Join(lines, "\n"))+1)+`},
		{"name":"app-2026-10-12.log","date":"2026-10-12","size_bytes":0}]}`, rec.Body.String())

	rec = do(t, h, http.MethodGet, "/api/logs/files/app-2026-10-13.log?level=WARN")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"total":2,"items":[
		{"time":"2026-10-13T06:02:00Z","level":"WARN","message":"third","component":"api"},
		{"time":"2026-10-13T06:01:00Z","level":"ERROR","message":"second","component":"db","error":"down","attrs":{"pool":"main"}}]}`,
		rec.Body.String())

	rec = do(t, h, http.MethodGet, "/api/logs/files/app-2026-10-13.log?component=api&limit=1&offset=1")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"total":2,"items":[
		{"time":"2026-10-13T06:00:00Z","level":"INFO","message":"first","component":"api"}]}`, rec.Body.String())

	rec = do(t, h, http.MethodGet, "/api/logs/files/app-2026-10-11.log")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, errcodes.LogFileNotFound, decodeProblem(t, rec).Code)

	for _, name := range []string{"..%2F..%2Fetc%2Fpasswd", "other.txt"} {
		rec = do(t, h, http.MethodGet, "/api/logs/files/"+name)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, name)
		require.Equal(t, errcodes.Pattern, decodeProblem(t, rec).Errors[0].Code)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestOpenAPIDocument(t *testing.T) {
	t.Parallel()

	raw, err := OpenAPI("1.2.3")
	require.NoError(t, err)

	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Enum       []string `json:"enum"`
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
				OneOf []struct {
					Ref string `json:"$ref"`
				} `json:"oneOf"`
			} `json:"schemas"`
		} `json:"components"`
		Raw map[string]any `json:"-"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Equal(t, "3.1.0", doc.OpenAPI)
	require.Equal(t, "1.2.3", doc.Info.Version)
	for _, p := range []string{
		"/api/operations", "/api/operations/{id}", "/api/logs/files", "/api/logs/files/{name}", "/api/status/live", "/api/ws",
		"/api/settings", "/api/settings/{key}", "/api/settings/secrets",
		"/api/playlist-types", "/api/playlist-types/{id}",
		"/api/playlists", "/api/playlists/{id}", "/api/playlists/{id}/tracks", "/api/playlists/{id}/play-rules",
		"/api/artists", "/api/artists/{id}",
		"/api/tracks", "/api/tracks/{id}", "/api/tracks/{id}/play-rules", "/api/tracks/{id}/audio",
		"/api/play-rules", "/api/play-rules/{id}", "/api/play-rules/{id}/targets",
		"/api/separation-rules", "/api/separation-rules/{id}",
		"/api/playout/callback", "/api/playout/emergency/next", "/api/playout/status", "/api/playout/music-gain",
		"/api/play-history", "/api/missed-items", "/api/missed-items/{id}/dismiss", "/api/missed-items/{id}/reschedule",
		"/api/outages",
		"/api/live/session", "/api/live/status", "/api/broadcast/status", "/api/air-window",
		"/api/import", "/api/import/folders", "/api/import/files", "/api/tracks/analyse", "/api/tracks/transcode",
	} {
		require.Contains(t, doc.Paths, p)
	}
	require.NotContains(t, doc.Paths, "/api/import/inbox", "replaced by the folders (docs/phase-18-plan.md)")

	schemas := doc.Components.Schemas
	require.Equal(t, stringsOf(errcodes.Problems()), schemas["ProblemCode"].Enum)
	require.Equal(t, stringsOf(errcodes.Fields()), schemas["FieldErrorCode"].Enum)
	require.Len(t, schemas["Event"].OneOf, 14)
	require.Equal(t, []string{ws.EventLiveState}, schemas["LiveStateEvent"].Properties["type"].Enum)
	require.Equal(t, []string{ws.EventErrorAlert}, schemas["ErrorAlertEvent"].Properties["type"].Enum)
	require.Equal(t, []string{ws.EventTrackPlanWarning}, schemas["TrackPlanWarningEvent"].Properties["type"].Enum)
	require.Equal(t, []string{ws.EventFillerRequestCreated}, schemas["FillerRequestCreatedEvent"].Properties["type"].Enum)
	require.Equal(t, []string{ws.EventNowPlaying}, schemas["NowPlayingEvent"].Properties["type"].Enum)
	require.Equal(t, []string{ws.EventTrackEnded}, schemas["TrackEndedEvent"].Properties["type"].Enum)
	require.Equal(t, []string{ws.EventLiquidsoapState}, schemas["LiquidsoapStateEvent"].Properties["type"].Enum)
	require.Equal(t, []string{ws.EventIcecastState}, schemas["IcecastStateEvent"].Properties["type"].Enum)
	require.Equal(t, []string{ws.EventBroadcastState}, schemas["BroadcastStateEvent"].Properties["type"].Enum)
	states := stringsOf([]broadcast.State{
		broadcast.StateConnected, broadcast.StateReconnecting, broadcast.StateUnreachable, broadcast.StateUnknown,
	})
	require.Equal(t, states, schemas["BroadcastStateData"].Properties["state"].Enum)
	require.Equal(t, states, schemas["BroadcastStatus"].Properties["state"].Enum)
	require.Equal(t, []string{ws.EventOutageStarted}, schemas["OutageStartedEvent"].Properties["type"].Enum)
	require.Equal(t, []string{ws.EventOutageEnded}, schemas["OutageEndedEvent"].Properties["type"].Enum)
	require.Contains(t, schemas["OperationCompletedEvent"].Properties["type"].Enum, operations.KindMissedItems.CompletedEvent())
	require.Contains(t, schemas["OperationFailedEvent"].Properties["type"].Enum, operations.KindReschedule.FailedEvent())
	require.Equal(t, stringsOf(rotation.WarningCodes()), schemas["TrackPlanWarning"].Properties["code"].Enum)
	require.Contains(t, schemas["OperationCompletedEvent"].Properties["type"].Enum, operations.KindClockBuild.CompletedEvent())
	require.Contains(t, schemas["OperationCompletedEvent"].Properties["type"].Enum, operations.KindRetention.CompletedEvent())
	require.Contains(t, schemas["OperationFailedEvent"].Properties["type"].Enum, operations.KindRetention.FailedEvent())
}

// The component filter of the logs API is a literal enum tag; it must list
// exactly the logging components.
func TestLogComponentEnumMatchesLogging(t *testing.T) {
	t.Parallel()
	raw, err := OpenAPI("dev")
	require.NoError(t, err)
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name   string `json:"name"`
				Schema struct {
					Enum []string `json:"enum"`
				} `json:"schema"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))

	enums := map[string][]string{}
	for _, p := range doc.Paths["/api/logs/files/{name}"]["get"].Parameters {
		enums[p.Name] = p.Schema.Enum
	}
	require.Equal(t, stringsOf(logging.Components()), enums["component"])
	require.Equal(t, logging.LevelNames(), enums["level"])
}

// The quality issue codes and fixes of the track DTO are the domain's: the
// panel words and offers them by code.
func TestTrackQualityEnumsMatchLibrary(t *testing.T) {
	t.Parallel()
	raw, err := OpenAPI("dev")
	require.NoError(t, err)
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Items struct {
						Enum []string `json:"enum"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	track := doc.Components.Schemas["Track"].Properties
	require.Equal(t, stringsOf(library.QualityIssues()), track["quality_issues"].Items.Enum)
	require.Equal(t, stringsOf([]library.Fix{library.FixLoudness, library.FixOptimize}), track["fixes"].Items.Enum)
}

func stringsOf[T ~string](vs []T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return slices.Clip(out)
}

// Streaming operations (track preview) run without the request timeout;
// every other operation keeps it.
func TestStreamingOperationsHaveNoRequestTimeout(t *testing.T) {
	t.Parallel()
	r := chi.NewRouter()
	timed := r.With(middleware.Timeout(time.Minute))
	api := newAPI(timed, r, "dev")
	deadlines := map[string]bool{}
	for _, name := range []string{"normal", "stream"} {
		op := huma.Operation{OperationID: name, Method: http.MethodGet, Path: "/" + name}
		if name == "stream" {
			op.Metadata = map[string]any{handlers.Streaming: true}
		}
		huma.Register(api, op, func(ctx context.Context, _ *struct{}) (*struct{}, error) {
			_, deadlines[name] = ctx.Deadline()
			return nil, nil
		})
	}

	for _, name := range []string{"normal", "stream"} {
		rec := do(t, r, http.MethodGet, "/"+name)
		require.Equal(t, http.StatusNoContent, rec.Code, name)
	}
	require.Equal(t, map[string]bool{"normal": true, "stream": false}, deadlines)
}

// The format enum of the track DTOs lists exactly the stored formats.
func TestTrackFormatEnumMatchesMedia(t *testing.T) {
	t.Parallel()
	raw, err := OpenAPI("dev")
	require.NoError(t, err)
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Enum []string `json:"enum"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	for _, s := range []string{"Track", "TrackBody"} {
		require.Equal(t, media.Formats(), doc.Components.Schemas[s].Properties["format"].Enum, s)
	}
}
