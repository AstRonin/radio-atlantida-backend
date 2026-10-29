//go:build integration

package rotation_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	rotationrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/rotation"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

// The nightly claim is a row per station date: a new service on the same
// database (a restarted backend) sees the claim of the night, and the next
// station day is due again.
func TestNightlyClaimSurvivesRestart(t *testing.T) {
	d := dbtest.Migrated(t)
	ctx := t.Context()
	loc, err := time.LoadLocation("Asia/Yerevan")
	require.NoError(t, err)
	settings := func(context.Context) (rotation.PlanSettings, error) {
		return rotation.PlanSettings{Location: loc, FullPassEnabled: true, FullPassTime: "03:00"}, nil
	}
	// service starts a backend instance on the database at the instant now.
	service := func(now time.Time) *rotation.Service {
		return rotation.NewService(d, rotationrepo.New(d.Pool), nil, nil, settings, nil, nil,
			slog.New(slog.DiscardHandler), func() time.Time { return now })
	}
	night := time.Date(2026, 10, 15, 0, 30, 0, 0, time.UTC) // 04:30 station local, 2026-10-15
	due := func(s *rotation.Service) bool {
		ok, err := s.NightlyDue(ctx)
		require.NoError(t, err)
		return ok
	}

	require.True(t, due(service(night)), "the night is due and takes the claim")
	require.False(t, due(service(night.Add(time.Hour))), "a second process the same night: claimed")
	require.False(t, due(service(night.Add(3*time.Hour))), "a restarted backend the same night: claimed")

	nextNight := night.Add(24 * time.Hour)
	require.True(t, due(service(nextNight)), "the next station day is due again")
	require.False(t, due(service(nextNight.Add(time.Minute))))
}

// ClaimNightly gives a date to one caller only.
func TestClaimNightlyOncePerDate(t *testing.T) {
	d := dbtest.Migrated(t)
	store := rotationrepo.New(d.Pool)
	first, err := store.ClaimNightly(t.Context(), "2026-10-15")
	require.NoError(t, err)
	require.True(t, first)
	again, err := store.ClaimNightly(t.Context(), "2026-10-15")
	require.NoError(t, err)
	require.False(t, again)
	other, err := store.ClaimNightly(t.Context(), "2026-10-16")
	require.NoError(t, err)
	require.True(t, other)
}
