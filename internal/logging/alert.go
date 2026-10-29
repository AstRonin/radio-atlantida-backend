package logging

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// alertBuffer is the number of alerts queued for the panel. Alerts beyond it
// are dropped (they are still on stdout and in Loki).
const alertBuffer = 256

// Alert is an ERROR entry forwarded to the panel as an error popup (§10.1).
type Alert struct {
	Time      time.Time
	Component string
	Message   string
	Error     string
	RequestID string
}

// alertHandler passes every record to next and, for ERROR records, also
// queues an Alert. Queuing never blocks: when the queue is full the alert is
// dropped and counted.
type alertHandler struct {
	next    slog.Handler
	alerts  chan<- Alert
	dropped *atomic.Uint64

	// Top-level attributes added through WithAttrs (component, request_id).
	component string
	requestID string
	grouped   bool // attributes added after WithGroup are not top-level
}

func newAlertHandler(next slog.Handler, alerts chan<- Alert, dropped *atomic.Uint64) *alertHandler {
	return &alertHandler{next: next, alerts: alerts, dropped: dropped}
}

func (h *alertHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *alertHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		a := Alert{
			Time:      r.Time.UTC(),
			Component: h.component,
			Message:   r.Message,
			RequestID: h.requestID,
		}
		if !h.grouped {
			r.Attrs(func(attr slog.Attr) bool {
				switch attr.Key {
				case KeyComponent:
					a.Component = attr.Value.String()
				case KeyRequestID:
					a.RequestID = attr.Value.String()
				case KeyError:
					a.Error = attr.Value.String()
				}
				return true
			})
		}
		select {
		case h.alerts <- a:
		default:
			h.dropped.Add(1)
		}
	}
	return h.next.Handle(ctx, r)
}

func (h *alertHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(attrs)
	if !h.grouped {
		for _, a := range attrs {
			switch a.Key {
			case KeyComponent:
				c.component = a.Value.String()
			case KeyRequestID:
				c.requestID = a.Value.String()
			}
		}
	}
	return &c
}

func (h *alertHandler) WithGroup(name string) slog.Handler {
	c := *h
	c.next = h.next.WithGroup(name)
	if name != "" {
		c.grouped = true
	}
	return &c
}
