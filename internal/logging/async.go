package logging

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by writes after [AsyncWriter.Close].
var ErrClosed = errors.New("log writer closed")

// AsyncWriter decouples log callers from slow destinations. Write copies the
// entry into a buffered channel and returns at once; one goroutine owns dst
// and drains the channel. When the buffer is full the entry is dropped and
// counted, and one WARN with the drop count is written once the buffer has
// drained. Logging never blocks playout (§10.1).
type AsyncWriter struct {
	ch      chan []byte
	done    chan struct{}
	dropped atomic.Uint64

	// mu guards closed and the send on ch against a concurrent Close.
	mu     sync.RWMutex
	closed bool
}

// NewAsyncWriter starts the drain goroutine. newWarnLogger builds the logger
// used for the drop warning; it writes directly to dst from the drain
// goroutine.
func NewAsyncWriter(dst io.Writer, size int, newWarnLogger func(io.Writer) *slog.Logger) *AsyncWriter {
	w := &AsyncWriter{
		ch:   make(chan []byte, size),
		done: make(chan struct{}),
	}
	go w.drain(dst, newWarnLogger(dst))
	return w
}

// Write queues a copy of p. It never blocks: a full buffer drops the entry.
func (w *AsyncWriter) Write(p []byte) (int, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return 0, ErrClosed
	}
	// slog reuses its buffer after Write returns, so keep a copy.
	entry := make([]byte, len(p))
	copy(entry, p)
	select {
	case w.ch <- entry:
	default:
		w.dropped.Add(1)
	}
	return len(p), nil
}

// Dropped returns the number of entries dropped and not yet reported.
func (w *AsyncWriter) Dropped() uint64 { return w.dropped.Load() }

// Close stops accepting entries and waits until every queued entry is written.
func (w *AsyncWriter) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.ch)
	w.mu.Unlock()
	<-w.done
}

func (w *AsyncWriter) drain(dst io.Writer, warn *slog.Logger) {
	defer close(w.done)
	for entry := range w.ch {
		// A failed write is not retried and never stops the caller; the
		// destination (see teeWriter) still delivers to the other outputs.
		_, _ = dst.Write(entry)
		if len(w.ch) == 0 {
			if n := w.dropped.Swap(0); n > 0 {
				warn.Warn("log entries dropped: buffer full", "dropped", n)
			}
		}
	}
	if n := w.dropped.Swap(0); n > 0 {
		warn.Warn("log entries dropped: buffer full", "dropped", n)
	}
}
