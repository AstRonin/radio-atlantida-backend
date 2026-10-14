package live

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// HarborMount is the mount of Liquidsoap's live input. It is fixed in the
// dev and production scripts, so it is not a setting.
const HarborMount = "live"

const (
	// harborDialTimeout bounds the connect and the source handshake.
	harborDialTimeout = 2 * time.Second
	// harborWriteTimeout bounds one page write: a stalled Liquidsoap must
	// never block the session (decision 7).
	harborWriteTimeout = time.Second
	// opusSampleRate is the rate of WebRTC Opus, always 48 kHz.
	opusSampleRate = 48000
	// opusChannels is the channel count WebRTC declares for Opus
	// (opus/48000/2), whatever the browser actually sends.
	opusChannels = 2
)

// ErrHarborAuth is returned when the harbor refuses the credentials.
var ErrHarborAuth = errors.New("harbor refused the source credentials")

// HarborTarget is where the live input of Liquidsoap listens.
type HarborTarget struct {
	Host     string
	Port     int
	User     string
	Password string
}

// HarborFunc reads the harbor target on every connect, so a settings
// change applies to the next connection.
type HarborFunc func(ctx context.Context) (HarborTarget, error)

// harborConn is one source connection to the harbor carrying one Ogg/Opus
// stream. It belongs to the relay goroutine of a session.
type harborConn struct {
	conn net.Conn
	ogg  *oggwriter.OggWriter
	// channels is the channel count in the stream header.
	channels int
}

// packetChannels returns the channel count of an Opus packet: the stereo
// flag of its TOC byte (RFC 6716 §3.1). Browsers send mono microphones as
// mono packets; Liquidsoap refuses packets whose count differs from the
// stream header, so the header follows the packets.
func packetChannels(payload []byte) int {
	if len(payload) > 0 && payload[0]&0x04 != 0 {
		return 2
	}
	return 1
}

// dialHarbor connects to the harbor as an Icecast source (the SOURCE
// method, which input.harbor accepts) and writes the Ogg/Opus headers for
// a stream of the given channel count.
func dialHarbor(ctx context.Context, t HarborTarget, channels int) (*harborConn, error) {
	ctx, cancel := context.WithTimeout(ctx, harborDialTimeout)
	defer cancel()
	addr := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to harbor %s: %w", addr, err)
	}
	if herr := handshake(ctx, conn, t); herr != nil {
		_ = conn.Close()
		return nil, herr
	}
	ogg, err := oggwriter.NewWith(deadlineWriter{conn}, opusSampleRate, uint16(channels)) //nolint:gosec // G115: 1 or 2
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("start ogg stream: %w", err)
	}
	return &harborConn{conn: conn, ogg: ogg, channels: channels}, nil
}

// handshake sends the source request and reads the answer. The harbor
// answers with a status line and headers, then only reads.
func handshake(ctx context.Context, conn net.Conn, t HarborTarget) error {
	if dl, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(dl); err != nil {
			return fmt.Errorf("set handshake deadline: %w", err)
		}
	}
	auth := base64.StdEncoding.EncodeToString([]byte(t.User + ":" + t.Password))
	req := "SOURCE /" + HarborMount + " HTTP/1.0\r\n" +
		"Authorization: Basic " + auth + "\r\n" +
		"Content-Type: audio/ogg\r\n" +
		"User-Agent: radio-atlantida-backend\r\n" +
		"Ice-Name: live\r\n" +
		"\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return fmt.Errorf("send source request: %w", err)
	}
	r := textproto.NewReader(bufio.NewReader(conn))
	status, err := r.ReadLine()
	if err != nil {
		return fmt.Errorf("read harbor answer: %w", err)
	}
	// The harbor sends nothing after its headers, so the buffered reader
	// holds no stream data when it is dropped.
	if _, err := r.ReadMIMEHeader(); err != nil {
		return fmt.Errorf("read harbor answer headers: %w", err)
	}
	_, code, _ := strings.Cut(status, " ")
	code, _, _ = strings.Cut(code, " ")
	switch code {
	case "200":
	case "401", "403":
		return ErrHarborAuth
	default:
		return fmt.Errorf("harbor refused the source: %q", status)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear handshake deadline: %w", err)
	}
	return nil
}

// WriteRTP writes one Opus packet as an Ogg page.
func (h *harborConn) WriteRTP(p *rtp.Packet) error {
	if err := h.ogg.WriteRTP(p); err != nil {
		return fmt.Errorf("write to harbor: %w", err)
	}
	return nil
}

// Close drops the connection. No end-of-stream page is sent: the harbor
// ends the input when the connection closes.
func (h *harborConn) Close() error {
	return h.conn.Close()
}

// deadlineWriter gives every write its own deadline.
type deadlineWriter struct{ conn net.Conn }

func (w deadlineWriter) Write(p []byte) (int, error) {
	if err := w.conn.SetWriteDeadline(time.Now().Add(harborWriteTimeout)); err != nil {
		return 0, err
	}
	return w.conn.Write(p)
}
