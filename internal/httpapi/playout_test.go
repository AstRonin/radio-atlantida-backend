package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
)

// fakePlayout returns a fixed status and records the callbacks.
type fakePlayout struct {
	status playout.Status
	events chan playout.Event
}

func (f fakePlayout) Callback(_ context.Context, e playout.Event) error {
	f.events <- e
	return nil
}

func (fakePlayout) EmergencyNext(context.Context) (string, bool, error) { return "", false, nil }
func (f fakePlayout) Status(context.Context) (playout.Status, error)    { return f.status, nil }
func (fakePlayout) SetMusicGain(context.Context, float64) error         { return nil }

func newPlayoutRouter(f fakePlayout) http.Handler {
	return NewRouter(Deps{
		Log: slog.New(logging.NewHandler(&syncBuffer{}, slog.LevelDebug)), DB: fakePinger{},
		Playout: f, Now: func() time.Time { return testNow },
	})
}

func TestPlayoutStatusSafety(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		status    playout.Status
		wantOnAir bool
		wantSince *string
	}{
		{name: "not on air", wantOnAir: false},
		{
			name: "on air", status: playout.Status{SafetyOnAir: true, SafetySince: &since},
			wantOnAir: true, wantSince: new("2026-10-15T06:00:00Z"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, newPlayoutRouter(fakePlayout{status: tt.status}), http.MethodGet, "/api/playout/status")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var got struct {
				SafetyOnAir *bool   `json:"safety_on_air"`
				SafetySince *string `json:"safety_since"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.NotNil(t, got.SafetyOnAir, "always present")
			require.Equal(t, tt.wantOnAir, *got.SafetyOnAir)
			require.Equal(t, tt.wantSince, got.SafetySince)
		})
	}
}

func TestPlayoutHeartbeatSafetyField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want *bool
	}{
		{name: "true", body: `{"event":"heartbeat","time":"2026-10-15T06:00:00Z","safety_on_air":true}`, want: new(true)},
		{name: "false", body: `{"event":"heartbeat","time":"2026-10-15T06:00:00Z","safety_on_air":false}`, want: new(false)},
		{name: "missing means unknown", body: `{"event":"heartbeat","time":"2026-10-15T06:00:00Z"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := fakePlayout{events: make(chan playout.Event, 1)}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/playout/callback", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			newPlayoutRouter(f).ServeHTTP(rec, req)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			e := <-f.events
			require.Equal(t, playout.EventHeartbeat, e.Kind)
			require.Equal(t, tt.want, e.SafetyOnAir)
		})
	}
}
