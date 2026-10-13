package httpapi

import (
	"log/slog"
	"net/http"
	"time"
)

// Server timeouts. WriteTimeout applies to every response; a streaming route
// (track preview, WebSocket) clears its own write deadline with
// http.ResponseController.SetWriteDeadline.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
)

// NewServer returns an http.Server with explicit timeouts. Errors of the
// server itself (TLS, accept, malformed requests) go to log at WARN.
func NewServer(addr string, h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
}
