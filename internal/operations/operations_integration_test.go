//go:build integration

package operations_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	operationsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

type recorder struct {
	mu     sync.Mutex
	events []any
}

func (r *recorder) Publish(e any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) all() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]any(nil), r.events...)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) errorEntries(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m))
		if m["level"] == "ERROR" {
			out = append(out, m)
		}
	}
	return out
}

var now = time.Date(2026, 10, 13, 6, 44, 0, 0, time.UTC)

func newTracker(t *testing.T) (*operations.Tracker, *recorder, *syncBuffer) {
	t.Helper()
	d := dbtest.Migrated(t)
	pub := &recorder{}
	logs := &syncBuffer{}
	log := slog.New(logging.NewHandler(logs, slog.LevelDebug))
	return operations.NewTracker(operationsrepo.New(d.Pool), pub, log, func() time.Time { return now }), pub, logs
}

func TestOperations(t *testing.T) {
	tracker, pub, logs := newTracker(t)
	ctx := t.Context()

	t.Run("run completes, stores the summary, and sends one event", func(t *testing.T) {
		id, err := tracker.Run(ctx, operations.KindRetention, logging.ComponentRetention,
			func(context.Context) (string, error) { return "deleted 3 rows", nil })
		require.NoError(t, err)
		tracker.Wait()

		op, err := tracker.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, operations.StatusCompleted, op.Status)
		require.Equal(t, "deleted 3 rows", *op.ResultSummary)
		require.Equal(t, now, op.FinishedAt.UTC())
		require.Nil(t, op.ErrorMessage)

		require.Contains(t, pub.all(), ws.OperationCompletedEvent{
			Type: "retention.completed", Time: now, OperationID: id,
			Data: ws.OperationCompletedData{Kind: "retention", Summary: "deleted 3 rows"},
		})
	})

	t.Run("failure is stored, logged once with its component, and sent", func(t *testing.T) {
		before := len(logs.errorEntries(t))
		id, err := tracker.Do(ctx, operations.KindRetention, logging.ComponentRetention,
			func(context.Context) (string, error) { return "", errors.New("disk full") })
		require.NoError(t, err)

		op, err := tracker.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, operations.StatusFailed, op.Status)
		require.Equal(t, "disk full", *op.ErrorMessage)

		entries := logs.errorEntries(t)[before:]
		require.Len(t, entries, 1)
		require.Equal(t, "retention", entries[0]["component"])
		require.EqualValues(t, id, entries[0]["operation_id"])

		require.Contains(t, pub.all(), ws.OperationFailedEvent{
			Type: "retention.failed", Time: now, OperationID: id,
			Data: ws.OperationFailedData{Kind: "retention", Component: "retention", Error: "disk full"},
		})
	})

	t.Run("panic fails the operation", func(t *testing.T) {
		id, err := tracker.Run(ctx, operations.KindRetention, logging.ComponentRetention,
			func(context.Context) (string, error) { panic("kaboom") })
		require.NoError(t, err)
		tracker.Wait()

		op, err := tracker.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, operations.StatusFailed, op.Status)
		require.Equal(t, "panic: kaboom", *op.ErrorMessage)
		last := logs.errorEntries(t)
		require.Contains(t, last[len(last)-1]["stack"], "operations_integration_test.go")
	})

	t.Run("cancelled service context still stores the outcome", func(t *testing.T) {
		runCtx, cancel := context.WithCancel(ctx)
		id, err := tracker.Run(runCtx, operations.KindRetention, logging.ComponentRetention,
			func(ctx context.Context) (string, error) {
				cancel()
				<-ctx.Done()
				return "", ctx.Err()
			})
		require.NoError(t, err)
		tracker.Wait()

		op, err := tracker.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, operations.StatusFailed, op.Status)
	})

	t.Run("finishing twice is refused without a second event", func(t *testing.T) {
		id, err := tracker.Start(ctx, operations.KindRetention)
		require.NoError(t, err)
		require.NoError(t, tracker.Complete(ctx, id, "ok"))
		events := len(pub.all())

		require.ErrorIs(t, tracker.Complete(ctx, id, "again"), operations.ErrNotRunning)
		require.ErrorIs(t, tracker.Fail(ctx, id, logging.ComponentAPI, errors.New("x")), operations.ErrNotRunning)
		require.Len(t, pub.all(), events)
	})

	t.Run("get unknown", func(t *testing.T) {
		_, err := tracker.Get(ctx, 999999)
		require.ErrorIs(t, err, operations.ErrNotFound)
	})
}

func TestFailInterruptedAndList(t *testing.T) {
	tracker, _, _ := newTracker(t)
	ctx := t.Context()

	running, err := tracker.Start(ctx, operations.KindRetention)
	require.NoError(t, err)
	done, err := tracker.Start(ctx, operations.KindRetention)
	require.NoError(t, err)
	require.NoError(t, tracker.Complete(ctx, done, "ok"))

	n, err := tracker.FailInterrupted(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	op, err := tracker.Get(ctx, running)
	require.NoError(t, err)
	require.Equal(t, operations.StatusFailed, op.Status)
	require.Contains(t, *op.ErrorMessage, "interrupted")

	all, err := tracker.List(ctx, operations.ListFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, running, all[0].ID)

	page, err := tracker.List(ctx, operations.ListFilter{AfterID: running, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, done, page[0].ID)

	none, err := tracker.List(ctx, operations.ListFilter{Since: now.Add(time.Second), Limit: 10})
	require.NoError(t, err)
	require.Empty(t, none)
}
