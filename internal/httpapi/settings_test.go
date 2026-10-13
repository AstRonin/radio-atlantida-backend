package httpapi

import (
	"context"
	"encoding/json"
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

func (m *memSettings) Upsert(_ context.Context, s settings.Stored) (settings.Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[s.Key] = s
	return s, nil
}

// newSettingsRouter serves the settings API on the real service over an
// in-memory store. The secrets are set to secretValue in the environment.
func newSettingsRouter(t *testing.T) (http.Handler, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	log := slog.New(logging.NewHandler(logs, slog.LevelDebug))
	svc, err := settings.NewService(settings.NewRegistry(), &memSettings{rows: map[string]settings.Stored{}},
		map[string]bool{"ICECAST_SOURCE_PASSWORD": true, "LIQUIDSOAP_CONTROL_USER": true, "LIQUIDSOAP_CONTROL_PASSWORD": false},
		log, func() time.Time { return testNow })
	require.NoError(t, err)
	return NewRouter(Deps{Log: log, DB: fakePinger{}, Settings: svc, Now: func() time.Time { return testNow }}), logs
}

const secretValue = "s3cret-value"

func put(t *testing.T, h http.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, target, strings.NewReader(body))
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

	rec := put(t, h, "/api/settings/filler.fallback", `{"value":{"mode":"playlist","source_id":3}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var saved map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &saved))
	require.Equal(t, map[string]any{"mode": "playlist", "source_id": 3.0}, saved["value"])
	require.Equal(t, false, saved["is_default"])
	require.Equal(t, "2026-10-13T06:44:05Z", saved["updated_at"], "UTC")

	rec = do(t, h, http.MethodGet, "/api/settings")
	s := findSetting(t, rec.Body.Bytes(), "filler.fallback")
	require.Equal(t, map[string]any{"mode": "playlist", "source_id": 3.0}, s["value"])
	require.Equal(t, map[string]any{"mode": "random"}, s["default"])

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
		{"filler.fallback", `{"value":{"mode":"artist"}}`, errcodes.Required},
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
