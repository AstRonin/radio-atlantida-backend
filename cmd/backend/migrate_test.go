package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/migrations"
)

func TestExitCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want int
	}{
		{errors.New("database unreachable"), 1},
		{fmt.Errorf("%w: missing command", errUsage), 2},
		{fmt.Errorf("%w: needs 1.4.0", errIncompatible), 3},
		{fmt.Errorf("%w: migration 23 failed", migrations.ErrRolledBack), 4},
		{fmt.Errorf("%w at version 23", migrations.ErrDirty), 5},
	}
	for _, tt := range tests {
		require.Equal(t, tt.want, exitCode(tt.err), tt.err.Error())
	}
}

func TestRunMigrateTargetCompat(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	require.NoError(t, run(t.Context(), []string{"migrate", "target-compat"}, &out))

	require.Equal(t, migrations.TargetMinAppVersion+"\n", out.String())
}

func fixedCompat(st migrations.Status) compatReader {
	return func(context.Context) (migrations.Status, error) { return st, nil }
}

func TestWriteCheck(t *testing.T) {
	t.Parallel()
	st := migrations.Status{SchemaVersion: 22, MinAppVersion: "1.4.0"}
	tests := []struct {
		binary string
		want   string
		err    error
	}{
		{"1.4.0", `{"schema_version":22,"dirty":false,"min_app_version":"1.4.0","binary_version":"1.4.0","compatible":true}`, nil},
		{"dev", `{"schema_version":22,"dirty":false,"min_app_version":"1.4.0","binary_version":"dev","compatible":true}`, nil},
		{"1.3.9", `{"schema_version":22,"dirty":false,"min_app_version":"1.4.0","binary_version":"1.3.9","compatible":false}`, errIncompatible},
	}
	for _, tt := range tests {
		t.Run(tt.binary, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer

			err := writeCheck(t.Context(), &out, fixedCompat(st), tt.binary)

			require.ErrorIs(t, err, tt.err)
			require.JSONEq(t, tt.want, out.String())
		})
	}

	// A database that cannot be read prints nothing and is an error (exit 1).
	var out bytes.Buffer
	err := writeCheck(t.Context(), &out, func(context.Context) (migrations.Status, error) {
		return migrations.Status{}, errors.New("connection refused")
	}, "1.0.0")
	require.Error(t, err)
	require.Equal(t, 1, exitCode(err))
	require.Empty(t, out.String())
}

func TestCheckSchema(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	st := migrations.Status{SchemaVersion: 30, MinAppVersion: "1.4.0"}

	checked, err := checkSchema(t.Context(), fixedCompat(st), "1.3.9", log)
	require.True(t, checked)
	require.ErrorIs(t, err, errIncompatible)
	require.Equal(t, 3, exitCode(err))
	require.Contains(t, logs.String(), "database schema needs at least 1.4.0; this backend is 1.3.9")

	checked, err = checkSchema(t.Context(), fixedCompat(st), "1.4.0", log)
	require.True(t, checked)
	require.NoError(t, err)

	checked, err = checkSchema(t.Context(), fixedCompat(st), "dev", log)
	require.True(t, checked)
	require.NoError(t, err)

	checked, err = checkSchema(t.Context(), func(context.Context) (migrations.Status, error) {
		return migrations.Status{}, errors.New("connection refused")
	}, "1.4.0", log)
	require.False(t, checked)
	require.Error(t, err)
}

// A backend that started without the database checks the schema once the
// database answers.
func TestGuardSchemaRetriesUntilTheDatabaseAnswers(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	read := func(context.Context) (migrations.Status, error) {
		if calls.Add(1) < 3 {
			return migrations.Status{}, errors.New("connection refused")
		}
		return migrations.Status{MinAppVersion: "2.0.0"}, nil
	}

	err := guardSchema(t.Context(), read, "1.0.0", slog.New(slog.DiscardHandler), time.Millisecond)

	require.ErrorIs(t, err, errIncompatible)
	require.EqualValues(t, 3, calls.Load())
}

func TestGuardSchemaStopsWithTheService(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	read := func(context.Context) (migrations.Status, error) {
		cancel()
		return migrations.Status{}, errors.New("connection refused")
	}

	require.NoError(t, guardSchema(ctx, read, "1.0.0", slog.New(slog.DiscardHandler), time.Hour))
}
