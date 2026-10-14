package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/handlers"
	"github.com/AstRonin/radio-atlantida-backend/internal/live"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
)

type fakeLive struct {
	err      error
	offer    string
	takeover bool
	stopped  bool
	status   live.Status
}

func (f *fakeLive) Start(_ context.Context, offer string, takeover bool) (live.Answer, error) {
	f.offer, f.takeover = offer, takeover
	if f.err != nil {
		return live.Answer{}, f.err
	}
	return live.Answer{SessionID: 4, SDP: "v=0 answer"}, nil
}

func (f *fakeLive) Stop()               { f.stopped = true }
func (f *fakeLive) Status() live.Status { return f.status }
func (f *fakeLive) Available() bool     { return true }

// outagesAPI is embedded under another name: the field may not be called
// Outages, which is also one of its methods.
type outagesAPI = handlers.Outages

// fakeOutages implements only OpenOutage; the other methods are not called.
type fakeOutages struct {
	outagesAPI
	open *outage.Outage
	err  error
}

func (f fakeOutages) OpenOutage(context.Context) (outage.Outage, bool, error) {
	if f.open == nil {
		return outage.Outage{}, false, f.err
	}
	return *f.open, true, f.err
}

// fakeGrid implements only Hours.
type fakeGrid struct {
	handlers.Grid
	hours []grid.ScheduledHour
}

func (f fakeGrid) Hours(_ context.Context, from, to time.Time) ([]grid.ScheduledHour, error) {
	var out []grid.ScheduledHour
	for _, h := range f.hours {
		if !h.StartUTC.Before(from) && h.StartUTC.Before(to) {
			out = append(out, h)
		}
	}
	return out, nil
}

func newLiveRouter(lv handlers.Live, o handlers.Outages, g handlers.Grid) http.Handler {
	return NewRouter(Deps{
		Log: slog.New(logging.NewHandler(&syncBuffer{}, slog.LevelDebug)), DB: fakePinger{},
		Live: lv, Outages: o, Grid: g, Now: func() time.Time { return testNow },
	})
}

func doJSONBody(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLiveSessionEndpoints(t *testing.T) {
	t.Parallel()
	lv := &fakeLive{}
	h := newLiveRouter(lv, nil, nil)

	rec := doJSONBody(t, h, http.MethodPost, "/api/live/session", `{"offer":"v=0 offer","takeover":true}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"session_id":4,"answer":"v=0 answer"}`, rec.Body.String())
	require.Equal(t, "v=0 offer", lv.offer)
	require.True(t, lv.takeover)

	rec = do(t, h, http.MethodDelete, "/api/live/session")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.True(t, lv.stopped)

	since := time.Date(2026, 10, 13, 6, 40, 0, 0, time.UTC)
	lv.status = live.Status{State: live.StateOnAir, SessionID: new(uint64(4)), OnAirSince: &since, HarborConnected: true, Packets: 50}
	rec = do(t, h, http.MethodGet, "/api/live/status")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"available":true,"state":"on_air","session_id":4,"on_air_since":"2026-10-13T06:40:00Z",
		"harbor_connected":true,"packets":50}`, rec.Body.String())
}

func TestLiveSessionRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		body   string
		err    error
		status int
		code   string
	}{
		{"active", `{"offer":"v=0"}`, live.ErrSessionActive, http.StatusConflict, "live_session_active"},
		{"invalid offer", `{"offer":"x"}`, &live.OfferError{Msg: "no audio section with Opus"}, http.StatusUnprocessableEntity, "live_offer_invalid"},
		{"unavailable", `{"offer":"v=0"}`, live.ErrUnavailable, http.StatusServiceUnavailable, "live_unavailable"},
		{"empty offer", `{"offer":""}`, nil, http.StatusUnprocessableEntity, "validation_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLiveRouter(&fakeLive{err: tc.err}, nil, nil)
			rec := doJSONBody(t, h, http.MethodPost, "/api/live/session", tc.body)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), `"code":"`+tc.code+`"`)
		})
	}
}

func TestLiveStatusKnown(t *testing.T) {
	t.Parallel()
	hour := testNow.UTC().Truncate(time.Hour) // 06:00Z, 10:00 in Yerevan
	g := fakeGrid{hours: []grid.ScheduledHour{
		{StartUTC: hour.Add(-time.Hour), Clock: grid.ClockRef{Name: "Morning"}},
		{StartUTC: hour, Clock: grid.ClockRef{Name: "Day"}},
	}}
	lv := &fakeLive{status: live.Status{State: live.StateReconnecting}}

	rec := do(t, newLiveRouter(lv, fakeOutages{open: &outage.Outage{ID: 9}}, g), http.MethodGet, "/api/status/live")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"on_air":true,"open_outage_id":9,"current_clock":"Day","time":"2026-10-13T06:44:05Z"}`, rec.Body.String())

	// No session, no outage, no clock this hour; a failing source is unknown.
	lv.status = live.Status{State: live.StateOff}
	rec = do(t, newLiveRouter(lv, fakeOutages{err: errors.New("db down")}, fakeGrid{}), http.MethodGet, "/api/status/live")
	require.JSONEq(t, `{"on_air":false,"open_outage_id":null,"current_clock":null,"time":"2026-10-13T06:44:05Z"}`, rec.Body.String())
}
