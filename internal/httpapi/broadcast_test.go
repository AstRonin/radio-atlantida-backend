package httpapi

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/broadcast"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

type fakeBroadcast struct{ status broadcast.Status }

func (f fakeBroadcast) Status() broadcast.Status { return f.status }

func TestBroadcastStatus(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 15, 6, 0, 0, 0, time.UTC)
	check := since.Add(time.Hour)
	tests := []struct {
		name   string
		status broadcast.Status
		want   string
	}{
		{
			name:   "unknown after a start",
			status: broadcast.Status{State: broadcast.StateUnknown},
			want:   `{"state":"unknown","since":null,"mount_ok":null,"last_check_at":null,"time":"2026-10-13T06:44:05Z"}`,
		},
		{
			name:   "connected",
			status: broadcast.Status{State: broadcast.StateConnected, Since: &since, MountOK: new(true), LastCheckAt: &check},
			want: `{"state":"connected","since":"2026-10-15T06:00:00Z","mount_ok":true,"last_check_at":"2026-10-15T07:00:00Z",
				"time":"2026-10-13T06:44:05Z"}`,
		},
		{
			name: "unreachable",
			status: broadcast.Status{
				State: broadcast.StateUnreachable, Since: &since, Reason: new("mount not found"), OutageID: new(uint64(7)),
				MountOK: new(false), LastCheckAt: &check,
			},
			want: `{"state":"unreachable","since":"2026-10-15T06:00:00Z","reason":"mount not found","outage_id":7,
				"mount_ok":false,"last_check_at":"2026-10-15T07:00:00Z","time":"2026-10-13T06:44:05Z"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := NewRouter(Deps{
				Log: slog.New(logging.NewHandler(&syncBuffer{}, slog.LevelDebug)), DB: fakePinger{},
				Broadcast: fakeBroadcast{status: tt.status}, Now: func() time.Time { return testNow },
			})
			rec := do(t, h, http.MethodGet, "/api/broadcast/status")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.JSONEq(t, tt.want, rec.Body.String())
		})
	}
}
