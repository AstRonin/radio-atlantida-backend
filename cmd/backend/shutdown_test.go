package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A request that is still running when the shutdown starts does not hold the
// process: the drain returns within its timeout, and the request is cut off.
func TestDrainHTTPReturnsWithinTheTimeoutWhileARequestIsInFlight(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{})
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release // a handler that ignores its context, the worst case
	})}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ln) }()

	clientDone := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+ln.Addr().String()+"/slow", nil)
		if err != nil {
			clientDone <- err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		clientDone <- err
	}()
	<-started

	began := time.Now()
	cancelled := false
	drainErr := drainHTTP(t.Context(), srv, func() { cancelled = true }, 300*time.Millisecond)
	elapsed := time.Since(began)
	require.Error(t, drainErr, "the request outlived the drain")
	require.Less(t, elapsed, 3*time.Second, "the drain returns at its timeout, not when the request ends")
	require.True(t, cancelled, "the requests are cancelled after the drain times out")
	select {
	case clientErr := <-clientDone:
		require.Error(t, clientErr, "the client's request ends with the connection")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the client connection is closed")
	}
}

// A drain with nothing in flight returns at once with no error.
func TestDrainHTTPWithNothingInFlight(t *testing.T) {
	t.Parallel()
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.NotFoundHandler()}
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ln) }()
	began := time.Now()
	require.NoError(t, drainHTTP(t.Context(), srv, func() {}, 5*time.Second))
	require.Less(t, time.Since(began), 2*time.Second)
}

// awaitStop gives up on components that ignore their context, so the process
// exits; components that return in time report their result.
func TestAwaitStop(t *testing.T) {
	t.Parallel()
	t.Run("components return: their result", func(t *testing.T) {
		t.Parallel()
		waited := make(chan error, 1)
		stopped := make(chan struct{})
		close(stopped)
		want := errors.New("boom")
		waited <- want
		stuck, err := awaitStop(waited, stopped, time.Second)
		require.False(t, stuck)
		require.ErrorIs(t, err, want)
	})
	t.Run("components never return: stuck after the grace", func(t *testing.T) {
		t.Parallel()
		waited := make(chan error, 1)
		stopped := make(chan struct{})
		close(stopped)
		began := time.Now()
		stuck, _ := awaitStop(waited, stopped, 100*time.Millisecond)
		require.True(t, stuck)
		require.Less(t, time.Since(began), 2*time.Second)
	})
}
