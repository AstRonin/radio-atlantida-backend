package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

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
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
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
	separationrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/separation"
	settingsrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/settings"
	"github.com/AstRonin/radio-atlantida-backend/internal/retention"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
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
	ff := media.NewFFmpeg(mediaLib)
	if !ff.Available() {
		apiLog.Warn("ffmpeg not found on PATH: import, analysis, and transcoding are refused")
	}
	librarySvc := library.NewService(database,
		libraryrepo.NewTypes(database), libraryrepo.NewPlaylists(database),
		libraryrepo.NewArtists(database), libraryrepo.NewTracks(database),
		mediaLib, prober, log)
	playRulesSvc := playrules.NewService(database, playrulesrepo.New(database.Pool))
	separationSvc := separation.NewService(separationrepo.New(database), separationLimit(settingsSvc))
	gridSvc := grid.NewService(database, gridrepo.New(database), gridSettings(settingsSvc), time.Now)
	clocksSvc := clocks.NewService(database, clocksrepo.New(database), clocksrepo.NewCheck(database.Pool),
		gridSvc, clockSettings(settingsSvc), time.Now)
	rotationSvc := rotation.NewService(database, rotationrepo.New(database.Pool), clocksSvc, gridSvc,
		rotationSettings(settingsSvc), tracker, hub, log, time.Now)
	playoutSvc := newPlayout(database, settingsSvc, hub, log)
	ingestSvc := ingest.NewService(database, ingestrepo.New(database.Pool), mediaLib, prober, ff, tracker,
		ingestSettings(settingsSvc), log, time.Now)
	outageStore := outagerepo.New(database.Pool)
	outageSvc := outage.NewService(outageStore, time.Now)
	detector := outage.NewDetector(database, outageStore, playoutSvc,
		outage.NewIcecastChecker(&http.Client{}), outage.NewJournal(cfg.PlayoutJournalRoot, playoutSvc, log),
		tracker, hub, outageSettings(settingsSvc), log, time.Now)

	bridge := live.NewBridge(live.Config{UDPAddr: cfg.LiveUDPAddr, PublicIPs: cfg.LivePublicIPs}, live.DefaultTimeouts,
		harborTarget(settingsSvc, cfg.Secrets), hub, log, time.Now)

	applyLogRetention(ctx, settingsSvc, logs, apiLog)
	cleanup := retention.New(retentionrepo.New(database.Pool), retentionPeriods(settingsSvc), time.Now)
	scheduler := jobs.NewScheduler(jobsrepo.NewLocks(database.Pool), tracker, log,
		jobs.Retention(cleanup.Run), jobs.TrackPlan(rotationSvc.RunJob))

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
		Clocks:     clocksSvc,
		Grid:       gridSvc,
		Rotation:   rotationSvc,
		Playout:    playoutSvc,
		Outages:    outageSvc,
		Ingest:     ingestSvc,
		Live:       bridge,
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

// newPlayout wires the Liquidsoap control client and playout. The control
// address is read on every connect; a change of it drops the connection, so
// the next command connects to the new address (apply mode reconnect).
func newPlayout(database *db.DB, svc *settings.Service, hub *ws.Hub, log *slog.Logger) *playout.Service {
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
	p := playout.NewService(database, playoutrepo.New(database.Pool), ctl, playoutSettings(svc), hub, log, time.Now, rnd)
	settings.OnChange(svc, settings.LiquidsoapMusicGain, p.MusicGainSettingChanged)
	// The pass reads the duck gain from the settings; wake it to apply a
	// change at once.
	settings.OnChange(svc, settings.LiquidsoapDuckGain, func(float64) { p.Wake() })
	return p
}

// harborTarget reads where Liquidsoap's live input listens: the control
// host (the same Liquidsoap) and the harbor port. The script takes the
// control credentials as the harbor's source credentials.
func harborTarget(svc *settings.Service, secrets config.Secrets) live.HarborFunc {
	return func(ctx context.Context) (live.HarborTarget, error) {
		host, err := settings.Value(ctx, svc, settings.LiquidsoapControlHost)
		if err != nil {
			return live.HarborTarget{}, err
		}
		port, err := settings.Value(ctx, svc, settings.LiquidsoapHarborPort)
		if err != nil {
			return live.HarborTarget{}, err
		}
		return live.HarborTarget{
			Host: host, Port: port, User: secrets.LiquidsoapControlUser, Password: secrets.LiquidsoapControlPassword,
		}, nil
	}
}

// controlAddr reads the Liquidsoap control address from the settings.
func controlAddr(svc *settings.Service) liquidsoap.AddrFunc {
	return func(ctx context.Context) (string, error) {
		host, err := settings.Value(ctx, svc, settings.LiquidsoapControlHost)
		if err != nil {
			return "", err
		}
		port, err := settings.Value(ctx, svc, settings.LiquidsoapControlPort)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(host, strconv.Itoa(port)), nil
	}
}

// playoutSettings reads the settings of playout on every pass, so a change
// applies at once (§6.3).
func playoutSettings(svc *settings.Service) playout.SettingsFunc {
	return func(ctx context.Context) (playout.Settings, error) {
		tz, err := settings.Value(ctx, svc, settings.StationTimezone)
		if err != nil {
			return playout.Settings{}, err
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return playout.Settings{}, fmt.Errorf("station timezone %q: %w", tz, err)
		}
		lookahead, err := settings.Value(ctx, svc, settings.RotationLookaheadItems)
		if err != nil {
			return playout.Settings{}, err
		}
		prepush, err := settings.Value(ctx, svc, settings.PlayoutPrepushMinutes)
		if err != nil {
			return playout.Settings{}, err
		}
		fade, err := settings.Value(ctx, svc, settings.LiquidsoapFadeOutMS)
		if err != nil {
			return playout.Settings{}, err
		}
		root, err := settings.Value(ctx, svc, settings.LiquidsoapMediaRoot)
		if err != nil {
			return playout.Settings{}, err
		}
		gain, err := settings.Value(ctx, svc, settings.LiquidsoapMusicGain)
		if err != nil {
			return playout.Settings{}, err
		}
		duck, err := settings.Value(ctx, svc, settings.LiquidsoapDuckGain)
		if err != nil {
			return playout.Settings{}, err
		}
		emergency, err := settings.Value(ctx, svc, settings.PlayoutEmergencyPlaylistID)
		if err != nil {
			return playout.Settings{}, err
		}
		return playout.Settings{
			Location: loc, Lookahead: lookahead, Prepush: time.Duration(prepush) * time.Minute, FadeOutMS: fade,
			MediaRoot: root, MusicGain: gain, DuckGain: duck, EmergencyPlaylistID: emergency,
		}, nil
	}
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

// gridSettings reads the station timezone and the grid horizon for each
// grid request, so a change applies at once (§6.3).
func gridSettings(svc *settings.Service) grid.SettingsFunc {
	return func(ctx context.Context) (grid.Settings, error) {
		tz, err := settings.Value(ctx, svc, settings.StationTimezone)
		if err != nil {
			return grid.Settings{}, err
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return grid.Settings{}, fmt.Errorf("station timezone %q: %w", tz, err)
		}
		days, err := settings.Value(ctx, svc, settings.RotationGridHorizonDays)
		if err != nil {
			return grid.Settings{}, err
		}
		return grid.Settings{Location: loc, HorizonDays: days}, nil
	}
}

// clockSettings reads the tolerance defaults of new elements and the grid
// horizon of the dry check.
func clockSettings(svc *settings.Service) clocks.SettingsFunc {
	return func(ctx context.Context) (clocks.Settings, error) {
		dur, err := settings.Value(ctx, svc, settings.RotationDurationToleranceMS)
		if err != nil {
			return clocks.Settings{}, err
		}
		fixed, err := settings.Value(ctx, svc, settings.RotationFixedToleranceMS)
		if err != nil {
			return clocks.Settings{}, err
		}
		days, err := settings.Value(ctx, svc, settings.RotationGridHorizonDays)
		if err != nil {
			return clocks.Settings{}, err
		}
		return clocks.Settings{DurationToleranceMS: dur, FixedToleranceMS: fixed, GridHorizonDays: days}, nil
	}
}

// rotationSettings reads the settings of the planner before each sync, so a
// change applies on the next build (§6.3).
func rotationSettings(svc *settings.Service) rotation.SettingsFunc {
	return func(ctx context.Context) (rotation.PlanSettings, error) {
		tz, err := settings.Value(ctx, svc, settings.StationTimezone)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return rotation.PlanSettings{}, fmt.Errorf("station timezone %q: %w", tz, err)
		}
		horizon, err := settings.Value(ctx, svc, settings.RotationPlanHorizonHours)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		deadline, err := settings.Value(ctx, svc, settings.FillerReminderDeadlineMinutes)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		fallback, err := settings.Value(ctx, svc, settings.FillerFallbackKey)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		minGap, err := settings.Value(ctx, svc, settings.RotationMinFillerGapMS)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		relaxed, err := settings.Value(ctx, svc, settings.RotationMaxRelaxedToleranceMS)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		prepush, err := settings.Value(ctx, svc, settings.PlayoutPrepushMinutes)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		fixedTol, err := settings.Value(ctx, svc, settings.RotationFixedToleranceMS)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		return rotation.PlanSettings{
			Location:                loc,
			PlanHorizonHours:        horizon,
			ReminderDeadline:        time.Duration(deadline) * time.Minute,
			Fallback:                clocks.Filler{Mode: clocks.FillerMode(fallback.Mode), SourceID: fallback.SourceID},
			Freeze:                  time.Duration(prepush) * time.Minute,
			DefaultFixedToleranceMS: fixedTol,
			Build:                   rotation.Settings{MinFillerGapMS: minGap, MaxRelaxedToleranceMS: relaxed},
		}, nil
	}
}

// outageSettings reads the detection settings on every evaluation, so a
// change applies at once (§6.3).
func outageSettings(svc *settings.Service) outage.SettingsFunc {
	return func(ctx context.Context) (outage.Settings, error) {
		timeout, err := settings.Value(ctx, svc, settings.OutageHeartbeatTimeoutSeconds)
		if err != nil {
			return outage.Settings{}, err
		}
		interval, err := settings.Value(ctx, svc, settings.OutageIcecastCheckSeconds)
		if err != nil {
			return outage.Settings{}, err
		}
		checks, err := settings.Value(ctx, svc, settings.OutageFailedChecks)
		if err != nil {
			return outage.Settings{}, err
		}
		prepush, err := settings.Value(ctx, svc, settings.PlayoutPrepushMinutes)
		if err != nil {
			return outage.Settings{}, err
		}
		host, err := settings.Value(ctx, svc, settings.StreamIcecastHost)
		if err != nil {
			return outage.Settings{}, err
		}
		port, err := settings.Value(ctx, svc, settings.StreamIcecastPort)
		if err != nil {
			return outage.Settings{}, err
		}
		mount, err := settings.Value(ctx, svc, settings.StreamMount)
		if err != nil {
			return outage.Settings{}, err
		}
		return outage.Settings{
			HeartbeatTimeout: time.Duration(timeout) * time.Second,
			IcecastInterval:  time.Duration(interval) * time.Second,
			FailedChecks:     checks,
			Prepush:          time.Duration(prepush) * time.Minute,
			Icecast:          outage.IcecastTarget{Host: host, Port: port, Mount: mount},
		}, nil
	}
}

// ingestSettings reads the import settings when an operation starts
// (docs/phase-11-plan.md §2, decision 11). The loudness target is the
// station's one, stream.loudness_target_lufs.
func ingestSettings(svc *settings.Service) ingest.SettingsFunc {
	return func(ctx context.Context) (ingest.Settings, error) {
		inbox, err := settings.Value(ctx, svc, settings.ImportInboxDir)
		if err != nil {
			return ingest.Settings{}, err
		}
		lib, err := settings.Value(ctx, svc, settings.ImportLibraryDir)
		if err != nil {
			return ingest.Settings{}, err
		}
		codec, err := settings.Value(ctx, svc, settings.ImportTargetCodec)
		if err != nil {
			return ingest.Settings{}, err
		}
		bitrate, err := settings.Value(ctx, svc, settings.ImportTargetBitrateKbps)
		if err != nil {
			return ingest.Settings{}, err
		}
		rate, err := settings.Value(ctx, svc, settings.ImportTargetSampleRateHz)
		if err != nil {
			return ingest.Settings{}, err
		}
		channels, err := settings.Value(ctx, svc, settings.ImportTargetChannels)
		if err != nil {
			return ingest.Settings{}, err
		}
		minBitrate, err := settings.Value(ctx, svc, settings.ImportMinBitrateKbps)
		if err != nil {
			return ingest.Settings{}, err
		}
		loudness, err := settings.Value(ctx, svc, settings.StreamLoudnessLUFS)
		if err != nil {
			return ingest.Settings{}, err
		}
		tolerance, err := settings.Value(ctx, svc, settings.ImportLoudnessToleranceLU)
		if err != nil {
			return ingest.Settings{}, err
		}
		peak, err := settings.Value(ctx, svc, settings.ImportMaxTruePeakDBTP)
		if err != nil {
			return ingest.Settings{}, err
		}
		prepush, err := settings.Value(ctx, svc, settings.PlayoutPrepushMinutes)
		if err != nil {
			return ingest.Settings{}, err
		}
		return ingest.Settings{
			InboxDir: inbox, LibraryDir: lib,
			Standard: ingest.Standard{
				Format: ingest.StandardFormat(codec), Codec: codec, BitrateKbps: bitrate, SampleRate: rate, Channels: channels,
				MinBitrateKbps: minBitrate, LoudnessLUFS: loudness, LoudnessToleranceLU: tolerance, MaxTruePeakDBTP: peak,
			},
			// The pusher takes items prepush minutes ahead; five more
			// minutes cover a long encode.
			BusyAhead: time.Duration(prepush+5) * time.Minute,
		}, nil
	}
}
