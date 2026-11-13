package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/AstRonin/radio-atlantida-backend/internal/airwindow"
	"github.com/AstRonin/radio-atlantida-backend/internal/broadcast"
	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/config"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi"
	"github.com/AstRonin/radio-atlantida-backend/internal/ingest"
	"github.com/AstRonin/radio-atlantida-backend/internal/jobs"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
	"github.com/AstRonin/radio-atlantida-backend/internal/live"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/logsearch"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/migrations"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
	airwindowrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/airwindow"
	broadcastrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/broadcast"
	clocksrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/clocks"
	gridrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/grid"
	ingestrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/ingest"
	jobsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/jobs"
	libraryrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/library"
	operationsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/operations"
	outagerepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/outage"
	playoutrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/playout"
	playrulesrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/playrules"
	retentionrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/retention"
	rotationrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/rotation"
	settingsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/settings"
	"github.com/AstRonin/radio-atlantida-backend/internal/retention"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
	"github.com/AstRonin/radio-atlantida-backend/internal/workerpool"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// compatRetry is how often the startup schema check is retried while the
// database is unreachable.
const compatRetry = 5 * time.Second

// shutdownTimeout bounds the drain of in-flight requests on SIGINT/SIGTERM.
const shutdownTimeout = 15 * time.Second

// Operations started by requests run on a bounded pool (§6.5). One manager
// starts them by hand, so a few workers are plenty; the queue absorbs a
// burst of clicks without a goroutine per click.
const (
	operationWorkers = 10
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

	logs := logging.New(cfg.LokiURL)
	// Logs are closed last, so every shutdown step can still log.
	defer logs.Close(ctx)
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
	pingErr := database.Pool.Ping(pingCtx)
	if pingErr != nil {
		// Not fatal: /readyz reports the state, and the pool reconnects.
		dbLog.Warn("database not reachable at startup", logging.KeyError, pingErr)
	}
	cancel()

	// Downgrade guard (design §9.2): a release below the schema's
	// min_app_version refuses to start. Without the database the check runs
	// in the background at the first successful connection (guardSchema).
	readCompat := func(ctx context.Context) (migrations.Status, error) {
		return migrations.Compat(ctx, cfg.DatabaseURL)
	}
	schemaChecked := false
	if pingErr == nil {
		var err error
		schemaChecked, err = checkSchema(ctx, readCompat, version, dbLog)
		if err != nil && schemaChecked {
			return err
		}
	}

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
	ff := media.NewFFmpeg(mediaLib)
	if !ff.Available() {
		apiLog.Warn("ffmpeg not found on PATH: import, analysis, and transcoding are refused")
	}
	playRulesSvc := playrules.NewService(database, playrulesrepo.New(database.Pool), intervalLimit(settingsSvc))
	playRulesSvc.GuardRetention(settingsSvc)
	checkEncoderLimits(settingsSvc)
	gridSvc := grid.NewService(database, gridrepo.New(database), gridSettings(settingsSvc), time.Now)
	clocksSvc := clocks.NewService(database, clocksrepo.New(database), clocksrepo.NewCheck(database.Pool),
		gridSvc, clockSettings(settingsSvc), time.Now)
	rotationSvc := rotation.NewService(database, rotationrepo.New(database.Pool), clocksSvc, gridSvc,
		rotationSettings(settingsSvc), tracker, hub, log, time.Now)
	// The broadcast state follows Liquidsoap's output reports (playout) and
	// the outage detector's checks and outages.
	broadcastState := broadcast.NewTracker(database, broadcastrepo.New(database.Pool), hub, log, time.Now)
	playoutSvc := newPlayout(database, settingsSvc, hub, broadcastState, log)
	// The library refuses a cue change of the track on air (playout) and
	// the delete of the emergency playlist (settings).
	librarySvc := library.NewService(database,
		libraryrepo.NewTypes(database), libraryrepo.NewPlaylists(database),
		libraryrepo.NewArtists(database), libraryrepo.NewTracks(database),
		mediaLib, prober, playoutSvc, emergencySetting{settingsSvc}, log)
	airWindowSvc := airwindow.NewService(airwindowrepo.New(database.Pool), playoutSvc, airWindowSettings(settingsSvc), time.Now)
	ingestSvc := ingest.NewService(database, ingestrepo.New(database.Pool), librarySvc, mediaLib, prober, ff, tracker,
		ingestSettings(settingsSvc), log, time.Now)
	outageStore := outagerepo.New(database.Pool)
	outageSvc := outage.NewService(outageStore, time.Now)
	detector := outage.NewDetector(database, outageStore, playoutSvc,
		outage.NewIcecastChecker(&http.Client{}), outage.NewJournal(cfg.PlayoutJournalRoot, playoutSvc, log),
		tracker, hub, broadcastState, outageSettings(settingsSvc), log, time.Now)

	bridge := live.NewBridge(live.Config{UDPAddr: cfg.LiveUDPAddr, PublicIPs: cfg.LivePublicIPs}, live.DefaultTimeouts,
		harborTarget(settingsSvc, cfg.Secrets), hub, log, time.Now)

	cleanup := retention.New(retentionrepo.New(database.Pool), retentionPeriods(settingsSvc), time.Now)
	scheduler := jobs.NewScheduler(jobsrepo.NewLocks(database.Pool), tracker, log,
		jobs.Retention(cleanup.Run), jobs.TrackPlan(rotationSvc.RunJob),
		jobs.TrackPlanFull(rotationSvc.RunNightly, rotationSvc.NightlyDue, rotation.FullPassTimeout))

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
		LogSearch:  logsearch.NewService(logsearch.NewClient(cfg.LokiURL, &http.Client{}), logging.For(log, logging.ComponentAPI)),
		Settings:   settingsSvc,
		Library:    librarySvc,
		PlayRules:  playRulesSvc,
		Clocks:     clocksSvc,
		Grid:       gridSvc,
		Rotation:   rotationSvc,
		Playout:    playoutSvc,
		Outages:    outageSvc,
		Ingest:     ingestSvc,
		Live:       bridge,
		Broadcast:  broadcastState,
		AirWindow:  airWindowSvc,
		Now:        time.Now,
	}), apiLog)
	// Shutdown does not wait for hijacked connections: close the panels'
	// WebSockets when it starts.
	srv.RegisterOnShutdown(hub.Close)

	g, gctx := errgroup.WithContext(ctx)
	if !schemaChecked {
		// An incompatible schema stops the service like a failed component.
		g.Go(func() error { return guardSchema(gctx, readCompat, version, dbLog, compatRetry) })
	}
	g.Go(func() error {
		forwardAlerts(gctx, logs.Alerts(), hub)
		return nil
	})
	g.Go(func() error { return scheduler.Run(gctx) })
	// The log sink pushes the lines to Loki and flushes them on shutdown
	// (logs-loki-plan §4.1). It returns at once when LOKI_URL is empty.
	g.Go(func() error { return logs.RunPush(gctx) })
	// The planner runs the clock_build operations started by requests.
	g.Go(func() error { return rotationSvc.Run(gctx) })
	// The pusher keeps Liquidsoap's hour queues filled (§8.1).
	g.Go(func() error { return playoutSvc.Run(gctx) })
	// The outage detector watches Icecast, the callbacks, and the control
	// link (§7.1); at startup it reconciles the journal.
	g.Go(func() error { return detector.Run(gctx) })
	// The media worker runs imports, analyses, and transcodes one at a
	// time.
	g.Go(func() error { return ingestSvc.Run(gctx) })
	// The live voice bridge relays the manager's microphone to the harbor
	// (§8.4); on shutdown it ends the session.
	g.Go(func() error { return bridge.Run(gctx) })
	g.Go(func() error {
		apiLog.Info("http server listening", "addr", cfg.HTTPAddr, "version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		apiLog.Info("shutting down")
		// The parent context is already cancelled; the drain gets its own.
		return drainHTTP(context.WithoutCancel(gctx), srv, cancelRequests, shutdownTimeout)
	})

	// Operations started by requests (queued ones too) finish their final
	// writes before the database closes. The wait is bounded: a component that
	// ignores its context must not keep the process alive (cmd/backend/shutdown.go).
	waited := make(chan error, 1)
	go func() {
		err := g.Wait()
		opsPool.Close()
		waited <- err
	}()
	stuck, waitErr := awaitStop(waited, gctx.Done(), shutdownTimeout+componentGrace)
	if stuck {
		apiLog.Error("background components did not stop in time; exiting",
			"grace", (shutdownTimeout + componentGrace).String())
		return fmt.Errorf("components did not stop within %s", shutdownTimeout+componentGrace)
	}
	if waitErr != nil {
		apiLog.Error("server stopped with error", logging.KeyError, waitErr)
		return waitErr
	}
	apiLog.Info("stopped")
	return nil
}

// newPlayout wires the Liquidsoap control client and playout. The control
// address is read on every connect; a change of it drops the connection, so
// the next command connects to the new address (apply mode reconnect).
func newPlayout(database *db.DB, svc *settings.Service, hub *ws.Hub, outputs playout.OutputObserver,
	log *slog.Logger,
) *playout.Service {
	lsLog := logging.For(log, logging.ComponentLiquidsoap)
	ctl := liquidsoap.New(controlAddr(svc), time.Now, func(st liquidsoap.State) {
		if st.Connected {
			lsLog.Info("liquidsoap control link up")
		} else {
			lsLog.Warn("liquidsoap control link down")
		}
		hub.Publish(ws.LiquidsoapStateEvent{
			Type: ws.EventLiquidsoapState, Time: st.Since, Data: ws.LiquidsoapStateData{Connected: st.Connected},
		})
	})
	settings.OnChange(svc, settings.LiquidsoapControlHost, func(string) { ctl.Reset() })
	settings.OnChange(svc, settings.LiquidsoapControlPort, func(int) { ctl.Reset() })

	// Emergency picks need no reproducibility; the seed only varies them.
	rnd := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0)) //nolint:gosec // G404: not for security; G115: positive
	p := playout.NewService(database, playoutrepo.New(database.Pool), ctl, playoutSettings(svc), hub, outputs, log, time.Now, rnd)
	settings.OnChange(svc, settings.LiquidsoapMusicGain, p.MusicGainSettingChanged)
	// The pass reads the duck gain from the settings; wake it to apply a
	// change at once.
	settings.OnChange(svc, settings.LiquidsoapDuckGain, func(float64) { p.Wake() })
	// The same for the stream output (§6.3: a reconnect, not a restart):
	// the pass sends output.set when the settings differ from what
	// Liquidsoap last accepted.
	svc.Subscribe(wakeOnStreamChange(p.Wake))
	return p
}

// wakeOnStreamChange returns a settings subscriber that calls wake once
// for a save that carries any stream.* setting, so a batch of several
// stream settings gives one pass and one output.set.
func wakeOnStreamChange(wake func()) func([]settings.Change) {
	return func(changes []settings.Change) {
		for _, c := range changes {
			if strings.HasPrefix(c.Key, string(settings.GroupStream)+".") {
				wake()
				return
			}
		}
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
