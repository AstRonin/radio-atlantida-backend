package outage

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

type recorder struct {
	events []playout.Event
	err    error
}

func (r *recorder) Replay(_ context.Context, e playout.Event) error {
	r.events = append(r.events, e)
	return r.err
}

func TestJournalReplayReadsTheWindowAcrossDays(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	write("journal-2026-10-13.jsonl", `{"event":"start","time":"2026-10-13T23:50:00.000Z","source":"plan","item_id":"1","track_id":""}
{"event":"start","time":"2026-10-13T23:58:00.000Z","source":"plan","item_id":"2","track_id":""}
not json
{"event":"output","time":"2026-10-13T23:59:00.000Z","connected":false}
`)
	write("journal-2026-10-14.jsonl", `{"event":"end","time":"2026-10-14T00:01:00.500Z","source":"plan","item_id":"2","track_id":"","elapsed_ms":180500}

{"event":"end","time":"2026-10-14T00:02:00.000Z","source":"emergency","item_id":"","track_id":"7","elapsed_ms":60000}
{"event":"start","time":"2026-10-14T02:00:00.000Z","source":"plan","item_id":"3","track_id":""}
`)
	rec := &recorder{}
	j := NewJournal(dir, rec, slog.New(slog.DiscardHandler))

	res, err := j.Replay(t.Context(), time.Date(2026, 10, 13, 23, 55, 0, 0, time.UTC), time.Date(2026, 10, 14, 1, 0, 0, 0, time.UTC))

	require.NoError(t, err)
	require.Equal(t, ReplayResult{Lines: 3, Bad: 1}, res)
	require.Len(t, rec.events, 3)
	require.Equal(t, playout.EventStart, rec.events[0].Kind)
	require.Equal(t, uint64(2), *rec.events[0].ItemID)
	require.Nil(t, rec.events[0].TrackID)
	require.Equal(t, playout.EventEnd, rec.events[1].Kind)
	require.Equal(t, 180500, rec.events[1].ElapsedMS)
	require.Equal(t, time.Date(2026, 10, 14, 0, 1, 0, 500_000_000, time.UTC), rec.events[1].Time)
	require.Equal(t, playout.SourceEmergency, rec.events[2].Source)
	require.Equal(t, uint64(7), *rec.events[2].TrackID)
}

func TestJournalReplayMissingFilesAndErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rec := &recorder{}
	j := NewJournal(dir, rec, slog.New(slog.DiscardHandler))
	res, err := j.Replay(t.Context(), t0.Add(-48*time.Hour), t0)
	require.NoError(t, err)
	require.Zero(t, res.Lines)

	require.NoError(t, os.WriteFile(filepath.Join(dir, JournalFile(t0)),
		[]byte(`{"event":"start","time":"2026-10-14T09:00:00Z","source":"plan","item_id":"1"}`+"\n"), 0o600))
	rec.err = errors.New("db down")
	_, err = j.Replay(t.Context(), t0.Add(-time.Hour), t0)
	require.ErrorContains(t, err, "db down")

	_, err = NewJournal(filepath.Join(dir, "absent"), rec, slog.New(slog.DiscardHandler)).Replay(t.Context(), t0, t0)
	require.Error(t, err)
}

func TestHasMount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"one source", `{"icestats":{"source":{"listenurl":"http://icecast:8000/radio"}}}`, true},
		{"several", `{"icestats":{"source":[{"listenurl":"http://x:8000/other"},{"listenurl":"http://x:8000/radio"}]}}`, true},
		{"other mount", `{"icestats":{"source":{"listenurl":"http://x:8000/radio2"}}}`, false},
		{"no source", `{"icestats":{"admin":"a@b"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := hasMount([]byte(tt.body), "/radio")
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
	_, err := hasMount([]byte(`<html>`), "/radio")
	require.Error(t, err)
}

func TestIcecastChecker(t *testing.T) {
	t.Parallel()
	body := `{"icestats":{"source":{"listenurl":"http://127.0.0.1/radio"}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/status-json.xsl", r.URL.Path)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.Listener.Addr().String())
	c := NewIcecastChecker(srv.Client())

	require.NoError(t, c.Check(t.Context(), IcecastTarget{Host: host, Port: port, Mount: "/radio"}))
	err := c.Check(t.Context(), IcecastTarget{Host: host, Port: port, Mount: "/live"})
	require.ErrorIs(t, err, errMountMissing)
	srv.Close()
	require.Error(t, c.Check(t.Context(), IcecastTarget{Host: host, Port: port, Mount: "/radio"}))
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, p, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(p)
	require.NoError(t, err)
	return host, port
}

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()
	c := Cursor{SortTime: time.Date(2026, 10, 14, 9, 0, 0, 123456000, time.UTC), ID: 42}
	got, err := ParseCursor(c.String())
	require.NoError(t, err)
	require.Equal(t, c, got)

	for _, bad := range []string{"", "!!", "bm9wZQ", "MjAyNl8x"} {
		_, err := ParseCursor(bad)
		var v *validation.Error
		require.ErrorAs(t, err, &v, bad)
		require.Equal(t, "query.cursor", v.Fields[0].Field)
	}
}

func TestServiceRanges(t *testing.T) {
	t.Parallel()
	svc := NewService(&fakeStore{}, func() time.Time { return t0 })
	_, _, err := svc.History(t.Context(), HistoryFilter{From: t0, To: t0, Limit: 10})
	require.ErrorContains(t, err, "query.to")
	_, _, err = svc.History(t.Context(), HistoryFilter{From: t0, To: t0.Add(32 * 24 * time.Hour), Limit: 10})
	require.ErrorContains(t, err, "31 days")
	_, _, err = svc.History(t.Context(), HistoryFilter{From: t0, To: t0.Add(time.Hour), Limit: 0})
	require.ErrorContains(t, err, "query.limit")
	_, err = svc.Outages(t.Context(), t0, t0.Add(8*24*time.Hour))
	require.ErrorContains(t, err, "7 days")
}
