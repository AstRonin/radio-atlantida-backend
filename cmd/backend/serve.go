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
	"github.com/AstRonin/radio-atlantida-backend/internal/jobs"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	jobsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/jobs"
	operationsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"
	retentionrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/retention"
	"github.com/AstRonin/radio-atlantida-backend/internal/retention"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// shutdownTimeout bounds the drain of in-flight requests on SIGINT/SIGTERM.
const shutdownTimeout = 15 * time.Second

// serve runs the HTTP API, the WebSocket hub, and the background jobs until
// ctx is cancelled, then shuts down in order: stop accepting requests and
// drain (closing the WebSockets), stop the jobs, wait for running
// operations, close the database, flush logs.
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

	database, err := db.Open(ctx, cfg.DatabaseURL, dbLog)
	if err != nil {
		dbLog.Error("open database failed", logging.KeyError, err)
		return err
	}
	defer database.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := database.Pool.Ping(pingCtx); err != nil {
		// Not fatal: /readyz reports the state, and the pool reconnects.
		dbLog.Warn("database not reachable at startup", logging.KeyError, err)
	}
	cancel()

	hub := ws.NewHub(apiLog)
	tracker := operations.NewTracker(operationsrepo.New(database.Pool), hub, log, time.Now)
	// Operations run only inside this process: a row still running now was
	// cut off by the previous shutdown or crash.
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if n, err := tracker.FailInterrupted(fctx); err != nil {
		dbLog.Warn("mark interrupted operations failed", logging.KeyError, err)
	} else if n > 0 {
		dbLog.Warn("operations interrupted by the previous shutdown marked failed", "count", n)
	}
	cancel()
	cleanup := retention.New(retentionrepo.New(database.Pool), time.Now)
	scheduler := jobs.NewScheduler(jobsrepo.NewLocks(database.Pool), tracker, log, jobs.Retention(cleanup.Run))

	// Request contexts are not cancelled by the shutdown signal itself:
	// in-flight requests may finish during the drain. They are cancelled when
	// the drain deadline passes (see below).
	requestsCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	srv := httpapi.NewServer(requestsCtx, cfg.HTTPAddr, httpapi.NewRouter(httpapi.Deps{
		Log:        log,
		DB:         database.Pool,
		Version:    version,
		Events:     hub,
		Operations: tracker,
		Logs:       logging.NewFileReader(cfg.LogRoot),
		Now:        time.Now,
	}), apiLog)
	// Shutdown does not wait for hijacked connections: close the panels'
	// WebSockets when it starts.
	srv.RegisterOnShutdown(hub.Close)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		forwardAlerts(gctx, logs.Alerts(), hub)
		return nil
	})
	g.Go(func() error { return scheduler.Run(gctx) })
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
			// queries stop and release connections before the database closes.
			cancelRequests()
			_ = srv.Close()
			return fmt.Errorf("http shutdown: %w", err)
		}
		return nil
	})

	waitErr := g.Wait()
	// Operations started by requests finish their final writes before the
	// database closes.
	tracker.Wait()
	if waitErr != nil {
		apiLog.Error("server stopped with error", logging.KeyError, waitErr)
		return waitErr
	}
	apiLog.Info("stopped")
	return nil
}

// forwardAlerts pushes ERROR log entries to the panels as error.alert events
// (§10.1) until ctx ends.
func forwardAlerts(ctx context.Context, alerts <-chan logging.Alert, hub *ws.Hub) {
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-alerts:
			hub.Publish(ws.NewErrorAlert(a))
		}
	}
}
