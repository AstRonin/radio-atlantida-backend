// Package ws is the WebSocket hub at GET /api/ws. It broadcasts UI events
// (operation outcomes, error alerts, now playing, outages, plan warnings)
// to every connected panel (§6, §6.5 of the design).
//
// Every event is a JSON object with the envelope
//
//	{ "type": "...", "time": "2026-10-13T06:44:00Z", "operation_id": 12, "data": { } }
//
// (operation_id only for operation events). Each event type is a named schema
// in the OpenAPI document. The panel never sends messages; the hub only reads
// to process control frames.
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

const (
	// sendBuffer is the number of events queued per client. A client that
	// falls this far behind is dropped, so it never blocks the hub.
	sendBuffer = 64
	// writeTimeout bounds one write to a client.
	writeTimeout = 10 * time.Second
	// pingInterval detects dead connections that never sent a close frame.
	pingInterval = 30 * time.Second
)

// originPatterns are accepted in addition to same-origin requests: a panel
// served by the Angular dev server or another local port. The API has no
// authentication, so arbitrary web origins are not allowed to read events.
var originPatterns = []string{"localhost:*", "127.0.0.1:*", "[::1]:*"}

// Hub fans events out to the connected panels.
//
// The client set is guarded by mu. Each client's connection is owned by the
// HTTP handler goroutine that accepted it; the hub only queues messages and
// closes the queue to tell that goroutine to stop.
type Hub struct {
	log *slog.Logger

	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool
}

type client struct {
	send chan []byte
	// closeCode and closeReason are set by the hub before it closes send.
	closeCode   websocket.StatusCode
	closeReason string
}

// NewHub returns an empty hub. log should carry component api.
func NewHub(log *slog.Logger) *Hub {
	return &Hub{log: log, clients: make(map[*client]struct{})}
}

// Publish sends event to every connected panel. It never blocks: a client
// whose queue is full is disconnected (the panel reconnects and catches up
// through the REST API). event must marshal to a JSON object with a type.
func (h *Hub) Publish(event any) {
	msg, err := json.Marshal(event)
	if err != nil {
		// A programmer error in an event type; WARN, so it is not turned
		// into an error alert that is published through this hub again.
		h.log.Warn("encode websocket event failed", logging.KeyError, err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c.send <- msg:
		default:
			h.dropLocked(c, websocket.StatusPolicyViolation, "client too slow")
			h.log.Warn("websocket client dropped: too slow", "buffer", sendBuffer)
		}
	}
}

// Clients returns the number of connected panels.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Close disconnects every panel with "going away" and refuses new ones. It is
// called on shutdown, because http.Server.Shutdown does not wait for
// hijacked (WebSocket) connections.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for c := range h.clients {
		h.dropLocked(c, websocket.StatusGoingAway, "server shutting down")
	}
}

func (h *Hub) dropLocked(c *client, code websocket.StatusCode, reason string) {
	delete(h.clients, c)
	c.closeCode, c.closeReason = code, reason
	close(c.send)
}

func (h *Hub) add() (*client, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, false
	}
	c := &client{send: make(chan []byte, sendBuffer)}
	h.clients[c] = struct{}{}
	return c, true
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
}

// ServeHTTP upgrades the request and streams events until the panel
// disconnects, the hub drops the client, or the request context ends.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqID := middleware.GetReqID(r.Context())

	// The server's read and write deadlines would cut the connection after
	// the normal request timeouts; a WebSocket lives as long as the panel.
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		h.log.Warn("clear read deadline failed", logging.KeyRequestID, reqID, logging.KeyError, err)
	}
	if err := rc.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		h.log.Warn("clear write deadline failed", logging.KeyRequestID, reqID, logging.KeyError, err)
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: originPatterns})
	if err != nil {
		// Accept has already written the HTTP error response.
		h.log.Debug("websocket upgrade refused", logging.KeyRequestID, reqID, logging.KeyError, err)
		return
	}

	c, ok := h.add()
	if !ok {
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	defer h.remove(c)

	// The panel sends nothing; CloseRead handles pings and the close frame
	// and cancels ctx when the connection ends.
	ctx := conn.CloseRead(r.Context())
	h.log.Debug("websocket client connected", logging.KeyRequestID, reqID)

	if err := h.writeLoop(ctx, conn, c); err != nil {
		h.log.Debug("websocket client disconnected", logging.KeyRequestID, reqID, logging.KeyError, err)
		_ = conn.CloseNow()
	}
}

func (h *Hub) writeLoop(ctx context.Context, conn *websocket.Conn, c *client) error {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-c.send:
			if !ok {
				// Dropped by the hub; closeCode was set under the hub lock
				// before the channel was closed.
				return conn.Close(c.closeCode, c.closeReason)
			}
			if err := write(ctx, conn, msg); err != nil {
				return err
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

func write(ctx context.Context, conn *websocket.Conn, msg []byte) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, msg)
}
