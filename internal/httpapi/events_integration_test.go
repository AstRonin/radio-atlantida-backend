//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	operationsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/workerpool"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// An operation emits its terminal event over the WebSocket, and the same
// outcome is readable through the REST API (init plan, Phase 3 "done when").
func TestOperationEventOverWebSocketAndREST(t *testing.T) {
	d := dbtest.Migrated(t)
	log := dbtest.Logger(t)
	hub := ws.NewHub(log)
	pool := workerpool.New(1, 1)
	t.Cleanup(pool.Close)
	tracker := operations.NewTracker(operationsrepo.New(d.Pool), hub, pool, log, time.Now)
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Log: log, DB: d.Pool, Events: hub, Operations: tracker, Logs: logging.NewFileReader(t.TempDir()),
	}))
	t.Cleanup(func() {
		hub.Close()
		srv.Close()
	})

	conn, resp, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/ws", nil)
	require.NoError(t, err)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	require.Eventually(t, func() bool { return hub.Clients() == 1 }, 2*time.Second, 5*time.Millisecond)

	id, err := tracker.Run(t.Context(), operations.KindRetention, logging.ComponentRetention,
		func(context.Context) (string, error) { return "deleted 0 play log rows and 0 operations", nil })
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, msg, err := conn.Read(ctx)
	require.NoError(t, err)
	var event ws.OperationCompletedEvent
	require.NoError(t, json.Unmarshal(msg, &event))
	require.Equal(t, "retention.completed", event.Type)
	require.Equal(t, id, event.OperationID)
	require.Equal(t, "retention", event.Data.Kind)
	require.Contains(t, string(msg), `Z"`, "event time is UTC")
	tracker.Wait()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/operations/"+strconv.FormatInt(id, 10), nil)
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode, string(body))

	var op struct {
		ID            int64  `json:"id"`
		Status        string `json:"status"`
		ResultSummary string `json:"result_summary"`
	}
	require.NoError(t, json.Unmarshal(body, &op))
	require.Equal(t, id, op.ID)
	require.Equal(t, "completed", op.Status)
	require.Equal(t, event.Data.Summary, op.ResultSummary)
}
