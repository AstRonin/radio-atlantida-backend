// Package logging builds the application logger: JSON Lines through log/slog,
// UTC timestamps, secret masking, a non-blocking writer, and daily log files
// (§10.1 of the design).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sync/atomic"
	"time"
)

// Component names the part of the system an entry comes from (§10.1).
type Component string

// The allowed components. Every entry should carry one of them under the
// "component" key, usually through [For].
const (
	ComponentAPI        Component = "api"
	ComponentDB         Component = "db"
	ComponentRotation   Component = "rotation"
	ComponentFiller     Component = "filler"
	ComponentLiquidsoap Component = "liquidsoap"
	ComponentIcecast    Component = "icecast"
	ComponentWebRTC     Component = "webrtc"
	ComponentRetention  Component = "retention"
	ComponentOutage     Component = "outage"
	ComponentReconcile  Component = "reconcile"
)

// Attribute keys shared across packages.
const (
	KeyComponent = "component"
	KeyRequestID = "request_id"
	KeyError     = "err"
)

const (
	bufferEntries = 4096
	// retentionDays applies until the admin setting retention.log_days is
	// read at startup (SetRetentionDays); it equals that setting's default.
	retentionDays = 10
)

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

	async   *AsyncWriter
	rotator *DailyRotator

	alerts        chan Alert
	alertsDropped atomic.Uint64
}

// New creates the application logger. Entries go to stdout and to daily files
// in logRoot, through a buffered channel, so logging never blocks the caller.
func New(logRoot string) (*Logging, error) {
	rotator, err := NewDailyRotator(logRoot, retentionDays, time.Now)
	if err != nil {
		return nil, err
	}

	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)

	async := NewAsyncWriter(teeWriter{os.Stdout, rotator}, bufferEntries, func(w io.Writer) *slog.Logger {
		return slog.New(NewHandler(w, level))
	})

	l := &Logging{
		Level:   level,
		async:   async,
		rotator: rotator,
		alerts:  make(chan Alert, alertBuffer),
	}
	l.Logger = slog.New(newAlertHandler(NewHandler(async, level), l.alerts, &l.alertsDropped))
	return l, nil
}

// Alerts delivers every ERROR entry, for the panel's error popups (§10.1).
// One consumer should read it for the life of the process; while nobody
// reads, alerts are dropped (see AlertsDropped), never blocking the logger.
// The channel is never closed.
func (l *Logging) Alerts() <-chan Alert { return l.alerts }

// AlertsDropped returns how many alerts were dropped because the queue was
// full.
func (l *Logging) AlertsDropped() uint64 { return l.alertsDropped.Load() }

// SetRetentionDays changes how many days of log files are kept (admin
// setting retention.log_days, §6.3).
func (l *Logging) SetRetentionDays(days int) error {
	return l.rotator.SetRetentionDays(days)
}

// Close flushes buffered entries and closes the current log file.
func (l *Logging) Close() error {
	l.async.Close()
	if err := l.rotator.Close(); err != nil {
		return fmt.Errorf("close log file: %w", err)
	}
	return nil
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
// so a broken log file never hides entries on stdout.
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
