package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/AstRonin/radio-atlantida-backend/internal/config"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi"
	"github.com/AstRonin/radio-atlantida-backend/internal/jobs"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
	jobsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/jobs"
	libraryrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/library"
	operationsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"
	playrulesrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/playrules"
	retentionrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/retention"
	separationrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/separation"
	settingsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/settings"
	"github.com/AstRonin/radio-atlantida-backend/internal/retention"
	"github.com/AstRonin/radio-atlantida-backend/internal/separation"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
	"github.com/AstRonin/radio-atlantida-backend/internal/workerpool"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// shutdownTimeout bounds the drain of in-flight requests on SIGINT/SIGTERM.
const shutdownTimeout = 15 * time.Second

// Operations started by requests run on a bounded pool (§6.5). One manager
// starts them by hand, so a few workers are plenty; the queue absorbs a
// burst of clicks without a goroutine per click.
const (
	operationWorkers = 4
	operationQueue   = 64
)

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

	settingsSvc, err := settings.NewService(settings.NewRegistry(), settingsrepo.New(database), cfg.Secrets.IsSet(), log, time.Now)
	if err != nil {
		return err
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := database.Pool.Ping(pingCtx); err != nil {
		// Not fatal: /readyz reports the state, and the pool reconnects.
		dbLog.Warn("database not reachable at startup", logging.KeyError, err)
	}
	cancel()

	hub := ws.NewHub(apiLog)
	opsPool := workerpool.New(operationWorkers, operationQueue)
	tracker := operations.NewTracker(operationsrepo.New(database.Pool), hub, opsPool, log, time.Now)
	// Operations run only inside this process: a row still running now was
	// cut off by the previous shutdown or crash.
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if n, err := tracker.FailInterrupted(fctx); err != nil {
		dbLog.Warn("mark interrupted operations failed", logging.KeyError, err)
	} else if n > 0 {
		dbLog.Warn("operations interrupted by the previous shutdown marked failed", "count", n)
	}
	cancel()

	mediaLib := media.New(cfg.MediaRoot)
	prober := media.NewProber(mediaLib)
	if !prober.Available() {
		apiLog.Warn("ffprobe not found on PATH: track metadata is not read from files")
	}
	librarySvc := library.NewService(database,
		libraryrepo.NewTypes(database), libraryrepo.NewPlaylists(database),
		libraryrepo.NewArtists(database), libraryrepo.NewTracks(database),
		mediaLib, prober, log)
	playRulesSvc := playrules.NewService(database, playrulesrepo.New(database.Pool))
	separationSvc := separation.NewService(separationrepo.New(database), separationLimit(settingsSvc))

	applyLogRetention(ctx, settingsSvc, logs, apiLog)
	cleanup := retention.New(retentionrepo.New(database.Pool), retentionPeriods(settingsSvc), time.Now)
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
		Settings:   settingsSvc,
		Library:    librarySvc,
		PlayRules:  playRulesSvc,
		Separation: separationSvc,
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
	// Operations started by requests (queued ones too) finish their final
	// writes before the database closes.
	opsPool.Close()
	if waitErr != nil {
		apiLog.Error("server stopped with error", logging.KeyError, waitErr)
		return waitErr
	}
	apiLog.Info("stopped")
	return nil
}

// applyLogRetention keeps the log files for retention.log_days: the saved
// value now, and every value saved later. If the database is unreachable at
// startup, the rotator keeps its default (equal to the setting's default)
// until the next change.
func applyLogRetention(ctx context.Context, svc *settings.Service, logs *logging.Logging, log *slog.Logger) {
	set := func(days int) {
		if err := logs.SetRetentionDays(days); err != nil {
			log.Warn("apply log retention failed", "days", days, logging.KeyError, err)
		}
	}
	settings.OnChange(svc, settings.RetentionLogDays, set)

	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	days, err := settings.Value(rctx, svc, settings.RetentionLogDays)
	if err != nil {
		log.Warn("read log retention failed, keeping the default", logging.KeyError, err)
		return
	}
	set(days)
}

// retentionPeriods reads the retention periods from the settings before
// each pass. Operations are kept as long as the logs (§6.5).
func retentionPeriods(svc *settings.Service) retention.PeriodsFunc {
	return func(ctx context.Context) (retention.Periods, error) {
		playLog, err := settings.Value(ctx, svc, settings.RetentionPlayLogDays)
		if err != nil {
			return retention.Periods{}, err
		}
		logDays, err := settings.Value(ctx, svc, settings.RetentionLogDays)
		if err != nil {
			return retention.Periods{}, err
		}
		day := 24 * time.Hour
		return retention.Periods{PlayLog: time.Duration(playLog) * day, Operations: time.Duration(logDays) * day}, nil
	}
}

// separationLimit caps separation intervals at the play log retention:
// separation is checked against aired history, which is kept only that long
// (§4.1).
func separationLimit(svc *settings.Service) separation.MaxHoursFunc {
	return func(ctx context.Context) (int, error) {
		days, err := settings.Value(ctx, svc, settings.RetentionPlayLogDays)
		if err != nil {
			return 0, err
		}
		return days * 24, nil
	}
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
