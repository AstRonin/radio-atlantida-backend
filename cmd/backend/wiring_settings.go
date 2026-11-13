package main

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/airwindow"
	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/config"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/ingest"
	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
	"github.com/AstRonin/radio-atlantida-backend/internal/live"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
	"github.com/AstRonin/radio-atlantida-backend/internal/retention"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
)

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
		heartbeat, err := settings.Value(ctx, svc, settings.OutageHeartbeatTimeoutSeconds)
		if err != nil {
			return playout.Settings{}, err
		}
		output, err := streamOutput(ctx, svc)
		if err != nil {
			return playout.Settings{}, err
		}
		return playout.Settings{
			Location: loc, Lookahead: lookahead, Prepush: time.Duration(prepush) * time.Minute, FadeOutMS: fade,
			MediaRoot: root, MusicGain: gain, DuckGain: duck, EmergencyPlaylistID: emergency,
			HeartbeatTimeout: time.Duration(heartbeat) * time.Second, Output: output,
		}, nil
	}
}

// streamOutput reads the stream output settings (§8.8) that Liquidsoap
// applies with output.set. The loudness target and HLS are not part of it
// (not applied to the output).
func streamOutput(ctx context.Context, svc *settings.Service) (playout.StreamOutput, error) {
	var (
		o   playout.StreamOutput
		err error
	)
	strs := []struct {
		key settings.Key[string]
		dst *string
	}{
		{settings.StreamIcecastHost, &o.Host},
		{settings.StreamMount, &o.Mount},
		{settings.StreamCodec, &o.Codec},
		{settings.StreamName, &o.Name},
		{settings.StreamDescription, &o.Description},
		{settings.StreamGenre, &o.Genre},
	}
	for _, f := range strs {
		if *f.dst, err = settings.Value(ctx, svc, f.key); err != nil {
			return playout.StreamOutput{}, err
		}
	}
	ints := []struct {
		key settings.Key[int]
		dst *int
	}{
		{settings.StreamIcecastPort, &o.Port},
		{settings.StreamBitrateKbps, &o.BitrateKbps},
		{settings.StreamSampleRateHz, &o.SampleRateHz},
		{settings.StreamChannels, &o.Channels},
	}
	for _, f := range ints {
		if *f.dst, err = settings.Value(ctx, svc, f.key); err != nil {
			return playout.StreamOutput{}, err
		}
	}
	if o.TLS, err = settings.Value(ctx, svc, settings.StreamIcecastTLS); err != nil {
		return playout.StreamOutput{}, err
	}
	return o, nil
}

// operationsRetentionDays keeps finished operations this many days. Until the
// log files were removed, operations followed their retention setting, whose
// default was this period (docs/logs-loki-plan.md §4.4); the period is kept.
const operationsRetentionDays = 10

// retentionPeriods reads the play log period from the settings before each
// pass; operations are kept for operationsRetentionDays (§6.5).
func retentionPeriods(svc *settings.Service) retention.PeriodsFunc {
	return func(ctx context.Context) (retention.Periods, error) {
		playLog, err := settings.Value(ctx, svc, settings.RetentionPlayLogDays)
		if err != nil {
			return retention.Periods{}, err
		}
		day := 24 * time.Hour
		return retention.Periods{PlayLog: time.Duration(playLog) * day, Operations: operationsRetentionDays * day}, nil
	}
}

// intervalLimit caps the intervals of play rules at the play log
// retention: they are checked against aired history, which is kept only
// that long (§4.1).
func intervalLimit(svc *settings.Service) playrules.MaxIntervalFunc {
	return func(ctx context.Context) (int, error) {
		days, err := settings.Value(ctx, svc, settings.RetentionPlayLogDays)
		if err != nil {
			return 0, err
		}
		return days * 24 * 60, nil
	}
}

// emergencySetting reads the emergency playlist for the library on each
// delete, so a change applies at once.
type emergencySetting struct{ svc *settings.Service }

// EmergencyPlaylistID implements library.EmergencySetting.
func (e emergencySetting) EmergencyPlaylistID(ctx context.Context) (*uint64, error) {
	return settings.Value(ctx, e.svc, settings.PlayoutEmergencyPlaylistID)
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

// clockSettings reads the default tolerance of new inserts and the grid
// horizon of the dry check.
func clockSettings(svc *settings.Service) clocks.SettingsFunc {
	return func(ctx context.Context) (clocks.Settings, error) {
		fixed, err := settings.Value(ctx, svc, settings.RotationFixedToleranceMS)
		if err != nil {
			return clocks.Settings{}, err
		}
		days, err := settings.Value(ctx, svc, settings.RotationGridHorizonDays)
		if err != nil {
			return clocks.Settings{}, err
		}
		return clocks.Settings{FixedToleranceMS: fixed, GridHorizonDays: days}, nil
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
		planDays, err := settings.Value(ctx, svc, settings.RotationPlanHorizonDays)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		gridDays, err := settings.Value(ctx, svc, settings.RotationGridHorizonDays)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		fullEnabled, err := settings.Value(ctx, svc, settings.RotationFullPassEnabled)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		fullTime, err := settings.Value(ctx, svc, settings.RotationFullPassTime)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		fullBatch, err := settings.Value(ctx, svc, settings.RotationFullPassBatchHours)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		fullSlow, err := settings.Value(ctx, svc, settings.RotationFullPassSlowMS)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		fullHeap, err := settings.Value(ctx, svc, settings.RotationFullPassHeapWarnMB)
		if err != nil {
			return rotation.PlanSettings{}, err
		}
		minGap, err := settings.Value(ctx, svc, settings.RotationMinGapMS)
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
			PlanHorizonDays:         planDays,
			GridHorizonDays:         gridDays,
			FullPassEnabled:         fullEnabled,
			FullPassTime:            fullTime,
			FullBatchHours:          fullBatch,
			FullSlowMS:              fullSlow,
			FullHeapWarnMB:          fullHeap,
			Freeze:                  time.Duration(prepush) * time.Minute,
			DefaultFixedToleranceMS: fixedTol,
			Build:                   rotation.Settings{MinGapMS: minGap},
		}, nil
	}
}

// airWindowSettings reads the counts of the air window on every request,
// so a change applies to the next one (docs/phase-17-plan.md §3).
func airWindowSettings(svc *settings.Service) airwindow.SettingsFunc {
	return func(ctx context.Context) (airwindow.Settings, error) {
		played, err := settings.Value(ctx, svc, settings.AirPlayedCount)
		if err != nil {
			return airwindow.Settings{}, err
		}
		queued, err := settings.Value(ctx, svc, settings.AirQueuedCount)
		if err != nil {
			return airwindow.Settings{}, err
		}
		return airwindow.Settings{PlayedCount: played, QueuedCount: queued}, nil
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
		useTLS, err := settings.Value(ctx, svc, settings.StreamIcecastTLS)
		if err != nil {
			return outage.Settings{}, err
		}
		return outage.Settings{
			HeartbeatTimeout: time.Duration(timeout) * time.Second,
			IcecastInterval:  time.Duration(interval) * time.Second,
			FailedChecks:     checks,
			Prepush:          time.Duration(prepush) * time.Minute,
			Icecast:          outage.IcecastTarget{Host: host, Port: port, Mount: mount, TLS: useTLS},
		}, nil
	}
}

// ingestSettings reads the import settings when an operation starts
// (docs/phase-11-plan.md §2, decision 11; the folders of decision 11 are
// gone with docs/phase-18-plan.md). The loudness target is the
// station's one, stream.loudness_target_lufs.
func ingestSettings(svc *settings.Service) ingest.SettingsFunc {
	return func(ctx context.Context) (ingest.Settings, error) {
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
