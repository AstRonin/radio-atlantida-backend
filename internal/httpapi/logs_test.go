package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/logsearch"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// fakeLogSearch records the query and answers with a page or an error.
type fakeLogSearch struct {
	q    logsearch.Query
	page logsearch.Page
	err  error
}

func (f *fakeLogSearch) Search(_ context.Context, q logsearch.Query) (logsearch.Page, error) {
	f.q = q
	return f.page, f.err
}

func newLogsRouter(t *testing.T, search *fakeLogSearch) (http.Handler, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(logging.NewHandler(logs, slog.LevelDebug))
	return NewRouter(Deps{
		Log:       log,
		DB:        fakePinger{},
		LogSearch: search,
		Now:       func() time.Time { return testNow },
	}), logs
}

const logsQuery = "/api/logs?from=2026-10-27T10:00:00Z&to=2026-10-27T11:00:00Z"

func TestListLogsPassesQueryAndEncodesPage(t *testing.T) {
	t.Parallel()
	total := int64(42)
	ts := time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC)
	search := &fakeLogSearch{page: logsearch.Page{
		Items: []logsearch.Entry{{
			Time: ts, Level: "WARN", Message: "slow", Component: "api", RequestID: "r1",
			Attrs: map[string]any{"ms": float64(900)},
		}},
		NextCursor: "djE6MTIz",
		Total:      &total,
	}}
	h, _ := newLogsRouter(t, search)

	rec := do(t, h, http.MethodGet, logsQuery+"&level=WARN&component=api&search=slow&request_id=r1&limit=2&cursor=djE6MTAw")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, logsearch.Query{
		From: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC),
		MinLevel: "WARN", Component: "api", Search: "slow", RequestID: "r1", Limit: 2, Cursor: "djE6MTAw",
	}, search.q)
	require.JSONEq(t, `{
		"items":[{"time":"2026-10-27T10:30:00Z","level":"WARN","message":"slow","component":"api",
			"request_id":"r1","attrs":{"ms":900}}],
		"next_cursor":"djE6MTIz",
		"total":42
	}`, rec.Body.String())
}

func TestListLogsDefaultsAndOmitsAbsentFields(t *testing.T) {
	t.Parallel()
	search := &fakeLogSearch{}
	h, _ := newLogsRouter(t, search)

	rec := do(t, h, http.MethodGet, logsQuery)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 100, search.q.Limit, "the default page size comes from the schema")
	require.JSONEq(t, `{"items":[]}`, rec.Body.String(), "no next_cursor and no total when absent")
}

func TestListLogsRefusesBadParametersWithQueryNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, query, field string
		code               errcodes.Field
	}{
		{"missing to", "/api/logs?from=2026-10-27T10:00:00Z", "query.to", errcodes.Required},
		{"bad time", "/api/logs?from=yesterday&to=2026-10-27T11:00:00Z", "query.from", errcodes.Format},
		{"unknown level", logsQuery + "&level=TRACE", "query.level", errcodes.Enum},
		{"unknown component", logsQuery + "&component=playout", "query.component", errcodes.Enum},
		{"limit above 500", logsQuery + "&limit=501", "query.limit", errcodes.Range},
		{"search above 200", logsQuery + "&search=" + strings.Repeat("a", 201), "query.search", errcodes.Length},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			search := &fakeLogSearch{}
			h, _ := newLogsRouter(t, search)

			rec := do(t, h, http.MethodGet, tt.query)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			p := decodeProblem(t, rec)
			require.Equal(t, errcodes.ValidationFailed, p.Code)
			require.Len(t, p.Errors, 1, rec.Body.String())
			require.Equal(t, tt.field, p.Errors[0].Field)
			require.Equal(t, tt.code, p.Errors[0].Code)
		})
	}
}

func TestListLogsDomainRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		status int
		code   errcodes.Problem
		field  string
		fcode  errcodes.Field
	}{
		{
			"loki down", fmt.Errorf("search: %w", logsearch.ErrUnavailable),
			http.StatusServiceUnavailable, errcodes.LogsUnavailable, "", "",
		},
		{
			"range too long", fmt.Errorf("search: %w", logsearch.ErrRangeTooLong),
			http.StatusUnprocessableEntity, errcodes.RangeTooLong, "", "",
		},
		{
			"domain order rule", validation.Field("query.to", validation.Order, "to must be after from"),
			http.StatusUnprocessableEntity, errcodes.ValidationFailed, "query.to", errcodes.Order,
		},
		{
			"domain enum rule", validation.Field("query.level", validation.Enum, "level must be one of"),
			http.StatusUnprocessableEntity, errcodes.ValidationFailed, "query.level", errcodes.Enum,
		},
		{
			"domain pattern rule", validation.Field("query.request_id", validation.Pattern, "bad characters"),
			http.StatusUnprocessableEntity, errcodes.ValidationFailed, "query.request_id", errcodes.Pattern,
		},
		{
			"domain cursor rule", validation.Field("query.cursor", validation.Invalid, "cursor"),
			http.StatusUnprocessableEntity, errcodes.ValidationFailed, "query.cursor", errcodes.Invalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, logs := newLogsRouter(t, &fakeLogSearch{err: tt.err})

			rec := do(t, h, http.MethodGet, logsQuery)

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			p := decodeProblem(t, rec)
			require.Equal(t, tt.code, p.Code)
			if tt.field != "" {
				require.Len(t, p.Errors, 1)
				require.Equal(t, tt.field, p.Errors[0].Field)
				require.Equal(t, tt.fcode, p.Errors[0].Code)
			}
			if tt.code == errcodes.LogsUnavailable {
				var causes int
				for _, e := range logs.entries(t) {
					if e["msg"] == "log search unavailable" {
						causes++
					}
				}
				require.Equal(t, 1, causes, "the cause is logged once")
				require.NotContains(t, rec.Body.String(), "search:")
			}
		})
	}
}

// The level and component enums of list-logs are literal schema tags; they
// must match the domain's lists.
func TestListLogsEnumsMatchDomain(t *testing.T) {
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
	for _, p := range doc.Paths["/api/logs"]["get"].Parameters {
		enums[p.Name] = p.Schema.Enum
	}
	require.Equal(t, logging.LevelNames(), enums["level"])
	var components []string
	for _, c := range logging.Components() {
		components = append(components, string(c))
	}
	require.Equal(t, components, enums["component"])
}
