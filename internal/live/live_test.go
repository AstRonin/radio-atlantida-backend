package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// opusSilence is a valid Opus packet (CELT, 20 ms, silence).
var opusSilence = []byte{0xf8, 0xff, 0xfe}

// fakeHarbor is Liquidsoap's harbor input: it checks the source request
// and records what the source sends.
type fakeHarbor struct {
	ln     net.Listener
	refuse atomic.Bool // answer 401

	mu       sync.Mutex
	requests []string // request lines
	auth     []string
	streams  [][]byte // one per accepted connection
	open     int
}

func newFakeHarbor(t *testing.T) *fakeHarbor {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	h := &fakeHarbor{ln: ln}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { h.serve(c) })
		}
	})
	return h
}

func (h *fakeHarbor) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	tp := textproto.NewReader(r)
	line, err := tp.ReadLine()
	if err != nil {
		return
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		return
	}
	h.mu.Lock()
	h.requests = append(h.requests, line)
	h.auth = append(h.auth, hdr.Get("Authorization"))
	h.mu.Unlock()
	if h.refuse.Load() {
		_, _ = io.WriteString(c, "HTTP/1.0 401 Unauthorized\r\n\r\n")
		return
	}
	_, _ = io.WriteString(c, "HTTP/1.0 200 OK\r\n\r\n")
	h.mu.Lock()
	idx := len(h.streams)
	h.streams = append(h.streams, nil)
	h.open++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.open--
		h.mu.Unlock()
	}()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			h.mu.Lock()
			h.streams[idx] = append(h.streams[idx], buf[:n]...)
			h.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (h *fakeHarbor) target() HarborTarget {
	addr := h.ln.Addr().(*net.TCPAddr)
	return HarborTarget{Host: "127.0.0.1", Port: addr.Port, User: "source", Password: "secret"}
}

// pages counts the Ogg pages received on all connections.
func (h *fakeHarbor) pages() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.streams {
		n += bytes.Count(s, []byte("OggS"))
	}
	return n
}

func (h *fakeHarbor) openConns() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.open
}

type events struct {
	mu   sync.Mutex
	list []ws.LiveStateData
}

func (e *events) Publish(ev any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev.(ws.LiveStateEvent).Data)
}

// wait waits until the published states are want.
func (e *events) wait(t *testing.T, want []string) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, want, e.states())
	}, wait, 10*time.Millisecond)
}

func (e *events) states() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.list))
	for i, d := range e.list {
		out[i] = d.State
		if d.Reason != nil {
			out[i] += ":" + *d.Reason
		}
	}
	return out
}

var testTimeouts = Timeouts{
	NoAudio:     time.Second,
	Dropout:     600 * time.Millisecond,
	HarborRetry: 50 * time.Millisecond,
	Gather:      2 * time.Second,
}

type harness struct {
	bridge *Bridge
	harbor *fakeHarbor
	events *events
	cancel context.CancelFunc
	done   chan struct{}
}

func newHarness(t *testing.T, timeouts Timeouts) *harness {
	t.Helper()
	h := &harness{harbor: newFakeHarbor(t), events: &events{}, done: make(chan struct{})}
	target := h.harbor.target()
	h.bridge = NewBridge(Config{UDPAddr: "127.0.0.1:0"}, timeouts,
		func(context.Context) (HarborTarget, error) { return target, nil },
		h.events, slog.New(slog.DiscardHandler), time.Now)
	ctx, cancel := context.WithCancel(t.Context())
	h.cancel = cancel
	go func() {
		defer close(h.done)
		_ = h.bridge.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-h.done
	})
	require.Eventually(t, h.bridge.Available, 2*time.Second, 10*time.Millisecond)
	return h
}

// browser is the panel's side of a session: a pion peer that sends Opus.
type browser struct {
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
	stop  chan struct{}
	wg    sync.WaitGroup
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	m := &webrtc.MediaEngine{}
	require.NoError(t, m.RegisterDefaultCodecs())
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "mic")
	require.NoError(t, err)
	_, err = pc.AddTrack(track)
	require.NoError(t, err)
	b := &browser{pc: pc, track: track, stop: make(chan struct{})}
	t.Cleanup(func() {
		b.mute()
		_ = pc.Close()
	})
	return b
}

// offer returns the complete offer, candidates included.
func (b *browser) offer(t *testing.T) string {
	t.Helper()
	o, err := b.pc.CreateOffer(nil)
	require.NoError(t, err)
	gathered := webrtc.GatheringCompletePromise(b.pc)
	require.NoError(t, b.pc.SetLocalDescription(o))
	<-gathered
	return b.pc.LocalDescription().SDP
}

func (b *browser) answer(t *testing.T, sdp string) {
	t.Helper()
	require.NoError(t, b.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}))
}

// speak sends a packet every 20 ms until mute.
func (b *browser) speak() {
	b.wg.Go(func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-tick.C:
				_ = b.track.WriteSample(media.Sample{Data: opusSilence, Duration: 20 * time.Millisecond})
			}
		}
	})
}

func (b *browser) mute() {
	select {
	case <-b.stop:
	default:
		close(b.stop)
	}
	b.wg.Wait()
}

// connect starts a session for a new browser.
func connect(t *testing.T, h *harness, takeover bool) (*browser, Answer) {
	t.Helper()
	br := newBrowser(t)
	a, err := h.bridge.Start(t.Context(), br.offer(t), takeover)
	require.NoError(t, err)
	br.answer(t, a.SDP)
	return br, a
}

const wait = 5 * time.Second

func TestSessionGoesOnAirAndStops(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testTimeouts)
	br, a := connect(t, h, false)
	require.NotZero(t, a.SessionID)
	require.Equal(t, StateConnecting, h.bridge.Status().State)

	br.speak()
	require.Eventually(t, func() bool { return h.bridge.Status().State == StateOnAir }, wait, 10*time.Millisecond)
	require.Eventually(t, func() bool { return h.harbor.pages() > 10 }, wait, 10*time.Millisecond)

	st := h.bridge.Status()
	require.True(t, st.OnAir())
	require.True(t, st.HarborConnected)
	require.NotNil(t, st.OnAirSince)
	require.Equal(t, a.SessionID, *st.SessionID)

	h.harbor.mu.Lock()
	require.Equal(t, "SOURCE /live HTTP/1.0", h.harbor.requests[0])
	require.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("source:secret")), h.harbor.auth[0])
	require.True(t, bytes.Contains(h.harbor.streams[0], []byte("OpusHead")), "the stream starts with the Opus headers")
	h.harbor.mu.Unlock()

	h.bridge.Stop()
	require.Equal(t, StateOff, h.bridge.Status().State)
	require.Eventually(t, func() bool { return h.harbor.openConns() == 0 }, wait, 10*time.Millisecond)
	h.events.wait(t, []string{"connecting", "on_air", "off:stopped"})

	// Stop without a session is a no-op.
	h.bridge.Stop()
	require.Len(t, h.events.states(), 3)
}

func TestTakeover(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testTimeouts)
	first, a1 := connect(t, h, false)
	first.speak()
	require.Eventually(t, func() bool { return h.bridge.Status().State == StateOnAir }, wait, 10*time.Millisecond)

	second := newBrowser(t)
	offer := second.offer(t)
	_, err := h.bridge.Start(t.Context(), offer, false)
	require.ErrorIs(t, err, ErrSessionActive)

	a2, err := h.bridge.Start(t.Context(), offer, true)
	require.NoError(t, err)
	require.Greater(t, a2.SessionID, a1.SessionID)
	second.answer(t, a2.SDP)
	second.speak()
	require.Eventually(t, func() bool {
		st := h.bridge.Status()
		return st.State == StateOnAir && *st.SessionID == a2.SessionID
	}, wait, 10*time.Millisecond)
	// The first session's harbor connection is closed; only one source.
	require.Eventually(t, func() bool { return h.harbor.openConns() == 1 }, wait, 10*time.Millisecond)
	h.events.wait(t, []string{"connecting", "on_air", "connecting", "on_air"})
}

func TestNoAudioEndsSession(t *testing.T) {
	t.Parallel()
	timeouts := testTimeouts
	timeouts.NoAudio = 300 * time.Millisecond
	h := newHarness(t, timeouts)
	connect(t, h, false)
	require.Eventually(t, func() bool { return h.bridge.Status().State == StateOff }, wait, 10*time.Millisecond)
	h.events.wait(t, []string{"connecting", "off:no_audio"})
}

func TestDropoutEndsSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testTimeouts)
	br, _ := connect(t, h, false)
	br.speak()
	require.Eventually(t, func() bool { return h.bridge.Status().State == StateOnAir }, wait, 10*time.Millisecond)
	br.mute()
	require.Eventually(t, func() bool { return h.bridge.Status().State == StateOff }, wait, 10*time.Millisecond)
	h.events.wait(t, []string{"connecting", "on_air", "off:dropout"})
}

func TestHarborDownThenUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testTimeouts)
	h.harbor.refuse.Store(true)
	br, _ := connect(t, h, false)
	br.speak()
	require.Eventually(t, func() bool { return h.bridge.Status().State == StateReconnecting }, wait, 10*time.Millisecond)
	st := h.bridge.Status()
	require.True(t, st.OnAir(), "audio arrives: the station is live")
	require.False(t, st.HarborConnected)
	require.Nil(t, st.OnAirSince)

	h.harbor.refuse.Store(false)
	require.Eventually(t, func() bool { return h.bridge.Status().State == StateOnAir }, wait, 10*time.Millisecond)
	h.events.wait(t, []string{"connecting", "reconnecting", "on_air"})
}

func TestShutdownEndsSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testTimeouts)
	br, _ := connect(t, h, false)
	br.speak()
	require.Eventually(t, func() bool { return h.bridge.Status().State == StateOnAir }, wait, 10*time.Millisecond)
	h.cancel()
	<-h.done
	require.Equal(t, StateOff, h.bridge.Status().State)
	h.events.wait(t, []string{"connecting", "on_air", "off:shutdown"})
}

func TestInvalidOffers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testTimeouts)

	_, err := h.bridge.Start(t.Context(), "not an sdp", false)
	var oe *OfferError
	require.ErrorAs(t, err, &oe)

	// A data channel only: no audio section.
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	_, err = pc.CreateDataChannel("x", nil)
	require.NoError(t, err)
	o, err := pc.CreateOffer(nil)
	require.NoError(t, err)
	_, err = h.bridge.Start(t.Context(), o.SDP, false)
	require.ErrorAs(t, err, &oe)
	require.Equal(t, StateOff, h.bridge.Status().State)
	require.Empty(t, h.events.states())
}

func TestUnavailableWithoutSocket(t *testing.T) {
	t.Parallel()
	b := NewBridge(Config{UDPAddr: "127.0.0.1:" + strconv.Itoa(-1)}, testTimeouts,
		func(context.Context) (HarborTarget, error) { return HarborTarget{}, nil },
		&events{}, slog.New(slog.DiscardHandler), time.Now)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	br := newBrowser(t)
	_, err := b.Start(t.Context(), br.offer(t), false)
	require.ErrorIs(t, err, ErrUnavailable)
	require.False(t, b.Available())
}

func TestPacketChannels(t *testing.T) {
	t.Parallel()
	require.Equal(t, 1, packetChannels(opusSilence))
	require.Equal(t, 2, packetChannels([]byte{0xfc, 0xff, 0xfe}))
	require.Equal(t, 1, packetChannels(nil))
}

// The Ogg header carries the channel count of the packets: a mono packet
// in a stereo stream is refused by Liquidsoap.
func TestStreamFollowsPacketChannels(t *testing.T) {
	t.Parallel()
	h := newHarness(t, testTimeouts)
	br, _ := connect(t, h, false)
	br.speak()
	require.Eventually(t, func() bool { return h.harbor.pages() > 5 }, wait, 10*time.Millisecond)
	h.harbor.mu.Lock()
	head := h.harbor.streams[0]
	h.harbor.mu.Unlock()
	i := bytes.Index(head, []byte("OpusHead"))
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, byte(1), head[i+9], "channel count of the OpusHead")
}

func TestHarborHandshake(t *testing.T) {
	t.Parallel()
	h := newFakeHarbor(t)
	c, err := dialHarbor(t.Context(), h.target(), 1)
	require.NoError(t, err)
	require.NoError(t, c.Close())

	h.refuse.Store(true)
	_, err = dialHarbor(t.Context(), h.target(), 1)
	require.ErrorIs(t, err, ErrHarborAuth)

	_, err = dialHarbor(t.Context(), HarborTarget{Host: "127.0.0.1", Port: 1}, 1)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrHarborAuth))
}
