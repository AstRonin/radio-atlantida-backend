// Command liqsmoke runs the playout script (dev/liquidsoap/radio.liq) in the
// pinned Liquidsoap image against a test Icecast and checks the runtime
// stream output (docs/deploy-prereqs-plan.md A9): `liquidsoap --check`
// accepts scripts that fail only when an encoder starts, so the script is
// run, not only checked. Used by `make liquidsoap-smoke` and by CI.
//
// It serves a stub backend (callbacks, emergency, the output endpoint),
// starts the stack of dev/liqsmoke/compose.yml, and checks in order:
//
//   - the startup output comes from GET /api/playout/output;
//   - output.set with the running output answers "OK unchanged";
//   - invalid outputs are refused and the running output stays;
//   - every codec × channel count × sample rate at 64 and 320 kbps, and
//     on both sides of each encoder limit, reaches Icecast with that
//     format, or is refused by the script where the encoder cannot make
//     it (Liquidsoap would exit);
//   - a switch to TLS through a TLS proxy in front of Icecast;
//   - a Liquidsoap restart with the backend down starts from the state
//     file, over TLS again.
//
// Exit code non-zero on any miss. A Liquidsoap container that stops while
// an output is awaited fails the case at once (an encoder that raises makes
// Liquidsoap exit with code 2), and every failure prints the container
// state and the last lines of the Liquidsoap log, which hold the encoder
// error. The stack is left running for inspection; `make
// liquidsoap-smoke` removes it.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/streamformat"
)

// output is the JSON object of output.set and GET /api/playout/output
// (internal/playout.StreamOutput).
type output struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Mount        string `json:"mount"`
	TLS          bool   `json:"tls"`
	Codec        string `json:"codec"`
	BitrateKbps  int    `json:"bitrate_kbps"`
	SampleRateHz int    `json:"sample_rate_hz"`
	Channels     int    `json:"channels"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Genre        string `json:"genre"`
}

type config struct {
	compose     []string // docker compose command with -f and -p
	telnet      string
	status      string
	listen      string
	icecastHost string
	tlsHost     string
	tlsPort     int
	timeout     time.Duration
	poll        time.Duration
}

func main() {
	var c config
	composeFile := flag.String("compose-file", "dev/liqsmoke/compose.yml", "compose file of the test stack")
	project := flag.String("project", "radio-liqsmoke", "compose project name")
	flag.StringVar(&c.telnet, "telnet", "127.0.0.1:11234", "Liquidsoap telnet address (published)")
	flag.StringVar(&c.status, "icecast", "http://127.0.0.1:18000", "Icecast base URL (published)")
	flag.StringVar(&c.listen, "listen", ":18090", "stub backend address; the script reaches it as host.docker.internal")
	flag.StringVar(&c.icecastHost, "icecast-host", "icecast", "Icecast host as Liquidsoap sees it")
	flag.StringVar(&c.tlsHost, "tls-host", "tlsproxy", "TLS proxy host as Liquidsoap sees it")
	flag.IntVar(&c.tlsPort, "tls-port", 8443, "TLS proxy port")
	flag.DurationVar(&c.timeout, "timeout", 15*time.Second, "how long to wait for each output on Icecast")
	flag.DurationVar(&c.poll, "poll", 200*time.Millisecond, "how often to check Icecast and the Liquidsoap container while waiting")
	certDir := flag.String("cert", "", "only write a throwaway CA and the proxy certificate into this directory, then exit")
	flag.Parse()
	c.compose = []string{"compose", "-f", *composeFile, "-p", *project}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	if *certDir != "" {
		err = writeCerts(*certDir, c.tlsHost)
	} else {
		err = run(ctx, c)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "liqsmoke: FAIL:", err)
		if *certDir == "" && ctx.Err() == nil {
			// The encoder error, if any, is in the Liquidsoap log.
			if state, code, serr := liquidsoapState(context.Background(), c); serr == nil {
				fmt.Fprintf(os.Stderr, "liqsmoke: liquidsoap container: %s (exit code %d)\n", state, code)
			}
			fmt.Fprintf(os.Stderr, "liqsmoke: last %d lines of the liquidsoap log:\n%s\n", logTail, liquidsoapLog(context.Background(), c))
		}
		os.Exit(1)
	}
}

// stub is the backend as the script sees it.
type stub struct {
	mu        sync.Mutex
	output    *output // nil: the output endpoint answers 503
	connected atomic.Int64
}

func (s *stub) setOutput(o *output) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.output = o
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/playout/callback":
		var e struct {
			Event     string `json:"event"`
			Connected bool   `json:"connected"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&e) == nil && e.Event == "output" && e.Connected {
			s.connected.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	case "/api/playout/emergency/next":
		w.WriteHeader(http.StatusNoContent)
	case "/api/playout/output":
		s.mu.Lock()
		o := s.output
		s.mu.Unlock()
		if o == nil {
			http.Error(w, "backend down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(o)
	default:
		http.NotFound(w, r)
	}
}

func run(ctx context.Context, c config) error {
	st := &stub{}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", c.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: st, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	plain := func(mount, codec string, br, sr, ch int) output {
		return output{
			Host: c.icecastHost, Port: 8000, Mount: mount, Codec: codec, BitrateKbps: br, SampleRateHz: sr, Channels: ch,
			Name: "Radio Atlantida (smoke)", Description: "liqsmoke", Genre: "Test",
		}
	}

	// 1. Startup from the backend.
	start := plain("/smoke-start", "mp3", 192, 48000, 2)
	st.setOutput(&start)
	if err = docker(ctx, c, "up", "-d", "--wait", "icecast", "tlsproxy"); err != nil {
		return err
	}
	if err = docker(ctx, c, "up", "-d", "--force-recreate", "liquidsoap"); err != nil {
		return err
	}
	ctl, err := dialTelnet(ctx, c.telnet, time.Minute)
	if err != nil {
		return err
	}
	defer ctl.close()
	if err = waitOutput(ctx, c, start); err != nil {
		return fmt.Errorf("startup output from the backend: %w", err)
	}
	pass("startup output from GET /api/playout/output")

	// 2. The running output again: unchanged.
	if err = expectReply(ctl, start, "OK unchanged"); err != nil {
		return err
	}
	pass("output.set with the running output: OK unchanged")

	// 3. Refusals keep the running output.
	bad := []struct{ name, arg string }{
		{"not JSON", "{"},
		{"missing field", `{"host":"icecast"}`},
		{"mount", mustJSON(plain("radio", "mp3", 128, 44100, 2))},
		{"codec", mustJSON(plain("/smoke", "flac", 128, 44100, 2))},
		{"bitrate", mustJSON(plain("/smoke", "mp3", 100, 44100, 2))},
		{"sample rate", mustJSON(plain("/smoke", "mp3", 128, 8000, 2))},
		{"channels", mustJSON(plain("/smoke", "mp3", 128, 44100, 3))},
	}
	for _, b := range bad {
		reply, cerr := ctl.command("output.set " + b.arg)
		if cerr != nil {
			return cerr
		}
		if !strings.HasPrefix(reply, "ERROR") {
			return fmt.Errorf("output.set with a bad %s: got %q, want ERROR", b.name, reply)
		}
	}
	if err = waitOutput(ctx, c, start); err != nil {
		return fmt.Errorf("running output after refusals: %w", err)
	}
	pass("invalid outputs refused, the running output stays")

	// 4. Every codec × channel count × sample rate, at the lowest and the
	// highest bitrate and on both sides of an encoder limit, on one mount
	// (the switch case of production).
	last := start
	for _, codec := range []string{"mp3", "aac", "vorbis"} {
		for _, ch := range []int{2, 1} {
			for _, sr := range []int{44100, 48000, 32000, 22050} {
				for _, br := range testBitrates(codec, ch, sr) {
					o := plain("/smoke", codec, br, sr, ch)
					if refused(o) {
						if err = expectReplyPrefix(ctl, o, "ERROR"); err != nil {
							return err
						}
						pass(fmt.Sprintf("%s %d ch %d kbps %d Hz: refused by the script", codec, ch, br, sr))
						continue
					}
					t0 := time.Now()
					if err = expectReply(ctl, o, "OK applied"); err != nil {
						return err
					}
					if err = waitOutput(ctx, c, o); err != nil {
						return fmt.Errorf("%s %d ch %d kbps %d Hz: %w", codec, ch, br, sr, err)
					}
					last = o
					pass(fmt.Sprintf("%s %d ch %d kbps %d Hz on Icecast after %s", codec, ch, br, sr,
						time.Since(t0).Round(100*time.Millisecond)))
				}
			}
		}
	}
	if err = waitOutput(ctx, c, last); err != nil {
		return fmt.Errorf("after the refused combinations: %w", err)
	}

	// 5. TLS through the proxy.
	tlsOut := plain("/smoke-tls", "mp3", 128, 44100, 2)
	tlsOut.Host, tlsOut.Port, tlsOut.TLS = c.tlsHost, c.tlsPort, true
	if err = expectReply(ctl, tlsOut, "OK applied"); err != nil {
		return err
	}
	if err = waitOutput(ctx, c, tlsOut); err != nil {
		return fmt.Errorf("TLS output: %w", err)
	}
	pass("TLS output through the proxy")

	// 6. Restart with the backend down: the state file.
	st.setOutput(nil)
	ctl.close()
	if err = docker(ctx, c, "restart", "liquidsoap"); err != nil {
		return err
	}
	ctl, err = dialTelnet(ctx, c.telnet, time.Minute)
	if err != nil {
		return err
	}
	defer ctl.close()
	if err = waitOutput(ctx, c, tlsOut); err != nil {
		return fmt.Errorf("restart from the state file: %w", err)
	}
	if err = expectReply(ctl, tlsOut, "OK unchanged"); err != nil {
		return err
	}
	pass("restart with the backend down: output from the state file, then OK unchanged")

	if st.connected.Load() == 0 {
		return errors.New("no output connected callback arrived")
	}
	pass(fmt.Sprintf("%d output connected callbacks", st.connected.Load()))
	fmt.Println("liqsmoke: PASS")
	return nil
}

// refused lists the combinations the script refuses because the encoder
// cannot make them (internal/streamformat, ENCODER_LIMITS in
// dev/liquidsoap/radio.liq). Kept in step by this test: a combination
// refused here and accepted there, or the other way round, fails.
func refused(o output) bool {
	return streamformat.Check(o.Codec, o.Channels, o.SampleRateHz, o.BitrateKbps) != nil
}

func pass(msg string) { fmt.Println("liqsmoke: ok:", msg) }

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func expectReply(ctl *telnet, o output, want string) error {
	reply, err := ctl.command("output.set " + mustJSON(o))
	if err != nil {
		return err
	}
	if reply != want {
		return fmt.Errorf("output.set %s: got %q, want %q", o.Mount, reply, want)
	}
	return nil
}

func expectReplyPrefix(ctl *telnet, o output, prefix string) error {
	reply, err := ctl.command("output.set " + mustJSON(o))
	if err != nil {
		return err
	}
	if !strings.HasPrefix(reply, prefix) {
		return fmt.Errorf("output.set %s %s %d ch %d kbps %d Hz: got %q, want %s", o.Mount, o.Codec, o.Channels,
			o.BitrateKbps, o.SampleRateHz, reply, prefix)
	}
	return nil
}

func docker(ctx context.Context, c config, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", append(append([]string{}, c.compose...), args...)...) //nolint:gosec // G204: a dev tool runs docker compose with its own arguments
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// --- Icecast ---------------------------------------------------------------

// serverTypes are the content types Icecast reports per codec.
var serverTypes = map[string]string{"mp3": "audio/mpeg", "aac": "audio/aac", "vorbis": "application/ogg"}

type source struct {
	ListenURL  string `json:"listenurl"`
	ServerType string `json:"server_type"`
	AudioInfo  string `json:"audio_info"`
	ServerName string `json:"server_name"`
	Genre      string `json:"genre"`
}

// waitOutput waits until Icecast lists the mount of o with its codec and
// format, and its stream name and genre. It stops at once when the
// Liquidsoap container is no longer running (an encoder that raises makes
// Liquidsoap exit).
func waitOutput(ctx context.Context, c config, o output) error {
	deadline := time.Now().Add(c.timeout)
	var last string
	for {
		if state, code, err := liquidsoapState(ctx, c); err == nil && state != "running" {
			return fmt.Errorf("%s: liquidsoap %s (exit code %d)", o.Mount, state, code)
		}
		srcs, err := sources(ctx, c.status)
		if err != nil {
			last = err.Error()
		} else {
			last = "mount not listed"
			for _, s := range srcs {
				u, err := url.Parse(s.ListenURL)
				if err != nil || u.Path != o.Mount {
					continue
				}
				if msg := mismatch(s, o); msg != "" {
					last = msg
					continue
				}
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not on Icecast within %s: %s", o.Mount, c.timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.poll):
		}
	}
}

// liquidsoapState returns the state of the Liquidsoap container ("running",
// "exited", ...) and its exit code, from `docker compose ps`.
func liquidsoapState(ctx context.Context, c config) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := append(append([]string{}, c.compose...), "ps", "-a", "--format", "{{.State}} {{.ExitCode}}", "liquidsoap")
	out, err := exec.CommandContext(ctx, "docker", args...).Output() //nolint:gosec // G204: a dev tool runs docker compose with its own arguments
	if err != nil {
		return "", 0, err
	}
	var state string
	var code int
	if _, err = fmt.Sscan(string(out), &state, &code); err != nil {
		return "", 0, fmt.Errorf("docker compose ps: %q: %w", out, err)
	}
	return state, code, nil
}

// logTail is how many Liquidsoap log lines a failure prints.
const logTail = 40

// liquidsoapLog returns the last logTail lines of the Liquidsoap log.
func liquidsoapLog(ctx context.Context, c config) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := append(append([]string{}, c.compose...), "logs", "--no-color", "--no-log-prefix", "--tail", fmt.Sprint(logTail), "liquidsoap")
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput() //nolint:gosec // G204: a dev tool runs docker compose with its own arguments
	if err != nil {
		return fmt.Sprintf("(docker compose logs: %v)\n%s", err, out)
	}
	return strings.TrimRight(string(out), "\n")
}

// mismatch compares what Icecast reports with o; empty when it matches.
func mismatch(s source, o output) string {
	if s.ServerType != serverTypes[o.Codec] {
		return fmt.Sprintf("server_type %q, want %q", s.ServerType, serverTypes[o.Codec])
	}
	info := map[string]string{}
	for kv := range strings.SplitSeq(s.AudioInfo, ";") {
		k, v, _ := strings.Cut(kv, "=")
		info[strings.TrimPrefix(k, "ice-")] = v
	}
	want := map[string]string{
		"channels": fmt.Sprint(o.Channels), "samplerate": fmt.Sprint(o.SampleRateHz), "bitrate": fmt.Sprint(o.BitrateKbps),
	}
	for k, v := range want {
		if info[k] != v {
			return fmt.Sprintf("audio_info %q, want %s=%s", s.AudioInfo, k, v)
		}
	}
	if s.ServerName != o.Name || s.Genre != o.Genre {
		return fmt.Sprintf("name %q genre %q, want %q %q", s.ServerName, s.Genre, o.Name, o.Genre)
	}
	return ""
}

func sources(ctx context.Context, base string) ([]source, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/status-json.xsl", http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Icestats struct {
			Source json.RawMessage `json:"source"`
		} `json:"icestats"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("icecast status: %w", err)
	}
	raw := strings.TrimSpace(string(doc.Icestats.Source))
	switch {
	case raw == "" || raw == "null":
		return nil, nil
	case raw[0] == '[':
		var ss []source
		return ss, json.Unmarshal([]byte(raw), &ss)
	default:
		var s source
		return []source{s}, json.Unmarshal([]byte(raw), &s)
	}
}

// --- Telnet -------------------------------------------------------------------

type telnet struct {
	conn net.Conn
	r    *bufio.Reader
}

// dialTelnet connects once the control port answers a command.
func dialTelnet(ctx context.Context, addr string, wait time.Duration) (*telnet, error) {
	deadline := time.Now().Add(wait)
	for {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			t := &telnet{conn: conn, r: bufio.NewReader(conn)}
			if _, err = t.command("uptime"); err == nil {
				return t, nil
			}
			t.close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("liquidsoap telnet at %s: %w", addr, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (t *telnet) close() { _ = t.conn.Close() }

// command sends one line and returns the reply lines before END, joined.
func (t *telnet) command(cmd string) (string, error) {
	if err := t.conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}
	if _, err := io.WriteString(t.conn, cmd+"\n"); err != nil {
		return "", err
	}
	var lines []string
	for {
		line, err := t.r.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("telnet %q: %w", strings.SplitN(cmd, " ", 2)[0], err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "END" {
			return strings.TrimSpace(strings.Join(lines, " ")), nil
		}
		lines = append(lines, line)
	}
}
