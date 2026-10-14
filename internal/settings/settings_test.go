package settings_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/config"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
)

var t0 = time.Date(2026, 10, 13, 6, 44, 0, 0, time.UTC)

type fakeStore struct {
	mu   sync.Mutex
	rows map[string]settings.Stored
	err  error
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]settings.Stored{}} }

func (f *fakeStore) List(context.Context) ([]settings.Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []settings.Stored
	for _, r := range f.rows {
		out = append(out, r)
	}
	return out, f.err
}

func (f *fakeStore) Get(_ context.Context, key string) (settings.Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return settings.Stored{}, f.err
	}
	r, ok := f.rows[key]
	if !ok {
		return settings.Stored{}, settings.ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) Upsert(_ context.Context, s settings.Stored) (settings.Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return settings.Stored{}, f.err
	}
	f.rows[s.Key] = s
	return s, nil
}

// allSecretsSet reports every secret of the configuration as set.
func allSecretsSet() map[string]bool {
	set := map[string]bool{}
	for name := range (config.Secrets{}).IsSet() {
		set[name] = true
	}
	return set
}

func newService(t *testing.T, store settings.Store) (*settings.Service, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log := slog.New(logging.NewHandler(&logs, slog.LevelDebug))
	svc, err := settings.NewService(settings.NewRegistry(), store, allSecretsSet(), log, func() time.Time { return t0 })
	require.NoError(t, err)
	return svc, &logs
}

// The connection defaults equal the dev stack (docker-compose.dev.yml), so
// a backend in the app profile reaches Icecast and Liquidsoap unconfigured
// (init plan, Phase 5 task 2). Change both together.
func TestDefaultsMatchDevStack(t *testing.T) {
	t.Parallel()
	devStack := map[string]any{
		"stream.icecast_host":     "icecast",      // service icecast
		"stream.icecast_port":     8000,           // icecast ports, liquidsoap ICECAST_PORT
		"stream.mount":            "/radio",       // liquidsoap ICECAST_MOUNT
		"stream.codec":            "mp3",          // dev/liquidsoap/radio.liq output
		"stream.bitrate_kbps":     128,            // dev/liquidsoap/radio.liq output
		"stream.sample_rate_hz":   44100,          // dev/liquidsoap/radio.liq output
		"stream.channels":         2,              // dev/liquidsoap/radio.liq output
		"liquidsoap.control_host": "liquidsoap",   // service liquidsoap
		"liquidsoap.control_port": 1234,           // liquidsoap TELNET_PORT
		"liquidsoap.harbor_port":  8005,           // liquidsoap HARBOR_PORT
		"station.timezone":        "Asia/Yerevan", // §10.2
		"liquidsoap.fade_out_ms":  2500,           // §6.3
	}
	reg := settings.NewRegistry()
	for key, want := range devStack {
		d, ok := reg.Lookup(key)
		require.True(t, ok, key)
		require.Equal(t, want, d.Default, key)
	}
}

func TestRegistryDefinitions(t *testing.T) {
	t.Parallel()
	keyPattern := regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`) // ck_settings_key
	groups := []settings.Group{
		settings.GroupStation, settings.GroupStream, settings.GroupLiquidsoap, settings.GroupRotation,
		settings.GroupFiller, settings.GroupOutage, settings.GroupRetention, settings.GroupPlayout,
	}
	reg := settings.NewRegistry()
	for _, d := range append(reg.Settings(), reg.Secrets()...) {
		require.Regexp(t, keyPattern, d.Key)
		require.Contains(t, groups, d.Group, d.Key)
		require.True(t, strings.HasPrefix(d.Key, string(d.Group)+"."), d.Key)
		require.NotEmpty(t, d.Description, d.Key)
		if d.Secret {
			require.NotEmpty(t, d.EnvName, d.Key)
			continue
		}
		require.Contains(t, []settings.Apply{settings.ApplyImmediate, settings.ApplyReconnect}, d.Apply, d.Key)

		// The default passes its own definition and survives a JSON
		// round trip unchanged.
		raw, err := json.Marshal(d.Default)
		require.NoError(t, err, d.Key)
		svc, _ := newService(t, newFakeStore())
		got, err := svc.Set(t.Context(), d.Key, raw)
		require.NoError(t, err, d.Key)
		require.Equal(t, d.Default, got.Value, d.Key)
	}
}

func TestSecretsMatchConfig(t *testing.T) {
	t.Parallel()
	var names []string
	for _, d := range settings.NewRegistry().Secrets() {
		names = append(names, d.EnvName)
	}
	var want []string
	for name := range (config.Secrets{}).IsSet() {
		want = append(want, name)
	}
	slices.Sort(names)
	slices.Sort(want)
	require.Equal(t, want, names)

	_, err := settings.NewService(settings.NewRegistry(), newFakeStore(), map[string]bool{}, slog.New(slog.DiscardHandler), time.Now)
	require.ErrorContains(t, err, "ICECAST_SOURCE_PASSWORD")
}

func TestUnsavedKeyReadsDefault(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t, newFakeStore())

	s, err := svc.Get(t.Context(), "liquidsoap.fade_out_ms")
	require.NoError(t, err)
	require.True(t, s.IsDefault)
	require.Equal(t, 2500, s.Value)
	require.Nil(t, s.UpdatedAt)

	tz, err := settings.Value(t.Context(), svc, settings.StationTimezone)
	require.NoError(t, err)
	require.Equal(t, "Asia/Yerevan", tz)

	id, err := settings.Value(t.Context(), svc, settings.PlayoutEmergencyPlaylistID)
	require.NoError(t, err)
	require.Nil(t, id)

	all, err := svc.List(t.Context())
	require.NoError(t, err)
	require.Len(t, all, len(settings.NewRegistry().Settings()))
	for _, s := range all {
		require.True(t, s.IsDefault, s.Definition.Key)
		require.False(t, s.Definition.Secret, "secrets are not listed")
	}
}

func TestSetSavesLogsAndNotifies(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc, logs := newService(t, store)
	var got []int
	settings.OnChange(svc, settings.LiquidsoapFadeOutMS, func(v int) { got = append(got, v) })
	var changes []settings.Change
	svc.Subscribe(func(c settings.Change) { changes = append(changes, c) })

	s, err := svc.Set(t.Context(), "liquidsoap.fade_out_ms", json.RawMessage(`2700.0`))
	require.NoError(t, err)
	require.Equal(t, 2700, s.Value)
	require.False(t, s.IsDefault)
	require.Equal(t, t0, *s.UpdatedAt)
	require.JSONEq(t, `2700`, string(store.rows["liquidsoap.fade_out_ms"].Value), "stored canonically")

	_, err = svc.Set(t.Context(), "station.name", json.RawMessage(`"Atlantida FM"`))
	require.NoError(t, err)

	require.Equal(t, []int{2700}, got, "OnChange sees only its key")
	require.Len(t, changes, 2)
	require.Equal(t, settings.Change{Key: "liquidsoap.fade_out_ms", Value: 2700, Apply: settings.ApplyImmediate, AffectsAir: true, At: t0}, changes[0])

	v, err := settings.Value(t.Context(), svc, settings.LiquidsoapFadeOutMS)
	require.NoError(t, err)
	require.Equal(t, 2700, v)

	var entry map[string]any
	line, _, _ := strings.Cut(logs.String(), "\n")
	require.NoError(t, json.Unmarshal([]byte(line), &entry))
	require.Equal(t, "INFO", entry["level"])
	require.Equal(t, "setting changed", entry["msg"])
	require.Equal(t, "liquidsoap.fade_out_ms", entry["key"])
	require.InDelta(t, 2700, entry["value"], 0)
	require.Equal(t, true, entry["affects_air"])
	require.Equal(t, "2026-10-13T06:44:00Z", entry["changed_at"])
}

func TestSetRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key, value string
		rule       settings.Rule
	}{
		{"liquidsoap.fade_out_ms", `1999`, settings.RuleRange},
		{"liquidsoap.fade_out_ms", `3001`, settings.RuleRange},
		{"liquidsoap.fade_out_ms", `2500.5`, settings.RuleType},
		{"liquidsoap.fade_out_ms", `"2500"`, settings.RuleType},
		{"liquidsoap.fade_out_ms", `null`, settings.RuleType},
		{"liquidsoap.fade_out_ms", `1e30`, settings.RuleRange},
		{"liquidsoap.music_gain", `1.5`, settings.RuleRange},
		{"liquidsoap.music_gain", `true`, settings.RuleType},
		{"station.timezone", `"Mars/Olympus"`, settings.RuleFormat},
		{"station.timezone", `"Local"`, settings.RuleFormat},
		{"station.timezone", `""`, settings.RuleLength},
		{"station.timezone", `42`, settings.RuleType},
		{"station.name", `" padded "`, settings.RuleFormat},
		{"stream.icecast_host", `"bad host"`, settings.RuleFormat},
		{"stream.icecast_host", `"-icecast"`, settings.RuleFormat},
		{"stream.mount", `"radio"`, settings.RuleFormat},
		{"stream.codec", `"flac"`, settings.RuleEnum},
		{"stream.bitrate_kbps", `100`, settings.RuleEnum},
		{"stream.channels", `3`, settings.RuleEnum},
		{"stream.icecast_port", `0`, settings.RuleRange},
		{"stream.icecast_port", `65536`, settings.RuleRange},
		{"stream.hls_enabled", `"yes"`, settings.RuleType},
		{"rotation.lookahead_items", `4`, settings.RuleRange},
		{"rotation.plan_horizon_hours", `12`, settings.RuleRange},
		{"retention.play_log_days", `2`, settings.RuleRange},
		{"playout.emergency_playlist_id", `0`, settings.RuleRange},
		{"playout.emergency_playlist_id", `"3"`, settings.RuleType},
		{"filler.fallback", `"random"`, settings.RuleType},
		{"filler.fallback", `null`, settings.RuleType},
		{"filler.fallback", `{}`, settings.RuleRequired},
		{"filler.fallback", `{"mode":"ask"}`, settings.RuleEnum},
		{"filler.fallback", `{"mode":"pool"}`, settings.RuleEnum},
		{"filler.fallback", `{"mode":"playlist"}`, settings.RuleRequired},
		{"filler.fallback", `{"mode":"random","source_id":3}`, settings.RuleInvalid},
		{"filler.fallback", `{"mode":"artist","source_id":0}`, settings.RuleRange},
		{"filler.fallback", `{"mode":"random","extra":1}`, settings.RuleUnknownField},
		{"filler.fallback", `{"mode":1}`, settings.RuleType},
		{"station.name", `"a" "b"`, settings.RuleType},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			t.Parallel()
			store := newFakeStore()
			svc, _ := newService(t, store)
			_, err := svc.Set(t.Context(), tt.key, json.RawMessage(tt.value))
			var ive *settings.InvalidValueError
			require.ErrorAs(t, err, &ive)
			require.Equal(t, tt.rule, ive.Rule, ive.Message)
			require.Equal(t, tt.key, ive.Key)
			require.Empty(t, store.rows, "nothing saved")
		})
	}
}

func TestSetAccepts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key, value string
		want       any
	}{
		{"station.timezone", `"UTC"`, "UTC"},
		{"station.timezone", `"Europe/Berlin"`, "Europe/Berlin"},
		{"stream.icecast_host", `"stream.example.org"`, "stream.example.org"},
		{"stream.icecast_host", `"192.168.1.20"`, "192.168.1.20"},
		{"stream.icecast_host", `"::1"`, "::1"},
		{"stream.description", `""`, ""},
		{"liquidsoap.music_gain", `0.2`, 0.2},
		{"liquidsoap.music_gain", `0`, 0.0},
		{"stream.loudness_target_lufs", `-23`, -23.0},
		{"playout.emergency_playlist_id", `7`, new(uint64(7))},
		{"playout.emergency_playlist_id", `null`, (*uint64)(nil)},
		{"filler.fallback", `{"mode":"random"}`, settings.FillerFallback{Mode: "random"}},
		{"filler.fallback", `{"mode":"artist","source_id":4}`, settings.FillerFallback{Mode: "artist", SourceID: new(uint64(4))}},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			t.Parallel()
			svc, _ := newService(t, newFakeStore())
			s, err := svc.Set(t.Context(), tt.key, json.RawMessage(tt.value))
			require.NoError(t, err)
			require.Equal(t, tt.want, s.Value)
		})
	}
}

func TestUnknownAndSecretKeys(t *testing.T) {
	t.Parallel()
	svc, logs := newService(t, newFakeStore())

	_, err := svc.Set(t.Context(), "no.such_key", json.RawMessage(`1`))
	require.ErrorIs(t, err, settings.ErrNotFound)
	_, err = svc.Get(t.Context(), "no.such_key")
	require.ErrorIs(t, err, settings.ErrNotFound)

	_, err = svc.Set(t.Context(), settings.SecretIcecastSourcePassword, json.RawMessage(`"hunter2"`))
	require.ErrorIs(t, err, settings.ErrNotEditable)
	_, err = svc.Get(t.Context(), settings.SecretLiquidsoapControlPassword)
	require.ErrorIs(t, err, settings.ErrNotEditable)
	require.NotContains(t, logs.String(), "hunter2")
}

func TestSecretsReportOnlyIsSet(t *testing.T) {
	t.Parallel()
	set := allSecretsSet()
	set["LIQUIDSOAP_CONTROL_PASSWORD"] = false
	svc, err := settings.NewService(settings.NewRegistry(), newFakeStore(), set, slog.New(slog.DiscardHandler), time.Now)
	require.NoError(t, err)

	got := map[string]bool{}
	for _, s := range svc.Secrets() {
		got[s.Definition.EnvName] = s.IsSet
		// The status carries no value field at all.
		require.Equal(t, 2, reflect.TypeOf(s).NumField())
	}
	require.Equal(t, map[string]bool{
		"ICECAST_SOURCE_PASSWORD":     true,
		"LIQUIDSOAP_CONTROL_USER":     true,
		"LIQUIDSOAP_CONTROL_PASSWORD": false,
	}, got)
}

func TestInvalidSavedValueFallsBackToDefault(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	// Saved by an earlier release under looser rules.
	store.rows["liquidsoap.fade_out_ms"] = settings.Stored{Key: "liquidsoap.fade_out_ms", Value: json.RawMessage(`5000`), UpdatedAt: t0}
	svc, logs := newService(t, store)

	s, err := svc.Get(t.Context(), "liquidsoap.fade_out_ms")
	require.NoError(t, err)
	require.True(t, s.IsDefault)
	require.Equal(t, 2500, s.Value)
	require.Contains(t, logs.String(), `"level":"WARN"`)
}

func TestStoreErrorsAreWrapped(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.err = errors.New("connection refused")
	svc, _ := newService(t, store)

	_, err := svc.List(t.Context())
	require.ErrorContains(t, err, "connection refused")
	_, err = svc.Get(t.Context(), "station.name")
	require.ErrorContains(t, err, "get setting station.name")
	_, err = svc.Set(t.Context(), "station.name", json.RawMessage(`"X"`))
	require.ErrorContains(t, err, "save setting station.name")
}
