package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

func startHub(t *testing.T) (*Hub, string) {
	t.Helper()
	hub := NewHub(slog.New(slog.DiscardHandler))
	srv := httptest.NewServer(hub)
	t.Cleanup(func() {
		hub.Close()
		srv.Close()
	})
	return hub, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, hub *Hub, url string) *websocket.Conn {
	t.Helper()
	before := hub.Clients()
	conn, resp, err := websocket.Dial(t.Context(), url, nil)
	require.NoError(t, err)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	require.Eventually(t, func() bool { return hub.Clients() == before+1 }, 2*time.Second, 5*time.Millisecond)
	return conn
}

func read(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, typ)
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

func TestPublishReachesEveryClient(t *testing.T) {
	t.Parallel()
	hub, url := startHub(t)
	a, b := dial(t, hub, url), dial(t, hub, url)

	hub.Publish(NewErrorAlert(logging.Alert{
		Time:      time.Date(2026, 10, 13, 10, 44, 0, 0, time.FixedZone("AMT", 4*3600)),
		Component: "db", Message: "insert failed", Error: "timeout",
	}))

	for _, c := range []*websocket.Conn{a, b} {
		require.Equal(t, map[string]any{
			"type": "error.alert",
			"time": "2026-10-13T06:44:00Z",
			"data": map[string]any{"component": "db", "message": "insert failed", "error": "timeout"},
		}, read(t, c))
	}
}

func TestSlowClientIsDropped(t *testing.T) {
	t.Parallel()
	hub, url := startHub(t)
	conn := dial(t, hub, url)

	// The client never reads; its queue fills and the hub drops it without
	// blocking Publish.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range sendBuffer * 4 {
			hub.Publish(map[string]string{"type": "test", "pad": strings.Repeat("x", 64<<10)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow client")
	}
	require.Eventually(t, func() bool { return hub.Clients() == 0 }, 2*time.Second, 5*time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn.SetReadLimit(1 << 20)
	var err error
	for err == nil {
		_, _, err = conn.Read(ctx)
	}
	require.Equal(t, websocket.StatusPolicyViolation, websocket.CloseStatus(err))
}

func TestCloseDisconnectsClientsAndRefusesNewOnes(t *testing.T) {
	t.Parallel()
	hub, url := startHub(t)
	conn := dial(t, hub, url)

	hub.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, err := conn.Read(ctx)
	require.Equal(t, websocket.StatusGoingAway, websocket.CloseStatus(err))
	require.Zero(t, hub.Clients())

	late, resp, err := websocket.Dial(t.Context(), url, nil)
	require.NoError(t, err)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	_, _, err = late.Read(ctx)
	require.Equal(t, websocket.StatusGoingAway, websocket.CloseStatus(err))
}

func TestCrossOriginRefused(t *testing.T) {
	t.Parallel()
	_, url := startHub(t)

	_, resp, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
	})

	require.Error(t, err)
	require.NotNil(t, resp)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestPublishUnencodableEventIsDropped(t *testing.T) {
	t.Parallel()
	hub, url := startHub(t)
	conn := dial(t, hub, url)

	hub.Publish(map[string]any{"bad": func() {}})
	hub.Publish(map[string]string{"type": "ok"})

	require.Equal(t, "ok", read(t, conn)["type"])
}
