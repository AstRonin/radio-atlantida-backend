// Package logging builds the application logger: JSON Lines through log/slog,
// UTC timestamps, secret masking, a non-blocking writer, and the push of every
// line to Loki (docs/logs-loki-plan.md §4.1). The backend keeps no log files.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sync/atomic"
)

// Component names the part of the system an entry comes from (§10.1).
type Component string

// The allowed components. Every entry should carry one of them under the
// "component" key, usually through [For].
const (
	ComponentAPI        Component = "api"
	ComponentDB         Component = "db"
	ComponentRotation   Component = "rotation"
	ComponentLiquidsoap Component = "liquidsoap"
	ComponentIcecast    Component = "icecast"
	ComponentWebRTC     Component = "webrtc"
	ComponentRetention  Component = "retention"
	ComponentOutage     Component = "outage"
	ComponentReconcile  Component = "reconcile"
	// ComponentMedia: import, analysis, and transcoding of track files.
	ComponentMedia Component = "media"
)

// Attribute keys shared across packages.
const (
	KeyComponent = "component"
	KeyRequestID = "request_id"
	KeyError     = "err"
)

// bufferEntries is how many entries wait for the stdout writer. A busy
// backend fills it only when stdout itself stalls; the entry is then dropped
// and counted (see AsyncWriter).
const bufferEntries = 4096

// For returns a logger that tags every entry with the component.
func For(log *slog.Logger, c Component) *slog.Logger {
	return log.With(KeyComponent, string(c))
}

// Logging owns the logger and its writers. Close it last, after everything
// that logs has stopped.
type Logging struct {
	Logger *slog.Logger
	// Level can be changed at runtime (a future admin setting).
	Level *slog.LevelVar

	async *AsyncWriter
	// loki pushes the lines to Loki; nil when LOKI_URL is empty.
	loki *LokiSink

	alerts        chan Alert
	alertsDropped atomic.Uint64
}

// New creates the application logger. Entries go to stdout and, when lokiURL
// is set, to Loki's push API (run by RunPush). Every destination is fed
// through buffered channels, so logging never blocks the caller.
func New(lokiURL string) *Logging {
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)

	var out io.Writer = os.Stdout
	var loki *LokiSink
	if lokiURL != "" {
		// The sink reports its own losses to stdout only (see LokiSink.report).
		report := For(slog.New(NewHandler(os.Stdout, slog.LevelInfo)), ComponentAPI)
		loki = newLokiSink(lokiURL, report, lokiBuffer)
		out = teeWriter{os.Stdout, loki}
	}
	async := NewAsyncWriter(out, bufferEntries, func(w io.Writer) *slog.Logger {
		return slog.New(NewHandler(w, level))
	})

	l := &Logging{
		Level:  level,
		async:  async,
		loki:   loki,
		alerts: make(chan Alert, alertBuffer),
	}
	l.Logger = slog.New(newAlertHandler(NewHandler(async, level), l.alerts, &l.alertsDropped))
	return l
}

// RunPush sends the lines to Loki until ctx ends, then flushes what is still
// buffered. It returns at once when LOKI_URL is empty. serve runs it in its
// errgroup.
func (l *Logging) RunPush(ctx context.Context) error {
	if l.loki == nil {
		return nil
	}
	return l.loki.Run(ctx)
}

// Alerts delivers every ERROR entry, for the panel's error popups (§10.1).
// One consumer should read it for the life of the process; while nobody
// reads, alerts are dropped (see AlertsDropped), never blocking the logger.
// The channel is never closed.
func (l *Logging) Alerts() <-chan Alert { return l.alerts }

// AlertsDropped returns how many alerts were dropped because the queue was
// full.
func (l *Logging) AlertsDropped() uint64 { return l.alertsDropped.Load() }

// Close flushes buffered entries and sends the lines still buffered to Loki
// (when the push runs). ctx is the shutdown's context: the Loki flush gets its
// own timeout from it, so it still runs after ctx is cancelled. RunPush must
// have returned before Close.
func (l *Logging) Close(ctx context.Context) {
	l.async.Close()
	if l.loki != nil {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lokiFlushTimeout)
		l.loki.Close(fctx)
		cancel()
	}
}

// NewHandler returns the JSON handler with the project conventions: UTC
// RFC 3339 timestamps and masked secrets.
func NewHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: replaceAttr,
	})
}

// sensitiveKey matches attribute keys whose values must never be logged.
var sensitiveKey = regexp.MustCompile(`(?i)password|secret|token|credential`)

// Masked replaces the value of a sensitive attribute.
const Masked = "***"

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		return slog.Time(slog.TimeKey, a.Value.Time().UTC())
	}
	if sensitiveKey.MatchString(a.Key) {
		return slog.String(a.Key, Masked)
	}
	return a
}

// teeWriter writes every entry to all writers, even when one of them fails,
// so a failing destination never hides entries on the others.
type teeWriter []io.Writer

func (t teeWriter) Write(p []byte) (int, error) {
	var first error
	for _, w := range t {
		if _, err := w.Write(p); err != nil && first == nil {
			first = err
		}
	}
	return len(p), first
}

// LevelNames are the level names written by the logger, lowest first.
func LevelNames() []string {
	return []string{
		slog.LevelDebug.String(), slog.LevelInfo.String(), slog.LevelWarn.String(), slog.LevelError.String(),
	}
}

// Components lists the allowed components (§10.1).
func Components() []Component {
	return []Component{
		ComponentAPI, ComponentDB, ComponentRotation, ComponentLiquidsoap,
		ComponentIcecast, ComponentWebRTC, ComponentRetention, ComponentOutage, ComponentReconcile, ComponentMedia,
	}
}
