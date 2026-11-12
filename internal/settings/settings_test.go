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
	"strconv"
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
	// upserts counts the UpsertMany calls.
	upserts int
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

func (f *fakeStore) UpsertMany(_ context.Context, sts []settings.Stored) ([]settings.Stored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	if f.err != nil {
		return nil, f.err
	}
	for _, s := range sts {
		f.rows[s.Key] = s
	}
	return sts, nil
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
		"stream.icecast_tls":      false,          // liquidsoap ICECAST_TLS, plain icecast service
		"stream.mount":            "/radio",       // liquidsoap ICECAST_MOUNT
		"stream.codec":            "mp3",          // dev/liquidsoap/radio.liq output
		"stream.bitrate_kbps":     128,            // dev/liquidsoap/radio.liq output
		"stream.sample_rate_hz":   44100,          // dev/liquidsoap/radio.liq output
		"stream.channels":         2,              // dev/liquidsoap/radio.liq output
		"liquidsoap.control_host": "liquidsoap",   // service liquidsoap
		"liquidsoap.control_port": 1234,           // liquidsoap TELNET_PORT
		"liquidsoap.harbor_port":  8005,           // liquidsoap HARBOR_PORT
		"liquidsoap.media_root":   "/media",       // liquidsoap volume MEDIA_ROOT:/media
		"liquidsoap.duck_gain":    0.2,            // dev/liquidsoap/radio.liq duck_gain
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
		settings.GroupOutage, settings.GroupRetention, settings.GroupPlayout, settings.GroupAir,
		settings.GroupImport,
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

	played, err := settings.Value(t.Context(), svc, settings.AirPlayedCount)
	require.NoError(t, err)
	require.Equal(t, 10, played)
	queued, err := settings.Value(t.Context(), svc, settings.AirQueuedCount)
	require.NoError(t, err)
	require.Equal(t, 9, queued)

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
	svc.Subscribe(func(cs []settings.Change) {
		require.Len(t, cs, 1, "Set notifies once with one key")
		changes = append(changes, cs...)
	})

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
		{"stream.icecast_tls", `"true"`, settings.RuleType},
		{"rotation.lookahead_items", `4`, settings.RuleRange},
		{"rotation.plan_horizon_days", `0`, settings.RuleRange},
		{"rotation.plan_horizon_days", `184`, settings.RuleRange},
		{"rotation.grid_horizon_days", `184`, settings.RuleRange},
		{"rotation.full_pass_time", `"24:00"`, settings.RuleFormat},
		{"rotation.full_pass_time", `"3:00"`, settings.RuleLength},
		{"rotation.full_pass_batch_hours", `23`, settings.RuleRange},
		{"rotation.full_pass_batch_hours", `745`, settings.RuleRange},
		{"rotation.full_pass_enabled", `"yes"`, settings.RuleType},
		{"retention.play_log_days", `2`, settings.RuleRange},
		{"playout.emergency_playlist_id", `0`, settings.RuleRange},
		{"playout.emergency_playlist_id", `"3"`, settings.RuleType},
		{"air.played_count", `0`, settings.RuleRange},
		{"air.played_count", `51`, settings.RuleRange},
		{"air.queued_count", `-1`, settings.RuleRange},
		{"air.queued_count", `51`, settings.RuleRange},
		{"air.queued_count", `2.5`, settings.RuleType},
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
		{"air.played_count", `1`, 1},
		{"air.played_count", `50`, 50},
		{"air.queued_count", `0`, 0},
		{"air.queued_count", `50`, 50},
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

// A saved row whose key is no longer in the registry (retention.log_days was
// removed) is ignored: it is not listed, it reads as not found, and loading
// does not warn or fail. The row stays in the table.
func TestStoredKeyNotInRegistryIsIgnored(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.rows["retention.log_days"] = settings.Stored{Key: "retention.log_days", Value: json.RawMessage(`3`), UpdatedAt: t0}
	svc, logs := newService(t, store)

	all, err := svc.List(t.Context())
	require.NoError(t, err)
	require.Len(t, all, len(settings.NewRegistry().Settings()))
	for _, s := range all {
		require.NotEqual(t, "retention.log_days", s.Definition.Key)
	}
	_, err = svc.Get(t.Context(), "retention.log_days")
	require.ErrorIs(t, err, settings.ErrNotFound)
	require.NotContains(t, logs.String(), `"level":"WARN"`)
	require.Contains(t, store.rows, "retention.log_days", "the row is left in the table")
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

func TestGuardRefusesBeforeSave(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc, _ := newService(t, store)
	settings.Guard(svc, settings.RetentionPlayLogDays, func(_ context.Context, days int) error {
		if days < 5 {
			return settings.Refuse(settings.RetentionPlayLogDays.Name(), settings.RuleIntervalBeyondRetention, "a play rule needs 5 days")
		}
		return nil
	})
	_, err := svc.Set(t.Context(), "retention.play_log_days", json.RawMessage(`4`))
	var ive *settings.InvalidValueError
	require.ErrorAs(t, err, &ive)
	require.Equal(t, settings.RuleIntervalBeyondRetention, ive.Rule)
	require.Empty(t, store.rows, "nothing saved")

	s, err := svc.Set(t.Context(), "retention.play_log_days", json.RawMessage(`5`))
	require.NoError(t, err)
	require.Equal(t, 5, s.Value)
}

// The full pass settings have their code defaults, and the plan horizon is
// kept in days: rotation.plan_horizon_hours is no longer defined.
func TestRotationFullPassSettingsDefaults(t *testing.T) {
	t.Parallel()
	reg := settings.NewRegistry()
	defaults := map[string]any{
		"rotation.grid_horizon_days":      183,
		"rotation.plan_horizon_days":      31,
		"rotation.full_pass_time":         "03:00",
		"rotation.full_pass_batch_hours":  168,
		"rotation.full_pass_enabled":      true,
		"rotation.full_pass_slow_ms":      60000,
		"rotation.full_pass_heap_warn_mb": 512,
	}
	for key, want := range defaults {
		d, ok := reg.Lookup(key)
		require.True(t, ok, key)
		require.Equal(t, want, d.Default, key)
	}
	_, ok := reg.Lookup("rotation.plan_horizon_hours")
	require.False(t, ok, "the hours key is replaced by plan_horizon_days")

	bounds := map[string][2]float64{
		"rotation.grid_horizon_days":      {1, 183},
		"rotation.plan_horizon_days":      {1, 183},
		"rotation.full_pass_batch_hours":  {24, 744},
		"rotation.full_pass_slow_ms":      {1000, 3600000},
		"rotation.full_pass_heap_warn_mb": {64, 16384},
	}
	for key, b := range bounds {
		d, _ := reg.Lookup(key)
		require.Equal(t, b[0], *d.Minimum, key)
		require.Equal(t, b[1], *d.Maximum, key)
	}
}

// refuseStreamBitrate registers a check that refuses a bitrate above 128
// kbps on every changed key it watches, and counts its runs.
func refuseStreamBitrate(svc *settings.Service, runs *int) {
	keys := []string{settings.StreamCodec.Name(), settings.StreamBitrateKbps.Name()}
	settings.Check(svc, keys, func(_ context.Context, v settings.View) error {
		*runs++
		kbps, err := settings.ViewValue(v, settings.StreamBitrateKbps)
		if err != nil {
			return err
		}
		if kbps <= 128 {
			return nil
		}
		var refused []error
		for _, k := range keys {
			if v.Changed(k) {
				refused = append(refused, settings.Refuse(k, settings.RuleEncoderLimit, "too high"))
			}
		}
		return errors.Join(refused...)
	})
}

// One bad field means nothing is saved, and every refusal comes back at
// once: own rules, guards, cross-key checks, unknown keys, and secrets.
func TestSetManyCollectsEveryRefusal(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc, logs := newService(t, store)
	runs := 0
	refuseStreamBitrate(svc, &runs)
	settings.Guard(svc, settings.RetentionPlayLogDays, func(context.Context, int) error {
		return settings.Refuse(settings.RetentionPlayLogDays.Name(), settings.RuleIntervalBeyondRetention, "a play rule needs more")
	})
	notified := 0
	svc.Subscribe(func([]settings.Change) { notified++ })

	_, err := svc.SetMany(t.Context(), map[string]json.RawMessage{
		"station.name":                       json.RawMessage(`"Fine"`),
		"liquidsoap.fade_out_ms":             json.RawMessage(`1500`),
		"stream.bitrate_kbps":                json.RawMessage(`192`),
		"retention.play_log_days":            json.RawMessage(`30`),
		"no.such_key":                        json.RawMessage(`1`),
		settings.SecretIcecastSourcePassword: json.RawMessage(`"hunter2"`),
	})
	var ive *settings.InvalidValuesError
	require.ErrorAs(t, err, &ive)
	type refusal struct {
		key  string
		rule settings.Rule
	}
	var got []refusal
	for _, e := range ive.Errors {
		got = append(got, refusal{e.Key, e.Rule})
	}
	require.Equal(t, []refusal{
		{"stream.bitrate_kbps", settings.RuleEncoderLimit},
		{"liquidsoap.fade_out_ms", settings.RuleRange},
		{"retention.play_log_days", settings.RuleIntervalBeyondRetention},
		{"no.such_key", settings.RuleUnknownField},
		{settings.SecretIcecastSourcePassword, settings.RuleUnknownField},
	}, got, "registry order, then unknown keys by key")
	require.Contains(t, ive.Errors[4].Message, "secret")
	require.Equal(t, 1, runs)
	require.Zero(t, store.upserts, "nothing written")
	require.Empty(t, store.rows)
	require.Zero(t, notified)
	require.NotContains(t, logs.String(), "setting changed")
	require.NotContains(t, logs.String(), "hunter2")
}

// A check whose key failed its own rules is skipped: it would judge the
// saved value instead of the one the manager typed.
func TestSetManySkipsCheckOfRefusedKey(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t, newFakeStore())
	runs := 0
	refuseStreamBitrate(svc, &runs)
	_, err := svc.SetMany(t.Context(), map[string]json.RawMessage{
		"stream.codec":        json.RawMessage(`"flac"`),
		"stream.bitrate_kbps": json.RawMessage(`192`),
	})
	var ive *settings.InvalidValuesError
	require.ErrorAs(t, err, &ive)
	require.Len(t, ive.Errors, 1)
	require.Equal(t, settings.RuleEnum, ive.Errors[0].Rule)
	require.Zero(t, runs)

	// A check that touches no key of the batch does not run either.
	_, err = svc.SetMany(t.Context(), map[string]json.RawMessage{"station.name": json.RawMessage(`"X"`)})
	require.NoError(t, err)
	require.Zero(t, runs)
}

// A check that fails for another reason fails the save with its error.
func TestSetManyCheckError(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc, _ := newService(t, store)
	settings.Check(svc, []string{"station.name"}, func(context.Context, settings.View) error {
		return errors.New("lookup failed")
	})
	_, err := svc.SetMany(t.Context(), map[string]json.RawMessage{"station.name": json.RawMessage(`"X"`)})
	require.ErrorContains(t, err, "lookup failed")
	var ive *settings.InvalidValuesError
	require.NotErrorAs(t, err, &ive)
	require.Zero(t, store.upserts)
}

// A valid batch is written in one call, logged per key, and notified once
// with every key in registry order.
func TestSetManySavesAndNotifiesOnce(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc, logs := newService(t, store)
	var calls [][]settings.Change
	svc.Subscribe(func(cs []settings.Change) { calls = append(calls, cs) })
	var gains []float64
	settings.OnChange(svc, settings.LiquidsoapMusicGain, func(v float64) { gains = append(gains, v) })

	saved, err := svc.SetMany(t.Context(), map[string]json.RawMessage{
		"stream.bitrate_kbps":   json.RawMessage(`192`),
		"liquidsoap.music_gain": json.RawMessage(`0.5`),
		"stream.codec":          json.RawMessage(`"vorbis"`),
	})
	require.NoError(t, err)
	require.Equal(t, 1, store.upserts)
	require.Len(t, store.rows, 3)

	keys := func(n int, key func(int) string) []string {
		out := make([]string, n)
		for i := range n {
			out[i] = key(i)
		}
		return out
	}
	want := []string{"stream.codec", "stream.bitrate_kbps", "liquidsoap.music_gain"}
	require.Equal(t, want, keys(len(saved), func(i int) string { return saved[i].Definition.Key }), "registry order")
	require.Len(t, calls, 1, "one notification")
	require.Equal(t, want, keys(len(calls[0]), func(i int) string { return calls[0][i].Key }))
	for _, c := range calls[0] {
		require.Equal(t, t0, c.At)
	}
	require.Equal(t, []float64{0.5}, gains)
	require.Equal(t, 192, saved[1].Value)
	require.Equal(t, t0, *saved[1].UpdatedAt)
	require.Equal(t, 3, strings.Count(logs.String(), `"msg":"setting changed"`))
}

// The view holds the batch over the saved values over the defaults.
func TestViewValue(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	store.rows["stream.codec"] = settings.Stored{Key: "stream.codec", Value: json.RawMessage(`"aac"`), UpdatedAt: t0}
	store.rows["stream.channels"] = settings.Stored{Key: "stream.channels", Value: json.RawMessage(`1`), UpdatedAt: t0}
	svc, _ := newService(t, store)
	type seen struct {
		codec                  string
		channels, rate, kbps   int
		codecChanged, chChange bool
	}
	var got seen
	settings.Check(svc, []string{"stream.channels"}, func(_ context.Context, v settings.View) error {
		var err error
		got.codec, err = settings.ViewValue(v, settings.StreamCodec)
		require.NoError(t, err)
		got.channels, err = settings.ViewValue(v, settings.StreamChannels)
		require.NoError(t, err)
		got.rate, err = settings.ViewValue(v, settings.StreamSampleRateHz)
		require.NoError(t, err)
		got.kbps, err = settings.ViewValue(v, settings.StreamBitrateKbps)
		require.NoError(t, err)
		got.codecChanged, got.chChange = v.Changed("stream.codec"), v.Changed("stream.channels")
		return nil
	})
	_, err := svc.SetMany(t.Context(), map[string]json.RawMessage{
		"stream.channels":     json.RawMessage(`2`),
		"stream.bitrate_kbps": json.RawMessage(`256`),
	})
	require.NoError(t, err)
	require.Equal(t, seen{codec: "aac", channels: 2, rate: 44100, kbps: 256, chChange: true}, got)
}

// An empty batch saves nothing and notifies nobody.
func TestSetManyEmpty(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc, _ := newService(t, store)
	notified := 0
	svc.Subscribe(func([]settings.Change) { notified++ })
	saved, err := svc.SetMany(t.Context(), nil)
	require.NoError(t, err)
	require.Empty(t, saved)
	require.Zero(t, store.upserts)
	require.Zero(t, notified)
}

// Saves run one at a time: concurrent batches never interleave their
// checks and writes, and each is notified once.
func TestSetManyConcurrent(t *testing.T) {
	t.Parallel()
	svc, _ := newService(t, newFakeStore())
	var mu sync.Mutex
	notified := 0
	svc.Subscribe(func([]settings.Change) {
		mu.Lock()
		notified++
		mu.Unlock()
	})
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			_, err := svc.SetMany(t.Context(), map[string]json.RawMessage{
				"rotation.min_gap_ms": json.RawMessage(strconv.Itoa(1000 + i)),
				"station.name":        json.RawMessage(`"X"`),
			})
			require.NoError(t, err)
		})
	}
	wg.Wait()
	require.Equal(t, 20, notified)
}
