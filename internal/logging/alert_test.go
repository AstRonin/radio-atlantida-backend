package logging

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAlertHandlerQueuesErrorsOnly(t *testing.T) {
	t.Parallel()
	alerts := make(chan Alert, 4)
	var dropped atomic.Uint64
	var out bytes.Buffer
	log := slog.New(newAlertHandler(NewHandler(&out, slog.LevelDebug), alerts, &dropped))
	apiLog := For(log, ComponentAPI).With(KeyRequestID, "req-1")

	apiLog.Warn("not an alert")
	apiLog.Error("request failed", KeyError, errors.New("boom"), "password", "hunter2")
	For(log, ComponentDB).WithGroup("pool").Error("grouped", KeyError, "inner")

	require.Len(t, alerts, 2)
	a := <-alerts
	require.Equal(t, "api", a.Component)
	require.Equal(t, "request failed", a.Message)
	require.Equal(t, "boom", a.Error)
	require.Equal(t, "req-1", a.RequestID)
	require.False(t, a.Time.IsZero())
	require.Equal(t, "UTC", a.Time.Location().String())

	// Attributes inside a group are not top-level fields of the entry.
	g := <-alerts
	require.Equal(t, "db", g.Component)
	require.Empty(t, g.Error)

	// The entries still reach the log, masked.
	require.Equal(t, 3, bytes.Count(out.Bytes(), []byte("\n")))
	require.NotContains(t, out.String(), "hunter2")
}

func TestAlertHandlerNeverBlocks(t *testing.T) {
	t.Parallel()
	alerts := make(chan Alert, 1)
	var dropped atomic.Uint64
	log := slog.New(newAlertHandler(NewHandler(io.Discard, slog.LevelInfo), alerts, &dropped))

	for range 3 {
		log.Error("e")
	}

	require.Len(t, alerts, 1)
	require.Equal(t, uint64(2), dropped.Load())
}
