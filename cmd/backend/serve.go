package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/AstRonin/radio-atlantida-backend/internal/config"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

// shutdownTimeout bounds the drain of in-flight requests on SIGINT/SIGTERM.
const shutdownTimeout = 15 * time.Second

// serve runs the HTTP API until ctx is cancelled, then shuts down in order:
// stop accepting requests and drain, close the database pool, flush logs.
func serve(ctx context.Context) error {
	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}

	logs, err := logging.New(cfg.LogRoot)
	if err != nil {
		return fmt.Errorf("start logging: %w", err)
	}
	// Logs are closed last, so every shutdown step can still log.
	defer func() { _ = logs.Close() }()
	log := logs.Logger
	apiLog := logging.For(log, logging.ComponentAPI)
	dbLog := logging.For(log, logging.ComponentDB)

	pool, err := db.OpenPool(ctx, cfg.DatabaseURL)
	if err != nil {
		dbLog.Error("open database pool failed", logging.KeyError, err)
		return err
	}
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := pool.Ping(pingCtx); err != nil {
		// Not fatal: /readyz reports the state, and the pool reconnects.
		dbLog.Warn("database not reachable at startup", logging.KeyError, err)
	}
	cancel()

	// Request contexts are not cancelled by the shutdown signal itself:
	// in-flight requests may finish during the drain. They are cancelled when
	// the drain deadline passes (see below).
	requestsCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	srv := httpapi.NewServer(requestsCtx, cfg.HTTPAddr,
		httpapi.NewRouter(httpapi.Deps{Log: log, DB: pool}), apiLog)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		apiLog.Info("http server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		apiLog.Info("shutting down")
		// The parent context is already cancelled; the drain gets its own.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(gctx), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			// Drain timed out: cancel the remaining requests, so their
			// queries stop and release connections before pool.Close.
			cancelRequests()
			_ = srv.Close()
			return fmt.Errorf("http shutdown: %w", err)
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		apiLog.Error("server stopped with error", logging.KeyError, err)
		return err
	}
	apiLog.Info("stopped")
	return nil
}
