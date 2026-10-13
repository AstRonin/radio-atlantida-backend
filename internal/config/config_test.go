package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tests in this file change process environment variables, so they do not
// run in parallel (t.Setenv forbids it).

var allVars = []string{
	"DATABASE_URL", "MEDIA_ROOT", "LOG_ROOT", "HTTP_ADDR",
	"ICECAST_SOURCE_PASSWORD", "LIQUIDSOAP_CONTROL_USER", "LIQUIDSOAP_CONTROL_PASSWORD",
}

// clearEnv unsets every variable the config reads and restores it afterwards.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allVars {
		t.Setenv(k, "")
		require.NoError(t, os.Unsetenv(k))
	}
}

func TestLoadMissingListsEveryVariable(t *testing.T) {
	clearEnv(t)

	_, err := Load("")

	require.EqualError(t, err,
		"missing required environment variables: DATABASE_URL, LOG_ROOT, MEDIA_ROOT")
}

func TestLoadEmptyRequiredCountsAsMissing(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MEDIA_ROOT", "")
	t.Setenv("LOG_ROOT", "/logs")

	_, err := Load("")

	require.EqualError(t, err, "missing required environment variables: MEDIA_ROOT")
}

func TestLoadDefaultsAndSecrets(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MEDIA_ROOT", "/media")
	t.Setenv("LOG_ROOT", "/logs")
	t.Setenv("ICECAST_SOURCE_PASSWORD", "s3cret")

	cfg, err := Load("")

	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:8080", cfg.HTTPAddr)
	require.Equal(t, "s3cret", cfg.Secrets.IcecastSourcePassword)
	require.Empty(t, cfg.Secrets.LiquidsoapControlPassword)
}

func TestLoadDotenvDoesNotOverrideEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("MEDIA_ROOT", "/from-env")
	dotenv := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(dotenv, []byte(
		"DATABASE_URL=postgres://dotenv\nMEDIA_ROOT=/from-file\nLOG_ROOT=/logs\n"), 0o600))

	cfg, err := Load(dotenv)

	require.NoError(t, err)
	require.Equal(t, "/from-env", cfg.MediaRoot)
	require.Equal(t, "postgres://dotenv", cfg.DatabaseURL)
}

func TestLoadMissingDotenvIsNotAnError(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MEDIA_ROOT", "/media")
	t.Setenv("LOG_ROOT", "/logs")

	_, err := Load(filepath.Join(t.TempDir(), "absent.env"))

	require.NoError(t, err)
}

func TestSecretsAreNotPrinted(t *testing.T) {
	t.Parallel()
	s := Secrets{IcecastSourcePassword: "s3cret", LiquidsoapControlPassword: "s3cret"}

	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		require.NotContains(t, fmt.Sprintf(format, Config{Secrets: s}), "s3cret", format)
	}
}
