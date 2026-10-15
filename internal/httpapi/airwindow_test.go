package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/airwindow"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

type fakeAirWindow struct{ w airwindow.Window }

func (f fakeAirWindow) Window(context.Context) (airwindow.Window, error) { return f.w, nil }

func TestAirWindowShape(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	h := NewRouter(Deps{
		Log: slog.New(logging.NewHandler(&syncBuffer{}, slog.LevelDebug)), DB: fakePinger{}, Now: func() time.Time { return testNow },
		AirWindow: fakeAirWindow{w: airwindow.Window{
			GeneratedAt: start.Add(5 * time.Minute), ControlLinkConnected: true,
			Rows: []airwindow.Row{
				{
					Kind: airwindow.KindPlayed, Status: airwindow.StatusMissed, ItemID: new(uint64(7)), TrackID: 1, Title: "M",
					Source: airwindow.SourcePlan, Start: start, End: new(start.Add(3 * time.Minute)),
					Resolution: new("open"), OutageID: new(uint64(2)),
				},
				{
					Kind: airwindow.KindNow, Status: airwindow.StatusNow, TrackID: 3, Title: "E", ArtistName: new("A"),
					Source: airwindow.SourceEmergency, Start: start.Add(3 * time.Minute), InLiquidsoap: true,
				},
			},
		}},
	})
	rec := do(t, h, http.MethodGet, "/api/air-window")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"generated_at":"2026-10-15T08:05:00Z","control_link_connected":true,"rows":[
		{"kind":"played","status":"missed","item_id":7,"track_id":1,"title":"M","artist_name":null,"source":"plan",
		 "start_utc":"2026-10-15T08:00:00Z","end_utc":"2026-10-15T08:03:00Z","truncated":false,"resolution":"open",
		 "outage_id":2,"in_liquidsoap":false},
		{"kind":"now","status":"now","item_id":null,"track_id":3,"title":"E","artist_name":"A","source":"emergency",
		 "start_utc":"2026-10-15T08:03:00Z","end_utc":null,"truncated":false,"resolution":null,"outage_id":null,
		 "in_liquidsoap":true}]}`, rec.Body.String())
}

func TestAirWindowEmptyRows(t *testing.T) {
	t.Parallel()
	h := NewRouter(Deps{
		Log: slog.New(logging.NewHandler(&syncBuffer{}, slog.LevelDebug)), DB: fakePinger{}, Now: func() time.Time { return testNow },
		AirWindow: fakeAirWindow{w: airwindow.Window{GeneratedAt: testNow}},
	})
	rec := do(t, h, http.MethodGet, "/api/air-window")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"generated_at":"2026-10-13T06:44:05Z","control_link_connected":false,"rows":[]}`, rec.Body.String())
}
