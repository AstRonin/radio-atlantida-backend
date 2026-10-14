package liquidsoap_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
)

// fakeServer answers telnet commands like Liquidsoap: reply lines, then END.
type fakeServer struct {
	ln net.Listener

	mu       sync.Mutex
	commands []string
	replies  map[string][]string
	// silent commands get no reply at all.
	silent map[string]bool
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &fakeServer{ln: ln, replies: map[string][]string{}, silent: map[string]bool{}}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

func (s *fakeServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeServer) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		s.mu.Lock()
		s.commands = append(s.commands, cmd)
		reply, silent := s.replies[cmd], s.silent[cmd]
		s.mu.Unlock()
		if silent {
			continue
		}
		for _, l := range reply {
			_, _ = conn.Write([]byte(l + "\n"))
		}
		_, _ = conn.Write([]byte("END\n"))
	}
}

func (s *fakeServer) reply(cmd string, lines ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies[cmd] = lines
}

func (s *fakeServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func newClient(addr string, states *[]liquidsoap.State) *liquidsoap.Client {
	return liquidsoap.New(
		func(context.Context) (string, error) { return addr, nil },
		time.Now,
		func(s liquidsoap.State) { *states = append(*states, s) },
	)
}

func TestClientCommands(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(t)
	srv.reply(`hour_1.push annotate:item_id="7":/media/a.mp3`, "12")
	srv.reply("hour_1.queue", "12 13")
	srv.reply("hour_0.queue", "")
	srv.reply("var.set music_gain = 0.2", "Variable music_gain set.")
	srv.reply("uptime", "0d 00h 01m 02s")

	var states []liquidsoap.State
	c := newClient(srv.ln.Addr().String(), &states)
	ctx := t.Context()

	rid, err := c.Push(ctx, "hour_1", `annotate:item_id="7":/media/a.mp3`)
	require.NoError(t, err)
	require.Equal(t, 12, rid)

	rids, err := c.Queue(ctx, "hour_1")
	require.NoError(t, err)
	require.Equal(t, []int{12, 13}, rids)
	rids, err = c.Queue(ctx, "hour_0")
	require.NoError(t, err)
	require.Empty(t, rids)

	require.NoError(t, c.SetVar(ctx, "music_gain", liquidsoap.FormatFloat(0.2)))
	require.NoError(t, c.Ping(ctx))

	require.Equal(t, uint64(1), c.Generation(), "one connection for every command")
	require.True(t, c.State().Connected)
	require.False(t, c.State().LastOK.IsZero())
	require.Len(t, states, 1)
	require.True(t, states[0].Connected)
}

func TestClientUnexpectedReplies(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(t)
	srv.reply("hour_0.push /media/x.mp3", "Unable to parse URI")
	srv.reply("var.set nope = 1.", "Variable nope not found.")

	var states []liquidsoap.State
	c := newClient(srv.ln.Addr().String(), &states)
	_, err := c.Push(t.Context(), "hour_0", "/media/x.mp3")
	require.Error(t, err)
	require.Error(t, c.SetVar(t.Context(), "nope", "1."))
	require.True(t, c.State().Connected, "an error reply keeps the link")

	_, err = c.Command(t.Context(), "hour_0.push a\nshutdown")
	require.Error(t, err, "a line break would inject a second command")
	require.NotContains(t, srv.seen(), "shutdown")
}

func TestClientUnavailable(t *testing.T) {
	t.Parallel()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	var states []liquidsoap.State
	c := newClient(addr, &states)
	err = c.Ping(t.Context())
	require.ErrorIs(t, err, liquidsoap.ErrUnavailable)
	require.False(t, c.State().Connected)
	require.Len(t, states, 1)
	require.Equal(t, uint64(0), c.Generation())
}

func TestClientTimeoutReconnects(t *testing.T) {
	t.Parallel()
	srv := newFakeServer(t)
	srv.mu.Lock()
	srv.silent["uptime"] = true
	srv.mu.Unlock()
	srv.reply("hour_0.queue", "1")

	var states []liquidsoap.State
	c := newClient(srv.ln.Addr().String(), &states)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, c.Ping(ctx), liquidsoap.ErrUnavailable)
	require.False(t, c.State().Connected)

	rids, err := c.Queue(t.Context(), "hour_0")
	require.NoError(t, err)
	require.Equal(t, []int{1}, rids)
	require.Equal(t, uint64(2), c.Generation(), "a broken connection is replaced")
	require.Len(t, states, 3) // up, down, up
}

func TestFormatFloat(t *testing.T) {
	t.Parallel()
	for in, want := range map[float64]string{0: "0.", 1: "1.", 0.2: "0.2", 2.5: "2.5", 1800: "1800.", 123.456: "123.456"} {
		require.Equal(t, want, liquidsoap.FormatFloat(in))
	}
}
