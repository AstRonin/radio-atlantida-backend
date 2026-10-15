//go:build integration

package httpapi_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
)

// fakeLiquidsoap answers the telnet commands playout sends, like the dev
// script: pushes are queued (never played), variables are kept.
type fakeLiquidsoap struct {
	t  *testing.T
	ln net.Listener

	mu     sync.Mutex
	rid    int
	queues map[string][]int
	pushed map[string][]string
	vars   map[string]string
	conns  []net.Conn
}

func newFakeLiquidsoap(t *testing.T) *fakeLiquidsoap {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &fakeLiquidsoap{t: t, ln: ln, queues: map[string][]int{}, pushed: map[string][]string{}, vars: map[string]string{}}
	go f.serve()
	t.Cleanup(f.stop)
	return f
}

func (f *fakeLiquidsoap) addr() string { return f.ln.Addr().String() }

// stop closes the listener and every connection: Liquidsoap is down.
func (f *fakeLiquidsoap) stop() {
	_ = f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		_ = c.Close()
	}
}

func (f *fakeLiquidsoap) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, conn)
		f.mu.Unlock()
		go f.handle(conn)
	}
}

func (f *fakeLiquidsoap) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		reply := f.answer(strings.TrimSpace(line))
		if _, err := io.WriteString(conn, reply+"END\n"); err != nil {
			return
		}
	}
}

func (f *fakeLiquidsoap) answer(cmd string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, arg, _ := strings.Cut(cmd, " ")
	switch {
	case name == "uptime":
		return "0d 00h 01m 00s\n"
	case name == "var.set":
		v, val, _ := strings.Cut(arg, " = ")
		f.vars[v] = val
		return "Variable " + v + " set.\n"
	case strings.HasSuffix(name, ".push"):
		q := strings.TrimSuffix(name, ".push")
		f.rid++
		f.queues[q] = append(f.queues[q], f.rid)
		f.pushed[q] = append(f.pushed[q], arg)
		return strconv.Itoa(f.rid) + "\n"
	case strings.HasSuffix(name, ".queue"):
		var rids []string
		for _, r := range f.queues[strings.TrimSuffix(name, ".queue")] {
			rids = append(rids, strconv.Itoa(r))
		}
		return strings.Join(rids, " ") + "\n"
	}
	return "ERROR: unknown command\n"
}

func (f *fakeLiquidsoap) pushes(queue string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pushed[queue]...)
}

func (f *fakeLiquidsoap) variable(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vars[name]
}

// runPusher runs the playout pusher until the test ends.
func (a *libraryAPI) runPusher() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.playout.Run(ctx)
	}()
	a.t.Cleanup(func() {
		cancel()
		<-done
	})
}

func (a *libraryAPI) callback(body map[string]any) {
	a.t.Helper()
	// Liquidsoap sends every annotation, empty when absent.
	full := map[string]any{"rid": "1", "item_id": "", "track_id": "", "uri": "u", "filename": "/media/f.mp3", "elapsed_ms": 0}
	for k, v := range body {
		full[k] = v
	}
	a.ok(http.StatusNoContent, http.MethodPost, "/api/playout/callback", full, nil)
}

type playLogRow struct {
	Status    string
	Started   time.Time
	Actual    int
	Ended     *time.Time
	Truncated bool
	Source    string
}

func (a *libraryAPI) playLog(where string, args ...any) playLogRow {
	a.t.Helper()
	var r playLogRow
	require.NoError(a.t, a.db.Pool.QueryRow(a.t.Context(), `SELECT status, started_at, actual_duration_ms, ended_at,
       truncated, source FROM play_log WHERE `+where, args...).
		Scan(&r.Status, &r.Started, &r.Actual, &r.Ended, &r.Truncated, &r.Source))
	return r
}

func TestPlayoutPushesAndLogs(t *testing.T) {
	f := newClockFixture(t)
	f.filler()
	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{
		"name":     "Day",
		"elements": []any{element(f.music, 19), element(f.music, 19), element(f.music, 19, "filler_mode", "random")},
	}, &saved)
	date, weekday, start := tomorrowNoon(t)
	var assigned dto.GridAssignResult
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": saved.Clock.ID, "weekdays": []int{weekday}, "hours": []int{12}, "valid_from": date, "valid_to": date,
	}, &assigned)
	f.waitOperation(assigned.BuildOperationID)
	items := f.plan().Items[0].Items
	require.Len(t, items, 4)
	queue := playout.QueueName(start, stationTZ(t))

	// Two minutes before the hour, its first items are pushed (pre-push).
	f.clock.set(start.Add(-2 * time.Minute))
	f.runPusher()
	require.Eventually(t, func() bool { return len(f.liquidsoap.pushes(queue)) == 3 }, 10*time.Second, 20*time.Millisecond)
	require.Equal(t, fmt.Sprintf(`annotate:item_id="%d":/media/music/`, items[0].ID),
		f.liquidsoap.pushes(queue)[0][:len(fmt.Sprintf(`annotate:item_id="%d":/media/music/`, items[0].ID))])
	require.Equal(t, "0.", f.liquidsoap.variable("hour_shift"))
	require.Equal(t, "1.", f.liquidsoap.variable("music_gain"))
	plan := f.plan().Items[0]
	for i, it := range plan.Items {
		require.Equal(t, map[bool]string{true: "queued", false: "planned"}[i < 3], it.Status, i)
	}

	// The hour is frozen: a rebuild leaves it as it is.
	var op dto.OperationAccepted
	f.ok(http.StatusAccepted, http.MethodPost, "/api/track-plan/rebuild", map[string]any{
		"from": start.Format(time.RFC3339), "to": start.Add(time.Hour).Format(time.RFC3339),
	}, &op)
	require.Contains(t, f.waitOperation(&op.OperationID).ResultSummary, "built 0 hours")
	require.Equal(t, plan.Items[0].ID, f.plan().Items[0].Items[0].ID)

	// The first item starts: it moves to the play log, and the fourth item
	// is pushed into the freed slot.
	f.clock.set(start)
	f.callback(map[string]any{
		"event": "start", "time": start.Add(200 * time.Millisecond).Format(time.RFC3339Nano), "source": "plan",
		"item_id": strconv.FormatUint(items[0].ID, 10),
	})
	r := f.playLog(`scheduled_item_id = $1`, items[0].ID)
	require.Equal(t, "aired", r.Status)
	require.Equal(t, start.Add(200*time.Millisecond), r.Started.UTC())
	require.Nil(t, r.Ended)
	require.Equal(t, "auto", r.Source)
	require.Len(t, f.plan().Items[0].Items, 3, "the started item left the plan")
	require.Eventually(t, func() bool { return len(f.liquidsoap.pushes(queue)) == 4 }, 10*time.Second, 20*time.Millisecond)

	var st dto.PlayoutStatus
	f.ok(http.StatusOK, http.MethodGet, "/api/playout/status", nil, &st)
	require.True(t, st.ControlLink.Connected)
	require.NotNil(t, st.NowPlaying)
	require.Equal(t, items[0].ID, *st.NowPlaying.ItemID)
	require.Equal(t, "plan", st.NowPlaying.Source)
	require.Len(t, st.Queued, 3)
	require.Equal(t, queue, st.Queued[0].Queue)
	require.NotNil(t, st.LastCallbackAt)
	require.InDelta(t, 1, *st.MusicGain, 0)

	// It ends cut by a skip: truncated; a duplicate end changes nothing.
	end := map[string]any{
		"event": "end", "time": start.Add(5 * time.Minute).Format(time.RFC3339Nano), "source": "plan",
		"item_id": strconv.FormatUint(items[0].ID, 10), "elapsed_ms": 300000,
	}
	f.callback(end)
	end["elapsed_ms"] = 1000
	f.callback(end)
	r = f.playLog(`scheduled_item_id = $1`, items[0].ID)
	require.Equal(t, "truncated", r.Status)
	require.True(t, r.Truncated)
	require.Equal(t, 300000, r.Actual)
	require.Equal(t, start.Add(200*time.Millisecond+5*time.Minute), r.Ended.UTC())

	// The second item's start was lost; its end logs it.
	f.callback(map[string]any{
		"event": "end", "time": start.Add(25 * time.Minute).Format(time.RFC3339Nano), "source": "plan",
		"item_id": strconv.FormatUint(items[1].ID, 10), "elapsed_ms": 1140000,
	})
	r = f.playLog(`scheduled_item_id = $1`, items[1].ID)
	require.Equal(t, "aired", r.Status)
	require.Equal(t, start.Add(6*time.Minute), r.Started.UTC())

	// The Icecast output state.
	f.callback(map[string]any{"event": "output", "time": start.Format(time.RFC3339Nano), "connected": false})
	f.ok(http.StatusOK, http.MethodGet, "/api/playout/status", nil, &st)
	require.Equal(t, "disconnected", st.Output.State)
	// ... and the broadcast state derived from it, stored with its since.
	var bs dto.BroadcastStatus
	f.ok(http.StatusOK, http.MethodGet, "/api/broadcast/status", nil, &bs)
	require.Equal(t, "reconnecting", bs.State)
	require.Equal(t, start, bs.Since.UTC())
	var stored string
	require.NoError(t, f.db.Pool.QueryRow(t.Context(), `SELECT state FROM broadcast_state`).Scan(&stored))
	require.Equal(t, "reconnecting", stored)
}

func TestPlayoutMusicGain(t *testing.T) {
	a := newLibraryAPI(t)
	var g dto.MusicGain
	a.ok(http.StatusOK, http.MethodPut, "/api/playout/music-gain", map[string]any{"gain": 0.2}, &g)
	require.InDelta(t, 0.2, g.Gain, 0)
	require.Equal(t, "0.2", a.liquidsoap.variable("music_gain"))
	a.fieldError("gain", errcodes.Range, http.MethodPut, "/api/playout/music-gain", map[string]any{"gain": 1.5})

	a.liquidsoap.stop()
	a.problem(http.StatusServiceUnavailable, errcodes.LiquidsoapUnavailable, http.MethodPut, "/api/playout/music-gain",
		map[string]any{"gain": 0})
}

func TestPlayoutEmergency(t *testing.T) {
	a := newLibraryAPI(t)
	rep := a.call(http.MethodGet, "/api/playout/emergency/next", nil, nil)
	require.Equal(t, http.StatusNoContent, rep.status, "no emergency playlist")

	var pl dto.Playlist
	a.ok(http.StatusCreated, http.MethodPost, "/api/playlists", map[string]any{"name": "Emergency", "type_id": a.typeID("music")}, &pl)
	a.emergency.set(pl.ID)
	rep = a.call(http.MethodGet, "/api/playout/emergency/next", nil, nil)
	require.Equal(t, http.StatusNoContent, rep.status, "an empty playlist")

	track := a.exec(`INSERT INTO tracks (title, file_path, type_id, duration_ms) VALUES ('Safe', 'safe/s.mp3', $1, 60000) RETURNING id`,
		a.typeID("music"))
	a.exec(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2) RETURNING track_id`, track, pl.ID)
	rep = a.call(http.MethodGet, "/api/playout/emergency/next", nil, nil)
	require.Equal(t, http.StatusOK, rep.status)
	require.Equal(t, fmt.Sprintf(`annotate:track_id="%d":/media/safe/s.mp3`, track), string(rep.body))
	require.Contains(t, rep.header.Get("Content-Type"), "text/plain")

	at := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	a.callback(map[string]any{"event": "start", "time": at.Format(time.RFC3339Nano), "source": "emergency", "track_id": strconv.FormatUint(track, 10)})
	var st dto.PlayoutStatus
	a.ok(http.StatusOK, http.MethodGet, "/api/playout/status", nil, &st)
	require.Equal(t, "emergency", st.NowPlaying.Source)
	a.callback(map[string]any{
		"event": "end", "time": at.Add(10 * time.Second).Format(time.RFC3339Nano), "source": "emergency",
		"track_id": strconv.FormatUint(track, 10), "elapsed_ms": 10000,
	})
	r := a.playLog(`source = 'emergency'`)
	require.Equal(t, "aired", r.Status)
	require.Equal(t, at, r.Started.UTC())
	require.Equal(t, 10000, r.Actual)
}

func TestPlayoutCallbackRefusals(t *testing.T) {
	a := newLibraryAPI(t)
	a.fieldError("event", errcodes.Enum, http.MethodPost, "/api/playout/callback",
		map[string]any{"event": "pause", "time": "2026-10-15T08:00:00Z"})
	a.fieldError("item_id", errcodes.Pattern, http.MethodPost, "/api/playout/callback",
		map[string]any{"event": "start", "time": "2026-10-15T08:00:00Z", "source": "plan", "item_id": "x1"})
	// An item that is not in the plan is ignored.
	a.callback(map[string]any{"event": "start", "time": "2026-10-15T08:00:00Z", "source": "plan", "item_id": "424242"})
}
