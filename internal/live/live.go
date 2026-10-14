// Package live relays the manager's microphone to air (§8.4): the browser
// sends Opus over WebRTC, and the bridge writes the packets unchanged as an
// Ogg/Opus stream into Liquidsoap's harbor input, which mixes the voice
// over the music. Decisions: docs/phase-12-plan.md §2.
package live

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// State of the live input.
type State string

// States (decision 6).
const (
	// StateOff: no session.
	StateOff State = "off"
	// StateConnecting: the answer is sent, no audio has arrived yet.
	StateConnecting State = "connecting"
	// StateOnAir: audio flows to the harbor.
	StateOnAir State = "on_air"
	// StateReconnecting: audio arrives, but the harbor is not reachable;
	// packets are dropped until it is.
	StateReconnecting State = "reconnecting"
)

// Reason a session ended.
type Reason string

// Reasons (decision 6).
const (
	ReasonStopped  Reason = "stopped"
	ReasonReplaced Reason = "replaced"
	ReasonNoAudio  Reason = "no_audio"
	ReasonDropout  Reason = "dropout"
	ReasonShutdown Reason = "shutdown"
)

// Errors of Start.
var (
	// ErrSessionActive: a session is running and takeover was not asked.
	ErrSessionActive = errors.New("a live session is active")
	// ErrUnavailable: the UDP port of the bridge is not open.
	ErrUnavailable = errors.New("live voice is unavailable")
)

// OfferError is an offer the bridge cannot answer.
type OfferError struct{ Msg string }

func (e *OfferError) Error() string { return "invalid offer: " + e.Msg }

// Answer is the result of Start.
type Answer struct {
	SessionID uint64
	SDP       string
}

// Status is the state of the live input.
type Status struct {
	State     State
	SessionID *uint64
	// OnAirSince is when audio first reached the harbor in this session.
	OnAirSince *time.Time
	// HarborConnected: the source connection to the harbor is open.
	HarborConnected bool
	Packets         uint64
	LastPacketAt    *time.Time
}

// OnAir reports whether the station is live: audio arrives, whether or not
// the harbor takes it at this moment.
func (s Status) OnAir() bool {
	return s.State == StateOnAir || s.State == StateReconnecting
}

// Publisher sends an event to the panels without blocking.
type Publisher interface {
	Publish(event any)
}

// Config is the network setup of the bridge (LIVE_UDP_ADDR,
// LIVE_PUBLIC_IPS; decision 4).
type Config struct {
	// UDPAddr is the address of the one UDP socket of all sessions.
	UDPAddr string
	// PublicIPs are the addresses the browser reaches UDPAddr on; empty:
	// the addresses of the host's interfaces.
	PublicIPs []string
}

// Timeouts of a session (decision 6). Tests shorten them.
type Timeouts struct {
	// NoAudio ends a session that gets no packet this long after the
	// answer.
	NoAudio time.Duration
	// Dropout ends a session that gets no packet this long.
	Dropout time.Duration
	// HarborRetry is the pause between harbor connects.
	HarborRetry time.Duration
	// Gather bounds the candidate gathering of the answer.
	Gather time.Duration
}

// DefaultTimeouts are the timeouts of decision 6.
var DefaultTimeouts = Timeouts{
	NoAudio:     10 * time.Second,
	Dropout:     5 * time.Second,
	HarborRetry: time.Second,
	Gather:      2 * time.Second,
}

// ICE timeouts: a peer that stops answering is closed within a few seconds,
// close to the dropout timeout.
const (
	iceDisconnected = 3 * time.Second
	iceFailed       = 5 * time.Second
	iceKeepAlive    = time.Second
)

// startRequest is a Start call handed to the Run goroutine.
type startRequest struct {
	offer    string
	takeover bool
	reply    chan startResult
}

type startResult struct {
	answer Answer
	err    error
}

// Bridge is the live voice bridge. The Run goroutine owns the UDP socket
// and creates and closes the peer connections; mu guards the session
// state, which the relay goroutine, pion's callbacks, and requests share.
type Bridge struct {
	cfg      Config
	timeouts Timeouts
	harbor   HarborFunc
	pub      Publisher
	log      *slog.Logger // component webrtc
	now      func() time.Time

	starts chan startRequest
	// closing receives ended sessions whose peer connection Run closes.
	closing chan *session

	mu     sync.Mutex
	api    *webrtc.API // nil until the socket is open
	cur    *session
	lastID uint64
}

// session is one live session. Its counters and state are guarded by
// Bridge.mu; pc is closed by the Run goroutine.
type session struct {
	id     uint64
	pc     *webrtc.PeerConnection
	ctx    context.Context
	cancel context.CancelFunc
	// noAudio fires when no packet arrived in time.
	noAudio *time.Timer

	state      State
	ended      bool
	relaying   bool
	onAirSince *time.Time
	harborUp   bool
	packets    uint64
	lastPacket *time.Time
}

// closingBuffer holds ended sessions until Run closes them. At most two
// sessions exist at a time (one during a takeover), so it never fills.
const closingBuffer = 8

// NewBridge returns the bridge. Run opens its socket and serves Start.
func NewBridge(cfg Config, timeouts Timeouts, harbor HarborFunc, pub Publisher, log *slog.Logger, now func() time.Time) *Bridge {
	return &Bridge{
		cfg: cfg, timeouts: timeouts, harbor: harbor, pub: pub,
		log: logging.For(log, logging.ComponentWebRTC), now: now,
		starts:  make(chan startRequest),
		closing: make(chan *session, closingBuffer),
	}
}

// Run opens the UDP socket and serves Start until ctx ends; then it ends
// the session and closes the socket. A socket that cannot be opened is
// logged, and Start answers ErrUnavailable; the rest of the backend runs
// on.
func (b *Bridge) Run(ctx context.Context) error {
	mux, err := b.listen(ctx)
	if err != nil {
		b.log.Error("live voice unavailable: open the UDP port failed", "addr", b.cfg.UDPAddr, logging.KeyError, err)
	} else {
		b.log.Info("live voice bridge listening", "addr", b.cfg.UDPAddr, "public_ips", b.cfg.PublicIPs)
		defer func() { _ = mux.Close() }()
	}
	for {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			cur := b.cur
			b.mu.Unlock()
			if cur != nil {
				b.end(cur, ReasonShutdown)
			}
			b.drainClosing()
			return nil
		case r := <-b.starts:
			a, err := b.start(ctx, r.offer, r.takeover)
			r.reply <- startResult{answer: a, err: err}
		case s := <-b.closing:
			closePeer(s)
		}
	}
}

// drainClosing closes the peer connections of the ended sessions.
func (b *Bridge) drainClosing() {
	for {
		select {
		case s := <-b.closing:
			closePeer(s)
		default:
			return
		}
	}
}

func closePeer(s *session) {
	// The relay goroutine ends when its track read fails after the close.
	_ = s.pc.Close()
}

// listen opens the UDP socket and builds the WebRTC API on it.
func (b *Bridge) listen(ctx context.Context) (*ice.UDPMuxDefault, error) {
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(ctx, "udp", b.cfg.UDPAddr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %s: %w", b.cfg.UDPAddr, err)
	}
	logs := newPionLogs(b.log)
	mux := ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: pc, Logger: logs.NewLogger("udpmux")})

	api, err := newAPI(mux, b.cfg.PublicIPs, logs)
	if err != nil {
		_ = mux.Close()
		return nil, err
	}
	b.mu.Lock()
	b.api = api
	b.mu.Unlock()
	return mux, nil
}

// newAPI builds the WebRTC API: Opus only, receive only, every session on
// the one socket, host candidates only (decisions 3 and 4).
func newAPI(mux ice.UDPMux, publicIPs []string, logs *pionLogs) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeOpus, ClockRate: opusSampleRate, Channels: opusChannels,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register opus: %w", err)
	}
	reg := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, reg); err != nil {
		return nil, fmt.Errorf("register interceptors: %w", err)
	}

	var se webrtc.SettingEngine
	se.LoggerFactory = logs
	se.SetICEUDPMux(mux)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	// The browser hides its address behind an mDNS name; it is learned
	// from its connectivity checks instead, so no mDNS is needed.
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetIncludeLoopbackCandidate(true)
	se.SetICETimeouts(iceDisconnected, iceFailed, iceKeepAlive)
	if len(publicIPs) > 0 {
		// The host candidates carry the public addresses instead of the
		// socket's own (decision 4).
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External: publicIPs, AsCandidateType: webrtc.ICECandidateTypeHost, Mode: webrtc.ICEAddressRewriteReplace,
		}); err != nil {
			return nil, fmt.Errorf("set public addresses: %w", err)
		}
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(reg), webrtc.WithSettingEngine(se)), nil
}

// Start answers the browser's offer and starts a session. With takeover,
// a running session ends (replaced); without, it is refused with
// ErrSessionActive. Errors: ErrUnavailable, *OfferError.
func (b *Bridge) Start(ctx context.Context, offer string, takeover bool) (Answer, error) {
	r := startRequest{offer: offer, takeover: takeover, reply: make(chan startResult, 1)}
	select {
	case b.starts <- r:
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	}
	// The reply is buffered: if the client gives up, Run does not block.
	select {
	case res := <-r.reply:
		return res.answer, res.err
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	}
}

// start runs on the Run goroutine. ctx is the service context.
func (b *Bridge) start(ctx context.Context, offer string, takeover bool) (Answer, error) {
	if err := checkOffer(offer); err != nil {
		return Answer{}, err
	}
	b.mu.Lock()
	api, cur := b.api, b.cur
	b.mu.Unlock()
	if api == nil {
		return Answer{}, ErrUnavailable
	}
	if cur != nil {
		if !takeover {
			return Answer{}, ErrSessionActive
		}
		b.end(cur, ReasonReplaced)
	}

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return Answer{}, fmt.Errorf("create peer connection: %w", err)
	}
	answer, err := b.negotiate(pc, offer)
	if err != nil {
		_ = pc.Close()
		return Answer{}, err
	}

	sctx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.lastID++
	s := &session{id: b.lastID, pc: pc, ctx: sctx, cancel: cancel, state: StateConnecting}
	b.cur = s
	b.mu.Unlock()

	s.noAudio = time.AfterFunc(b.timeouts.NoAudio, func() {
		b.mu.Lock()
		silent := s.packets == 0
		b.mu.Unlock()
		if silent {
			b.end(s, ReasonNoAudio)
		}
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if tr.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		// One audio track goes to air; the harbor takes one source.
		b.mu.Lock()
		first := !s.relaying
		s.relaying = true
		b.mu.Unlock()
		if first {
			b.relay(s, tr)
		}
	})
	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		if st == webrtc.PeerConnectionStateFailed || st == webrtc.PeerConnectionStateClosed {
			b.end(s, ReasonDropout)
		}
	})

	b.log.Info("live session started", "session_id", s.id, "takeover", cur != nil)
	b.publish(Status{State: StateConnecting, SessionID: &s.id}, nil)
	return Answer{SessionID: s.id, SDP: answer}, nil
}

// negotiate sets the offer and returns the complete answer.
func (b *Bridge) negotiate(pc *webrtc.PeerConnection, offer string) (string, error) {
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return "", &OfferError{Msg: err.Error()}
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", &OfferError{Msg: err.Error()}
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("set local description: %w", err)
	}
	select {
	case <-gathered:
	case <-time.After(b.timeouts.Gather):
		return "", errors.New("gather candidates: timed out")
	}
	return pc.LocalDescription().SDP, nil
}

// checkOffer refuses an offer without an Opus audio section before any
// peer connection is made.
func checkOffer(offer string) error {
	var d sdp.SessionDescription
	if err := d.UnmarshalString(offer); err != nil {
		return &OfferError{Msg: "not an SDP: " + err.Error()}
	}
	for _, m := range d.MediaDescriptions {
		if m.MediaName.Media != "audio" {
			continue
		}
		for _, a := range m.Attributes {
			if a.Key == "rtpmap" && strings.Contains(strings.ToLower(a.Value), " opus/48000") {
				return nil
			}
		}
	}
	return &OfferError{Msg: "no audio section with Opus"}
}

// Stop ends the session, if any (stopped).
func (b *Bridge) Stop() {
	b.mu.Lock()
	cur := b.cur
	b.mu.Unlock()
	if cur != nil {
		b.end(cur, ReasonStopped)
	}
}

// Status returns the state of the live input.
func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == nil {
		return Status{State: StateOff}
	}
	return b.cur.statusLocked()
}

// Available reports whether the UDP socket is open.
func (b *Bridge) Available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.api != nil
}

func (s *session) statusLocked() Status {
	return Status{
		State: s.state, SessionID: new(s.id), OnAirSince: s.onAirSince, HarborConnected: s.harborUp,
		Packets: s.packets, LastPacketAt: s.lastPacket,
	}
}

// end ends a session once: the session is detached, its context cancelled,
// and its peer connection handed to Run for closing.
func (b *Bridge) end(s *session, reason Reason) {
	b.mu.Lock()
	if s.ended {
		b.mu.Unlock()
		return
	}
	s.ended = true
	current := b.cur == s
	if current {
		b.cur = nil
	}
	onAir, packets := s.onAirSince, s.packets
	b.mu.Unlock()

	s.cancel()
	if s.noAudio != nil {
		s.noAudio.Stop()
	}
	b.closing <- s

	attrs := []any{"session_id", s.id, "reason", reason, "packets", packets}
	if onAir != nil {
		attrs = append(attrs, "on_air_seconds", int(b.now().Sub(*onAir).Seconds()))
	}
	if reason == ReasonNoAudio || reason == ReasonDropout {
		b.log.Warn("live session ended", attrs...)
	} else {
		b.log.Info("live session ended", attrs...)
	}
	// After a takeover the new session publishes its own state.
	if current && reason != ReasonReplaced {
		b.publish(Status{State: StateOff}, &reason)
	}
}

// relay is the session's only reader: it reads the audio track and writes
// it to the harbor until the session ends or the audio stops. It runs on
// the goroutine pion starts for the track and owns the harbor connection.
func (b *Bridge) relay(s *session, tr *webrtc.TrackRemote) {
	var h *harborConn
	var lastDial time.Time
	defer func() {
		if h != nil {
			_ = h.Close()
		}
	}()
	for {
		if err := tr.SetReadDeadline(time.Now().Add(b.timeouts.Dropout)); err != nil {
			b.end(s, ReasonDropout)
			return
		}
		p, _, err := tr.ReadRTP()
		if err != nil {
			// A closed session reads an error too; end is a no-op then.
			b.end(s, ReasonDropout)
			return
		}
		if s.ctx.Err() != nil {
			return
		}
		b.packet(s)

		channels := packetChannels(p.Payload)
		if h != nil && h.channels != channels {
			// The browser switched between mono and stereo: a new stream
			// with a matching header.
			_ = h.Close()
			h, lastDial = nil, time.Time{}
		}
		if h == nil && b.now().Sub(lastDial) >= b.timeouts.HarborRetry {
			lastDial = b.now()
			h, err = b.dial(s, channels)
			if err != nil {
				b.harborDown(s, err)
			}
		}
		if h == nil {
			continue
		}
		if err := h.WriteRTP(p); err != nil {
			_ = h.Close()
			h = nil
			lastDial = b.now()
			b.harborDown(s, err)
			continue
		}
		b.harborUp(s)
	}
}

func (b *Bridge) dial(s *session, channels int) (*harborConn, error) {
	t, err := b.harbor(s.ctx)
	if err != nil {
		return nil, fmt.Errorf("read harbor target: %w", err)
	}
	return dialHarbor(s.ctx, t, channels)
}

// packet counts a received packet.
func (b *Bridge) packet(s *session) {
	now := b.now().UTC()
	b.mu.Lock()
	s.packets++
	s.lastPacket = &now
	b.mu.Unlock()
}

// harborUp marks audio as reaching the harbor.
func (b *Bridge) harborUp(s *session) {
	b.mu.Lock()
	if s.ended || s.state == StateOnAir {
		b.mu.Unlock()
		return
	}
	if s.onAirSince == nil {
		s.onAirSince = new(b.now().UTC())
	}
	s.state, s.harborUp = StateOnAir, true
	st := s.statusLocked()
	b.mu.Unlock()
	b.log.Info("live voice on air", "session_id", s.id)
	b.publish(st, nil)
}

// harborDown marks audio as not reaching the harbor (decision 6).
func (b *Bridge) harborDown(s *session, err error) {
	b.mu.Lock()
	if s.ended {
		b.mu.Unlock()
		return
	}
	changed := s.state != StateReconnecting
	s.state, s.harborUp = StateReconnecting, false
	st := s.statusLocked()
	b.mu.Unlock()
	if changed {
		b.log.Warn("live voice cannot reach the harbor, retrying", "session_id", s.id, logging.KeyError, err)
		b.publish(st, nil)
	}
}

func (b *Bridge) publish(st Status, reason *Reason) {
	data := ws.LiveStateData{State: string(st.State), SessionID: st.SessionID, OnAirSince: st.OnAirSince}
	if reason != nil {
		data.Reason = new(string(*reason))
	}
	b.pub.Publish(ws.LiveStateEvent{Type: ws.EventLiveState, Time: b.now().UTC(), Data: data})
}
