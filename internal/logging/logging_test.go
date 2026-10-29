package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func decode(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(line, &m))
	return m
}

func TestHandlerMasksSecretsAndUsesUTC(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(NewHandler(&buf, slog.LevelInfo)).With("db_password", "p1")

	log.Info("connect",
		"Password", "p2",
		"api_token", "p3",
		"client_secret", "p4",
		"Credentials", "p5",
		slog.Group("icecast", "source_password", "p6", "host", "icecast"),
		"user", "admin",
	)

	out := buf.String()
	for _, secret := range []string{"p1", "p2", "p3", "p4", "p5", "p6"} {
		require.NotContains(t, out, `"`+secret+`"`)
	}
	m := decode(t, buf.Bytes())
	require.Equal(t, Masked, m["Password"])
	require.Equal(t, "admin", m["user"])
	require.Equal(t, "icecast", m["icecast"].(map[string]any)["host"])
	require.True(t, strings.HasSuffix(m["time"].(string), "Z"), "time %q is not UTC", m["time"])
	_, err := time.Parse(time.RFC3339, m["time"].(string))
	require.NoError(t, err)
}

func TestForAddsComponent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer

	For(slog.New(NewHandler(&buf, slog.LevelInfo)), ComponentAPI).Warn("x")

	require.Equal(t, "api", decode(t, buf.Bytes())["component"])
}

// blockingWriter blocks every write until release is closed.
type blockingWriter struct {
	release chan struct{}
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	<-b.release
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *blockingWriter) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAsyncWriterDropsWhenFullAndReportsOnce(t *testing.T) {
	t.Parallel()
	dst := &blockingWriter{release: make(chan struct{})}
	w := NewAsyncWriter(dst, 2, func(w io.Writer) *slog.Logger {
		return slog.New(NewHandler(w, slog.LevelInfo))
	})

	// The drain goroutine takes at most one entry and blocks on it; the
	// buffer holds two more. Everything after that is dropped, and Write
	// still returns at once.
	start := time.Now()
	for range 10 {
		n, err := w.Write([]byte("entry\n"))
		require.NoError(t, err)
		require.Equal(t, 6, n)
	}
	require.Less(t, time.Since(start), time.Second)
	require.GreaterOrEqual(t, w.Dropped(), uint64(7))

	close(dst.release)
	w.Close()

	out := dst.String()
	require.Equal(t, 1, strings.Count(out, "log entries dropped"))
	require.GreaterOrEqual(t, strings.Count(out, "entry\n"), 2)
	require.Zero(t, w.Dropped())

	_, err := w.Write([]byte("late\n"))
	require.ErrorIs(t, err, ErrClosed)
	w.Close() // idempotent
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrPermission }

func TestAsyncWriterSurvivesWriteFailure(t *testing.T) {
	t.Parallel()
	var ok bytes.Buffer
	w := NewAsyncWriter(teeWriter{failingWriter{}, &ok}, 8, func(w io.Writer) *slog.Logger {
		return slog.New(NewHandler(w, slog.LevelInfo))
	})

	_, err := w.Write([]byte("still here\n"))
	require.NoError(t, err)
	w.Close()

	require.Equal(t, "still here\n", ok.String())
}
