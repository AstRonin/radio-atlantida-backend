package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
	"github.com/AstRonin/radio-atlantida-backend/internal/streamformat"
)

type memSettings struct {
	mu   sync.Mutex
	rows map[string]settings.Stored
}

func (m *memSettings) List(context.Context) ([]settings.Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []settings.Stored
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}

func (m *memSettings) Get(_ context.Context, key string) (settings.Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[key]
	if !ok {
		return settings.Stored{}, settings.ErrNotFound
	}
	return r, nil
}

func (m *memSettings) UpsertMany(_ context.Context, sts []settings.Stored) ([]settings.Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range sts {
		m.rows[s.Key] = s
	}
	return sts, nil
}

// newSettingsRouter serves the settings API on the real service over an
// in-memory store. The secrets are set to secretValue in the environment.
func newSettingsRouter(t *testing.T) (http.Handler, *syncBuffer) {
	t.Helper()
	h, logs, _ := newSettingsRouterWith(t)
	return h, logs
}

// newSettingsRouterWith is newSettingsRouter with access to the service,
// and the stream encoder check of cmd/backend (checkEncoderLimits) in a
// copy that refuses on the changed keys.
func newSettingsRouterWith(t *testing.T) (http.Handler, *syncBuffer, *settings.Service) {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(logging.NewHandler(logs, slog.LevelDebug))
	svc, err := settings.NewService(settings.NewRegistry(), &memSettings{rows: map[string]settings.Stored{}},
		map[string]bool{"ICECAST_SOURCE_PASSWORD": true, "LIQUIDSOAP_CONTROL_USER": true, "LIQUIDSOAP_CONTROL_PASSWORD": false},
		log, func() time.Time { return testNow })
	require.NoError(t, err)
	keys := []string{
		settings.StreamCodec.Name(), settings.StreamBitrateKbps.Name(),
		settings.StreamSampleRateHz.Name(), settings.StreamChannels.Name(),
	}
	settings.Check(svc, keys, func(_ context.Context, v settings.View) error {
		codec, _ := settings.ViewValue(v, settings.StreamCodec)
		ch, _ := settings.ViewValue(v, settings.StreamChannels)
		sr, _ := settings.ViewValue(v, settings.StreamSampleRateHz)
		kbps, _ := settings.ViewValue(v, settings.StreamBitrateKbps)
		l := streamformat.Check(codec, ch, sr, kbps)
		if l == nil {
			return nil
		}
		var refused []error
		for _, k := range keys {
			if v.Changed(k) {
				refused = append(refused, settings.Refuse(k, settings.RuleEncoderLimit, l.String()))
			}
		}
		return errors.Join(refused...)
	})
	return NewRouter(Deps{Log: log, DB: fakePinger{}, Settings: svc, Now: func() time.Time { return testNow }}), logs, svc
}

const secretValue = "s3cret-value"

func put(t *testing.T, h http.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return send(t, h, http.MethodPut, target, body)
}

func send(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

func findSetting(t *testing.T, body []byte, key string) map[string]any {
	t.Helper()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(body, &list))
	for _, s := range list.Items {
		if s["key"] == key {
			return s
		}
	}
	t.Fatalf("setting %s not listed", key)
	return nil
}

// Reading an unsaved key returns its default (Phase 5 "done when").
func TestSettingsUnsavedKeyReturnsDefault(t *testing.T) {
	t.Parallel()
	h, _ := newSettingsRouter(t)

	rec := do(t, h, http.MethodGet, "/api/settings")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	s := findSetting(t, rec.Body.Bytes(), "liquidsoap.fade_out_ms")
	require.JSONEq(t, `{"key":"liquidsoap.fade_out_ms","group":"liquidsoap",
		"description":"Fade-out of a truncated track, ending at the segment end.",
		"type":"integer","unit":"ms","value":2500,"default":2500,"is_default":true,"nullable":false,
		"minimum":2000,"maximum":3000,"apply":"immediate","affects_air":true}`, mustJSON(t, s))

	id := findSetting(t, rec.Body.Bytes(), "playout.emergency_playlist_id")
	require.Nil(t, id["value"])
	require.Equal(t, true, id["nullable"])

	host := findSetting(t, rec.Body.Bytes(), "stream.icecast_host")
	require.Equal(t, "icecast", host["value"])
	require.Equal(t, "reconnect", host["apply"])
}

func TestSettingsSaveAndReadBack(t *testing.T) {
	t.Parallel()
	h, logs := newSettingsRouter(t)

	rec := put(t, h, "/api/settings/rotation.min_gap_ms", `{"value":5000}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var saved map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &saved))
	require.Equal(t, 5000.0, saved["value"])
	require.Equal(t, false, saved["is_default"])
	require.Equal(t, "2026-10-13T06:44:05Z", saved["updated_at"], "UTC")

	rec = do(t, h, http.MethodGet, "/api/settings")
	s := findSetting(t, rec.Body.Bytes(), "rotation.min_gap_ms")
	require.Equal(t, 5000.0, s["value"])
	require.Equal(t, 10000.0, s["default"])

	var changed map[string]any
	for _, e := range logs.entries(t) {
		if e["msg"] == "setting changed" {
			changed = e
		}
	}
	require.NotNil(t, changed)
	require.Equal(t, "INFO", changed["level"])
	require.Equal(t, "api", changed["component"])
	require.Equal(t, "2026-10-13T06:44:05Z", changed["changed_at"])
}

// An invalid value returns a field error (Phase 5 "done when").
func TestSettingsInvalidValueIsFieldError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		key, body string
		code      errcodes.Field
	}{
		{"liquidsoap.fade_out_ms", `{"value":1500}`, errcodes.Range},
		{"liquidsoap.fade_out_ms", `{"value":"2500"}`, errcodes.Type},
		{"station.timezone", `{"value":"Nowhere/City"}`, errcodes.Format},
		{"stream.codec", `{"value":"wav"}`, errcodes.Enum},
	}
	for _, tt := range tests {
		t.Run(tt.key+" "+tt.body, func(t *testing.T) {
			t.Parallel()
			h, _ := newSettingsRouter(t)

			rec := put(t, h, "/api/settings/"+tt.key, tt.body)

			require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
			p := decodeProblem(t, rec)
			require.Equal(t, errcodes.ValidationFailed, p.Code)
			require.Len(t, p.Errors, 1)
			require.Equal(t, "value", p.Errors[0].Field)
			require.Equal(t, tt.code, p.Errors[0].Code)

			rec = do(t, h, http.MethodGet, "/api/settings")
			require.Equal(t, true, findSetting(t, rec.Body.Bytes(), tt.key)["is_default"], "nothing saved")
		})
	}

	t.Run("missing value", func(t *testing.T) {
		t.Parallel()
		h, _ := newSettingsRouter(t)
		rec := put(t, h, "/api/settings/station.name", `{}`)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
		p := decodeProblem(t, rec)
		require.Equal(t, "value", p.Errors[0].Field)
		require.Equal(t, errcodes.Required, p.Errors[0].Code)
	})
}

func TestSettingsUnknownAndSecretKeys(t *testing.T) {
	t.Parallel()
	h, _ := newSettingsRouter(t)

	rec := put(t, h, "/api/settings/no.such_key", `{"value":1}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, errcodes.SettingNotFound, decodeProblem(t, rec).Code)

	rec = put(t, h, "/api/settings/"+settings.SecretIcecastSourcePassword, `{"value":"`+secretValue+`"}`)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, errcodes.SettingNotEditable, decodeProblem(t, rec).Code)
}

// Secrets never appear in any response or log (Phase 5 "done when"): the
// API reports only whether each is set.
func TestSettingsSecretsReportOnlyIsSet(t *testing.T) {
	t.Parallel()
	h, logs := newSettingsRouter(t)

	rec := do(t, h, http.MethodGet, "/api/settings/secrets")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"items":[
		{"name":"ICECAST_SOURCE_PASSWORD","group":"stream","description":"Icecast source password.","is_set":true},
		{"name":"LIQUIDSOAP_CONTROL_USER","group":"liquidsoap","description":"Liquidsoap control user.","is_set":true},
		{"name":"LIQUIDSOAP_CONTROL_PASSWORD","group":"liquidsoap","description":"Liquidsoap control password.","is_set":false}
	]}`, rec.Body.String())

	all := do(t, h, http.MethodGet, "/api/settings")
	require.NotContains(t, all.Body.String(), "password")
	refused := put(t, h, "/api/settings/"+settings.SecretLiquidsoapControlPassword, `{"value":"`+secretValue+`"}`)
	require.NotContains(t, refused.Body.String(), secretValue)
	require.NotContains(t, logs.buf.String(), secretValue)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func patchSettings(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return send(t, h, http.MethodPatch, "/api/settings", body)
}

// PATCH /api/settings saves a batch and returns the saved settings in the
// order of GET /api/settings.
func TestSettingsPatchSavesBatch(t *testing.T) {
	t.Parallel()
	h, logs, _ := newSettingsRouterWith(t)

	rec := patchSettings(t, h, `{"values":{"rotation.min_gap_ms":5000,"stream.codec":"vorbis","playout.emergency_playlist_id":null}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list struct {
		Items []map[string]any `json:"items"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	var keys []string
	for _, s := range list.Items {
		keys = append(keys, s["key"].(string))
		require.Equal(t, false, s["is_default"])
		require.Equal(t, "2026-10-13T06:44:05Z", s["updated_at"])
	}
	require.Equal(t, []string{"stream.codec", "rotation.min_gap_ms", "playout.emergency_playlist_id"}, keys)
	require.Equal(t, "vorbis", list.Items[0]["value"])
	require.Equal(t, 5000.0, list.Items[1]["value"])
	require.Nil(t, list.Items[2]["value"])

	all := do(t, h, http.MethodGet, "/api/settings")
	require.Equal(t, "vorbis", findSetting(t, all.Body.Bytes(), "stream.codec")["value"])
	changed := 0
	for _, e := range logs.entries(t) {
		if e["msg"] == "setting changed" {
			changed++
		}
	}
	require.Equal(t, 3, changed)
}

// One refused value refuses the batch: every refusal is a field error on
// values.<key>, and nothing is saved.
func TestSettingsPatchRefusesWholeBatch(t *testing.T) {
	t.Parallel()
	h, _, svc := newSettingsRouterWith(t)
	_, err := svc.SetMany(t.Context(), map[string]json.RawMessage{
		"stream.codec": json.RawMessage(`"vorbis"`), "stream.channels": json.RawMessage(`1`),
		"stream.sample_rate_hz": json.RawMessage(`22050`), "stream.bitrate_kbps": json.RawMessage(`64`),
	})
	require.NoError(t, err)

	rec := patchSettings(t, h, `{"values":{"stream.bitrate_kbps":128,"liquidsoap.fade_out_ms":1500,"station.name":"New","no.such_key":1,"stream.icecast_source_password":"`+secretValue+`"}}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), secretValue)
	p := decodeProblem(t, rec)
	require.Equal(t, errcodes.ValidationFailed, p.Code)
	type fe struct {
		field string
		code  errcodes.Field
	}
	var got []fe
	for _, e := range p.Errors {
		got = append(got, fe{e.Field, e.Code})
		require.NotEmpty(t, e.Message)
	}
	require.Equal(t, []fe{
		{"values.stream.bitrate_kbps", errcodes.EncoderLimit},
		{"values.liquidsoap.fade_out_ms", errcodes.Range},
		{"values.no.such_key", errcodes.UnknownField},
		{"values.stream.icecast_source_password", errcodes.UnknownField},
	}, got)
	require.Equal(t, "Vorbis mono at 22050 Hz allows at most 64 kbps", p.Errors[0].Message)

	all := do(t, h, http.MethodGet, "/api/settings")
	require.Equal(t, true, findSetting(t, all.Body.Bytes(), "station.name")["is_default"], "nothing saved")
	require.Equal(t, 64.0, findSetting(t, all.Body.Bytes(), "stream.bitrate_kbps")["value"])
}

// Raising the sample rate and the bitrate together passes whatever the
// order of the keys; the bitrate alone is refused.
func TestSettingsPatchOrderIndependent(t *testing.T) {
	t.Parallel()
	saved := `{"values":{"stream.codec":"vorbis","stream.channels":1,"stream.sample_rate_hz":22050,"stream.bitrate_kbps":64}}`
	for _, body := range []string{
		`{"values":{"stream.sample_rate_hz":44100,"stream.bitrate_kbps":192}}`,
		`{"values":{"stream.bitrate_kbps":192,"stream.sample_rate_hz":44100}}`,
	} {
		h, _ := newSettingsRouter(t)
		require.Equal(t, http.StatusOK, patchSettings(t, h, saved).Code)
		rec := patchSettings(t, h, body)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", body, rec.Body.String())
	}

	h, _ := newSettingsRouter(t)
	require.Equal(t, http.StatusOK, patchSettings(t, h, saved).Code)
	rec := put(t, h, "/api/settings/stream.bitrate_kbps", `{"value":192}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	p := decodeProblem(t, rec)
	require.Len(t, p.Errors, 1)
	require.Equal(t, "value", p.Errors[0].Field, "the single-key route keeps its field name")
	require.Equal(t, errcodes.EncoderLimit, p.Errors[0].Code)
}

// The body needs values with at least one key; huma refuses the rest.
func TestSettingsPatchBody(t *testing.T) {
	t.Parallel()
	h, _ := newSettingsRouter(t)
	for _, tt := range []struct {
		body  string
		field string
		code  errcodes.Field
	}{
		{`{}`, "values", errcodes.Required},
		{`{"values":{}}`, "values", errcodes.Length},
		{`{"values":[]}`, "values", errcodes.Type},
	} {
		rec := patchSettings(t, h, tt.body)
		require.Equal(t, http.StatusUnprocessableEntity, rec.Code, "%s: %s", tt.body, rec.Body.String())
		p := decodeProblem(t, rec)
		require.Equal(t, errcodes.ValidationFailed, p.Code)
		require.NotEmpty(t, p.Errors, tt.body)
		require.Equal(t, tt.field, p.Errors[0].Field, tt.body)
		require.Equal(t, tt.code, p.Errors[0].Code, tt.body)
	}
}
