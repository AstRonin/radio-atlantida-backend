//go:build integration

package httpapi_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
)

// airFixture is a station with history and a plan around now: twelve aired
// items, one missed, and five plan items from a minute ahead on (the test
// clock moves on, so an item planned at now would have passed).
type airFixture struct {
	*libraryAPI
	now   time.Time
	track uint64
	// items are the plan items, in planned order.
	items []uint64
}

func newAirFixture(t *testing.T) *airFixture {
	t.Helper()
	f := &airFixture{libraryAPI: newLibraryAPI(t), now: time.Now().UTC().Truncate(time.Second)}
	f.clock.set(f.now)
	ctx := t.Context()
	require.NoError(t, f.db.Pool.QueryRow(ctx, `WITH a AS (INSERT INTO artists (name) VALUES ('A') RETURNING id)
INSERT INTO tracks (title, artist_id, duration_ms, file_path, type_id)
SELECT 'T', a.id, 180000, 'music/t.mp3', pt.id FROM a, playlist_types pt WHERE pt.code = 'music' RETURNING id`).
		Scan(&f.track))
	var clock, outage uint64
	require.NoError(t, f.db.Pool.QueryRow(ctx, `INSERT INTO clocks (name) VALUES ('C') RETURNING id`).Scan(&clock))
	require.NoError(t, f.db.Pool.QueryRow(ctx, `INSERT INTO broadcast_outages (started_at, ended_at, detected_by, component)
VALUES ($1, $1, 'icecast', 'icecast') RETURNING id`, f.now.Add(-5*time.Minute)).Scan(&outage))
	// Aired every 3 minutes until 6 minutes ago; missed 3 minutes ago.
	_, err := f.db.Pool.Exec(ctx, `INSERT INTO play_log (track_id, status, planned_start_utc, started_at, planned_out_ms,
                      actual_duration_ms)
SELECT $1, 'aired', g, g, 180000, 180000
FROM generate_series($2::timestamptz - interval '39 minutes', $2::timestamptz - interval '6 minutes', interval '3 minutes') g`,
		f.track, f.now)
	require.NoError(t, err)
	_, err = f.db.Pool.Exec(ctx, `INSERT INTO play_log (track_id, status, planned_start_utc, planned_out_ms, outage_id, resolution)
VALUES ($1, 'missed', $2, 180000, $3, 'open')`, f.track, f.now.Add(-3*time.Minute), outage)
	require.NoError(t, err)
	rows, err := f.db.Pool.Query(ctx, `INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source)
SELECT $1, $2, g, 180000, 'fill'
FROM generate_series($3::timestamptz + interval '1 minute', $3::timestamptz + interval '13 minutes', interval '3 minutes') g
RETURNING id`, clock, f.track, f.now)
	require.NoError(t, err)
	for rows.Next() {
		var id uint64
		require.NoError(t, rows.Scan(&id))
		f.items = append(f.items, id)
	}
	require.NoError(t, rows.Err())
	return f
}

func (f *airFixture) window() dto.AirWindow {
	f.t.Helper()
	var w dto.AirWindow
	f.ok(http.StatusOK, http.MethodGet, "/api/air-window", nil, &w)
	return w
}

// kinds returns kind:status of every row.
func kinds(w dto.AirWindow) []string {
	out := make([]string, len(w.Rows))
	for i, r := range w.Rows {
		out[i] = r.Kind + ":" + r.Status
	}
	return out
}

func TestAirWindowControlLinkDown(t *testing.T) {
	f := newAirFixture(t)

	// The pusher never ran: the control link is down.
	w := f.window()
	require.False(t, w.ControlLinkConnected)
	want := make([]string, 0, 15)
	for range 9 {
		want = append(want, "played:aired")
	}
	want = append(want, "played:missed")
	for range 5 {
		want = append(want, "queued:planned")
	}
	require.Equal(t, want, kinds(w), "10 played, no now row, the queued rows from now on")

	missed := w.Rows[9]
	require.Equal(t, f.now.Add(-3*time.Minute), missed.StartUTC)
	require.Equal(t, f.now, *missed.EndUTC, "the planned end")
	require.Equal(t, "open", *missed.Resolution)
	require.NotNil(t, missed.OutageID)
	require.Equal(t, f.items[0], *w.Rows[10].ItemID)
	require.Equal(t, f.now.Add(time.Minute), w.Rows[10].StartUTC)
	require.False(t, w.Rows[10].InLiquidsoap)

	// A changed count shows on the next call; out of range is refused.
	f.ok(http.StatusOK, http.MethodPut, "/api/settings/air.played_count", map[string]any{"value": 2}, nil)
	f.ok(http.StatusOK, http.MethodPut, "/api/settings/air.queued_count", map[string]any{"value": 0}, nil)
	require.Equal(t, []string{"played:aired", "played:missed"}, kinds(f.window()))
	p := f.problem(http.StatusUnprocessableEntity, errcodes.ValidationFailed, http.MethodPut, "/api/settings/air.played_count",
		map[string]any{"value": 51})
	require.Len(t, p.Errors, 1)
	require.Equal(t, "value", p.Errors[0].Field)
}

func TestAirWindowOnAir(t *testing.T) {
	f := newAirFixture(t)
	f.runPusher()

	// The first plan item starts: it moves to the play log and is the now
	// row, not a played row.
	started := f.now.Add(time.Minute + 500*time.Millisecond)
	f.callback(map[string]any{
		"event": "start", "time": started.Format(time.RFC3339Nano), "source": "plan",
		"item_id": strconv.FormatUint(f.items[0], 10),
	})
	require.Eventually(t, func() bool { return f.window().ControlLinkConnected }, 10*time.Second, 20*time.Millisecond)
	w := f.window()
	require.Len(t, w.Rows, 10+1+4)
	now := w.Rows[10]
	require.Equal(t, "now", now.Kind)
	require.Equal(t, f.items[0], *now.ItemID)
	require.Equal(t, started, now.StartUTC)
	require.Equal(t, started.Add(3*time.Minute), *now.EndUTC)
	require.True(t, now.InLiquidsoap)
	require.Equal(t, "played:missed", kinds(w)[9], "the newest played row is the missed one")
	for i, r := range w.Rows[11:] {
		require.Equal(t, f.items[i+1], *r.ItemID)
		require.Equal(t, "queued", r.Kind)
	}
	// The pusher keeps three items of the hour in Liquidsoap.
	require.Eventually(t, func() bool {
		q := f.window().Rows[11:]
		return q[0].InLiquidsoap && q[0].Status == "queued"
	}, 10*time.Second, 20*time.Millisecond)

	// It ends cut short: no now row, and it is the newest played row,
	// truncated, with the length that aired.
	f.callback(map[string]any{
		"event": "end", "time": started.Add(time.Minute).Format(time.RFC3339Nano), "source": "plan",
		"item_id": strconv.FormatUint(f.items[0], 10), "elapsed_ms": 60000,
	})
	w = f.window()
	require.Len(t, w.Rows, 10+4)
	last := w.Rows[9]
	require.Equal(t, "played:truncated", last.Kind+":"+last.Status)
	require.Equal(t, started, last.StartUTC)
	require.Equal(t, started.Add(time.Minute), *last.EndUTC)
	require.Equal(t, "queued", w.Rows[10].Kind)
}
