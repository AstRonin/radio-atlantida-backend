package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// componentGrace bounds the wait for the background components after the HTTP
// drain. They return when the service context ends, and the children of the
// media tools end with their contexts (internal/media), so this only bounds a
// component that ignores its context: the process must still exit.
const componentGrace = 10 * time.Second

// drainHTTP shuts the HTTP server down within timeout: in-flight requests get
// that long to finish. Then the requests are cancelled (their queries and the
// media tools they started stop) and the connections are closed. It returns the
// error of the drain, if there was one.
func drainHTTP(parent context.Context, srv *http.Server, cancelRequests func(), timeout time.Duration) error {
	sctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		cancelRequests()
		_ = srv.Close()
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}

// awaitStop waits for the background components: waited delivers their result
// once they have all returned. After stopped closes, it waits at most grace
// more; stuck reports that they did not return by then, so the caller exits
// instead of waiting for them forever.
func awaitStop(waited <-chan error, stopped <-chan struct{}, grace time.Duration) (stuck bool, err error) {
	select {
	case err := <-waited:
		return false, err
	case <-stopped:
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-waited:
		return false, err
	case <-timer.C:
		return true, nil
	}
}
