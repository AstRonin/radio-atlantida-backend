// Package liquidsoap is the control client of Liquidsoap (§8.1): it pushes
// requests into the hour queues, reads them, flushes them, and sets
// interactive variables over the telnet control interface.
//
// The interface is a line protocol: the client sends one command per line,
// and Liquidsoap answers with zero or more lines followed by END. It has no
// authentication; the port stays on the private network (§8.9).
package liquidsoap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrUnavailable is returned when Liquidsoap cannot be reached or the
// connection broke.
var ErrUnavailable = errors.New("liquidsoap control link is unavailable")

const (
	// commandTimeout bounds a command whose context has no earlier deadline.
	// A local Liquidsoap answers in milliseconds.
	commandTimeout = 3 * time.Second
	// dialTimeout bounds opening the connection.
	dialTimeout = 2 * time.Second
	// maxReplyLines bounds a reply, so a broken peer cannot grow memory.
	maxReplyLines = 1000
	// endMarker ends every reply.
	endMarker = "END"
)

// AddrFunc returns the control address, host:port, read from the settings
// on every connect.
type AddrFunc func(ctx context.Context) (string, error)

// State is the state of the control link, for the panel and the outage
// detector (§7.1).
type State struct {
	Connected bool
	// Since is when the link last changed state; zero before the first
	// attempt.
	Since time.Time
	// LastOK is the time of the last successful command.
	LastOK time.Time
}

// Client is the control client. It holds one connection, opened on first
// use and after a failure; commands are serialized by mu, which also guards
// the connection and the state. It starts no goroutines.
type Client struct {
	addr     AddrFunc
	now      func() time.Time
	onChange func(State)

	mu    sync.Mutex
	conn  net.Conn
	r     *bufio.Reader
	gen   uint64
	state State
}

// New returns a client. onChange, if set, is called (with mu held, so it
// must not block or call the client) whenever the link changes state.
func New(addr AddrFunc, now func() time.Time, onChange func(State)) *Client {
	return &Client{addr: addr, now: now, onChange: onChange}
}

// State returns the link state.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Generation counts the connections opened so far. A caller that keeps
// state in Liquidsoap (variables, queues) compares it with the value it last
// saw to tell that the connection, and maybe Liquidsoap itself, is new.
func (c *Client) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// Reset closes the connection, for example after the control address
// changed; the next command connects again.
func (c *Client) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

// Command sends one command and returns the lines of the reply, without
// END. A failure closes the connection and returns an error wrapping
// ErrUnavailable.
func (c *Client) Command(ctx context.Context, cmd string) ([]string, error) {
	if strings.ContainsAny(cmd, "\r\n") {
		return nil, fmt.Errorf("liquidsoap command %q: contains a line break", cmd)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(ctx); err != nil {
		return nil, err
	}
	lines, err := c.roundTrip(ctx, cmd)
	if err != nil {
		c.closeLocked()
		return nil, fmt.Errorf("liquidsoap command %q: %w: %w", cmd, ErrUnavailable, err)
	}
	c.state.LastOK = c.now().UTC()
	return lines, nil
}

func (c *Client) connectLocked(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	addr, err := c.addr(ctx)
	if err != nil {
		return fmt.Errorf("liquidsoap control address: %w", err)
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		c.setConnectedLocked(false)
		return fmt.Errorf("connect liquidsoap at %s: %w: %w", addr, ErrUnavailable, err)
	}
	c.conn, c.r = conn, bufio.NewReader(conn)
	c.gen++
	c.setConnectedLocked(true)
	return nil
}

func (c *Client) closeLocked() {
	if c.conn == nil {
		return
	}
	_ = c.conn.Close() // the connection is dropped either way
	c.conn, c.r = nil, nil
	c.setConnectedLocked(false)
}

func (c *Client) setConnectedLocked(connected bool) {
	if c.state.Connected == connected && !c.state.Since.IsZero() {
		return
	}
	c.state.Connected = connected
	c.state.Since = c.now().UTC()
	if c.onChange != nil {
		c.onChange(c.state)
	}
}

// roundTrip writes cmd and reads the reply up to END, within the context
// deadline or commandTimeout, whichever is earlier.
func (c *Client) roundTrip(ctx context.Context, cmd string) ([]string, error) {
	deadline := time.Now().Add(commandTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn := c.conn
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	// A cancelled context interrupts a blocked read or write. The callback
	// uses its own copy of the connection: c.conn is cleared under mu.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if _, err := conn.Write([]byte(cmd + "\n")); err != nil {
		return nil, err
	}
	var lines []string
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == endMarker {
			return lines, nil
		}
		if len(lines) == maxReplyLines {
			return nil, fmt.Errorf("reply longer than %d lines", maxReplyLines)
		}
		lines = append(lines, line)
	}
}

// Push adds a request to the end of a queue and returns its request ID.
func (c *Client) Push(ctx context.Context, queue, uri string) (int, error) {
	lines, err := c.Command(ctx, queue+".push "+uri)
	if err != nil {
		return 0, err
	}
	if len(lines) != 1 {
		return 0, fmt.Errorf("push to %s: unexpected reply %q", queue, lines)
	}
	rid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0, fmt.Errorf("push to %s: %w: %s", queue, errPushRefused, lines[0])
	}
	return rid, nil
}

// errPushRefused: Liquidsoap answered a push with an error text.
var errPushRefused = errors.New("push refused")

// Queue returns the request IDs waiting in a queue, the playing one
// excluded.
func (c *Client) Queue(ctx context.Context, queue string) ([]int, error) {
	lines, err := c.Command(ctx, queue+".queue")
	if err != nil {
		return nil, err
	}
	var rids []int
	for _, l := range lines {
		for f := range strings.FieldsSeq(l) {
			rid, err := strconv.Atoi(f)
			if err != nil {
				return nil, fmt.Errorf("queue of %s: unexpected reply %q", queue, lines)
			}
			rids = append(rids, rid)
		}
	}
	return rids, nil
}

// SetVar sets an interactive variable (var.set name = value).
func (c *Client) SetVar(ctx context.Context, name, value string) error {
	lines, err := c.Command(ctx, "var.set "+name+" = "+value)
	if err != nil {
		return err
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "Variable "+name+" set") {
		return fmt.Errorf("set %s: unexpected reply %q", name, lines)
	}
	return nil
}

// Ping checks the link (uptime).
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Command(ctx, "uptime")
	return err
}

// FormatFloat formats a number for var.set and request annotations:
// Liquidsoap float literals need a decimal point.
func FormatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += "."
	}
	return s
}
