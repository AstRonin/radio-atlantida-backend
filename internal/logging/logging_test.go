package logging

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func TestDailyRotatorSwitchesAtUTCMidnight(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 23:59 UTC on Oct 13 is already Oct 14 in Asia/Yerevan; the file date
	// must still be the UTC date.
	clock := &fakeClock{t: time.Date(2026, 10, 13, 23, 59, 0, 0, time.UTC)}
	r, err := NewDailyRotator(dir, 10, clock.Now)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })

	_, err = r.Write([]byte("a\n"))
	require.NoError(t, err)
	clock.Set(time.Date(2026, 10, 14, 0, 0, 1, 0, time.UTC))
	_, err = r.Write([]byte("b\n"))
	require.NoError(t, err)

	requireFile(t, filepath.Join(dir, "app-2026-10-13.log"), "a\n")
	requireFile(t, filepath.Join(dir, "app-2026-10-14.log"), "b\n")
}

func TestDailyRotatorDeletesFilesOlderThanRetention(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{
		"app-2026-10-02.log", // 12 days old: deleted
		"app-2026-10-03.log", // 11 days old: deleted
		"app-2026-10-04.log", // 10 days old: kept
		"app-2026-10-13.log",
		"other.log", // not a log file of ours: kept
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	clock := &fakeClock{t: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)}
	r, err := NewDailyRotator(dir, 10, clock.Now)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })

	_, err = r.Write([]byte("x\n"))
	require.NoError(t, err)

	var names []string
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	require.ElementsMatch(t, []string{
		"app-2026-10-04.log", "app-2026-10-13.log", "app-2026-10-14.log", "other.log",
	}, names)
}

func TestDailyRotatorSetRetentionDaysCleansAtOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"app-2026-10-10.log", "app-2026-10-12.log"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	clock := &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	r, err := NewDailyRotator(dir, 10, clock.Now)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })

	require.NoError(t, r.SetRetentionDays(3), "before the first write: nothing to clean yet")
	_, err = r.Write([]byte("x\n"))
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(dir, "app-2026-10-10.log"), "4 days old, cleaned on the first switch")

	require.NoError(t, r.SetRetentionDays(1))
	require.NoFileExists(t, filepath.Join(dir, "app-2026-10-12.log"), "2 days old, cleaned without waiting for midnight")
	require.FileExists(t, filepath.Join(dir, "app-2026-10-14.log"))
}

func requireFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // test file in a temp dir
	require.NoError(t, err)
	require.Equal(t, want, string(got))
}

func TestNewWritesToLogRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	l, err := New(dir)
	require.NoError(t, err)

	l.Logger.Info("hello", "component", "api")
	require.NoError(t, l.Close())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Regexp(t, FileNamePattern, entries[0].Name())
}
