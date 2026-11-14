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
	output playout.StreamOutput
	events chan playout.Event
}

func (f fakePlayout) Callback(_ context.Context, e playout.Event) error {
	f.events <- e
	return nil
}

func (fakePlayout) EmergencyNext(context.Context) (string, bool, error) { return "", false, nil }
func (f fakePlayout) Status(context.Context) (playout.Status, error)    { return f.status, nil }
func (fakePlayout) SetMusicGain(context.Context, float64) error         { return nil }

func (f fakePlayout) Output(context.Context) (playout.StreamOutput, error) { return f.output, nil }

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

// The startup fetch of the Liquidsoap script: every field, always, in the
// encoding of output.set.
func TestPlayoutOutput(t *testing.T) {
	t.Parallel()
	o := playout.StreamOutput{
		Host: "listen.example.org", Port: 8001, Mount: "/atlantida", TLS: true, Codec: "aac", BitrateKbps: 160,
		SampleRateHz: 48000, Channels: 1, Name: "Radio Atlantida", Description: "", Genre: "Jazz",
	}
	rec := do(t, newPlayoutRouter(fakePlayout{output: o}), http.MethodGet, "/api/playout/output")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"host":"listen.example.org","port":8001,"mount":"/atlantida","tls":true,"codec":"aac",
		"bitrate_kbps":160,"sample_rate_hz":48000,"channels":1,"name":"Radio Atlantida","description":"","genre":"Jazz"}`,
		rec.Body.String())
	cmd, err := o.Command()
	require.NoError(t, err)
	arg, ok := strings.CutPrefix(cmd, "output.set ")
	require.True(t, ok)
	require.JSONEq(t, rec.Body.String(), arg, "the endpoint and the command carry the same object")
	require.NotContains(t, strings.ToLower(rec.Body.String()), "password")
}

// The output state in the heartbeat (contract 2): optional fields, and a
// strict body, which is why a new field is a contract change.
func TestPlayoutHeartbeatOutputFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		body          string
		wantCode      int
		wantConnected *bool
		wantSince     *time.Time
	}{
		{
			name:     "contract 2",
			body:     `{"event":"heartbeat","time":"2026-10-15T06:00:05Z","safety_on_air":false,"output_connected":true,"output_since":"2026-10-15T05:00:00Z"}`,
			wantCode: http.StatusNoContent, wantConnected: new(true), wantSince: new(time.Date(2026, 10, 15, 5, 0, 0, 0, time.UTC)),
		},
		{
			name:     "contract 2, not connected, offset time in UTC",
			body:     `{"event":"heartbeat","time":"2026-10-15T06:00:05Z","output_connected":false,"output_since":"2026-10-15T09:00:00+04:00"}`,
			wantCode: http.StatusNoContent, wantConnected: new(false), wantSince: new(time.Date(2026, 10, 15, 5, 0, 0, 0, time.UTC)),
		},
		{name: "contract 1: no fields", body: `{"event":"heartbeat","time":"2026-10-15T06:00:05Z","safety_on_air":true}`, wantCode: http.StatusNoContent},
		{
			name:     "an unknown field is refused",
			body:     `{"event":"heartbeat","time":"2026-10-15T06:00:05Z","output_state":"x"}`,
			wantCode: http.StatusUnprocessableEntity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := fakePlayout{events: make(chan playout.Event, 1)}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/playout/callback", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			newPlayoutRouter(f).ServeHTTP(rec, req)
			require.Equal(t, tt.wantCode, rec.Code, rec.Body.String())
			if tt.wantCode != http.StatusNoContent {
				require.Empty(t, f.events)
				return
			}
			e := <-f.events
			require.Equal(t, playout.EventHeartbeat, e.Kind)
			require.Equal(t, tt.wantConnected, e.OutputConnected)
			require.Equal(t, tt.wantSince, e.OutputSince)
			if e.OutputSince != nil {
				require.Equal(t, time.UTC, e.OutputSince.Location())
			}
		})
	}
}

func TestPlayoutStatusOutput(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		output *playout.Output
		want   string
	}{
		{name: "unknown after a backend start: absent", want: `null`},
		{name: "connected", output: &playout.Output{State: "connected", Since: since}, want: `{"state":"connected","since":"2026-10-15T06:00:00Z"}`},
		{name: "unknown after a silence", output: &playout.Output{State: "unknown", Since: since}, want: `{"state":"unknown","since":"2026-10-15T06:00:00Z"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, newPlayoutRouter(fakePlayout{status: playout.Status{Output: tt.output}}), http.MethodGet, "/api/playout/status")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var got struct {
				Output json.RawMessage `json:"output"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			if tt.want == `null` {
				require.Nil(t, got.Output)
				return
			}
			require.JSONEq(t, tt.want, string(got.Output))
		})
	}
}

func TestPlayoutStatusHeldUntil(t *testing.T) {
	t.Parallel()
	held := time.Date(2026, 10, 15, 6, 5, 59, 500000000, time.UTC)
	tests := []struct {
		name string
		held *time.Time
		want string
	}{
		{name: "nothing held: absent"},
		{name: "held", held: &held, want: `"2026-10-15T06:05:59.5Z"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := do(t, newPlayoutRouter(fakePlayout{status: playout.Status{HeldUntil: tt.held}}), http.MethodGet, "/api/playout/status")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var got map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			v, ok := got["held_until"]
			if tt.want == "" {
				require.False(t, ok, rec.Body.String())
				return
			}
			require.JSONEq(t, tt.want, string(v))
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
