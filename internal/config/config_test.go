package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Tests in this file change process environment variables, so they do not
// run in parallel (t.Setenv forbids it).

var allVars = []string{
	"DATABASE_URL", "MEDIA_ROOT", "LOG_ROOT", "PLAYOUT_JOURNAL_ROOT", "HTTP_ADDR", "LIVE_UDP_ADDR", "LIVE_PUBLIC_IPS",
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
		"missing required environment variables: DATABASE_URL, LOG_ROOT, MEDIA_ROOT, PLAYOUT_JOURNAL_ROOT")
}

func TestLoadEmptyRequiredCountsAsMissing(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MEDIA_ROOT", "")
	t.Setenv("LOG_ROOT", "/logs")
	t.Setenv("PLAYOUT_JOURNAL_ROOT", "/journal")

	_, err := Load("")

	require.EqualError(t, err, "missing required environment variables: MEDIA_ROOT")
}

func TestLoadMigrateNeedsOnlyDatabase(t *testing.T) {
	clearEnv(t)

	_, err := LoadMigrate("")
	require.EqualError(t, err, "missing required environment variables: DATABASE_URL")

	t.Setenv("DATABASE_URL", "postgres://x")
	cfg, err := LoadMigrate("")
	require.NoError(t, err)
	require.Equal(t, "postgres://x", cfg.DatabaseURL)
}

func TestLoadDefaultsAndSecrets(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("MEDIA_ROOT", "/media")
	t.Setenv("LOG_ROOT", "/logs")
	t.Setenv("PLAYOUT_JOURNAL_ROOT", "/journal")
	t.Setenv("ICECAST_SOURCE_PASSWORD", "s3cret")

	cfg, err := Load("")

	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:8090", cfg.HTTPAddr)
	require.Equal(t, "127.0.0.1:8189", cfg.LiveUDPAddr)
	require.Empty(t, cfg.LivePublicIPs)
	require.Equal(t, "s3cret", cfg.Secrets.IcecastSourcePassword)
	require.Empty(t, cfg.Secrets.LiquidsoapControlPassword)
}

func TestLoadLiveNetwork(t *testing.T) {
	for _, tc := range []struct {
		name, addr, ips string
		want            []string
		err             string
	}{
		{name: "list", addr: "0.0.0.0:8189", ips: "127.0.0.1, 192.168.1.20,", want: []string{"127.0.0.1", "192.168.1.20"}},
		{name: "bad ip", addr: "0.0.0.0:8189", ips: "127.0.0.1,host", err: `LIVE_PUBLIC_IPS: "host" is not an IP address`},
		{name: "bad addr", addr: "8189", err: "LIVE_UDP_ADDR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("DATABASE_URL", "postgres://x")
			t.Setenv("MEDIA_ROOT", "/media")
			t.Setenv("LOG_ROOT", "/logs")
			t.Setenv("PLAYOUT_JOURNAL_ROOT", "/journal")
			t.Setenv("LIVE_UDP_ADDR", tc.addr)
			t.Setenv("LIVE_PUBLIC_IPS", tc.ips)

			cfg, err := Load("")
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.LivePublicIPs)
		})
	}
}

func TestLoadDotenvDoesNotOverrideEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("MEDIA_ROOT", "/from-env")
	dotenv := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(dotenv, []byte(
		"DATABASE_URL=postgres://dotenv\nMEDIA_ROOT=/from-file\nLOG_ROOT=/logs\nPLAYOUT_JOURNAL_ROOT=/journal\n"), 0o600))

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
	t.Setenv("PLAYOUT_JOURNAL_ROOT", "/journal")

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

// IsSet names every secret by its environment variable.
func TestSecretsIsSetCoversEveryField(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[Secrets]()
	want := map[string]bool{}
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("env"), ",")
		want[name] = false
	}
	require.Equal(t, want, Secrets{}.IsSet())

	set := Secrets{LiquidsoapControlUser: "source"}.IsSet()
	require.True(t, set["LIQUIDSOAP_CONTROL_USER"])
	require.False(t, set["ICECAST_SOURCE_PASSWORD"])
}
