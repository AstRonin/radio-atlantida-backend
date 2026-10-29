package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// LokiJob is the job label of the backend's lines in Loki. The playout
// journal has its own job (playout-journal), collected by Alloy
// (docs/logs-loki-plan.md §3.3).
const LokiJob = "backend"

const (
	// lokiBuffer is how many entries wait for the push. A slow or stopped
	// Loki must never block a logger, so an entry that finds the buffer full
	// is dropped and counted; stdout still has it. 4096 entries cover many
	// seconds of a busy backend at the flush interval.
	lokiBuffer = 4096
	// lokiBatchSize caps the entries of one push request.
	lokiBatchSize = 500
	// lokiFlushEvery is how often a partial batch is sent.
	lokiFlushEvery = time.Second
	// lokiRequestTimeout bounds one push request.
	lokiRequestTimeout = 5 * time.Second
	// lokiAttempts is how many times a push that fails is sent (the first
	// try included). A rejected push (4xx) is not retried.
	lokiAttempts = 3
	// lokiBackoff is the wait before the second attempt; it doubles after.
	lokiBackoff = 500 * time.Millisecond
	// lokiFlushTimeout bounds the flush at shutdown, so shutdown stays
	// within the container's stop grace period.
	lokiFlushTimeout = 3 * time.Second
	// lokiReportEvery is how often the drop and loss counters are logged.
	lokiReportEvery = time.Minute
	// lokiNone is the label value of an entry without a component (or
	// level), so every line carries every label (logs-loki-plan §3.3).
	lokiNone = "none"
)

// errRejected marks a push that Loki refused with a 4xx status. Retrying the
// same batch would be refused again.
var errRejected = errors.New("loki rejected the push")

// LokiSink sends each log line to Loki's push API (logs-loki-plan §4.1).
//
// Write is called from the async writer's goroutine and never blocks: it
// copies the line into a bounded buffer or drops it and counts it. One
// goroutine, Run, owns the batching, the requests, the retries and the
// counters' report, so it needs no lock beyond the channel and the atomics.
type LokiSink struct {
	pushURL string
	client  *http.Client
	// log writes to stdout only. The sink never logs through itself, so a
	// failing Loki cannot feed back into the buffer.
	log *slog.Logger
	ch  chan []byte

	// dropped counts lines that found the buffer full. lost counts lines of
	// batches that failed after every attempt. Both are reported and reset
	// by report.
	dropped atomic.Uint64
	lost    atomic.Uint64
	// lastErr is the error of the last failed batch. Only Run touches it.
	lastErr error

	// flushEvery and reportEvery are fixed by the constants; tests set them
	// before Run starts.
	flushEvery  time.Duration
	reportEvery time.Duration
	backoff     time.Duration
}

// newLokiSink returns a sink for the Loki at baseURL, with a buffer of size
// lines. It does not start the goroutine: Run does.
func newLokiSink(baseURL string, log *slog.Logger, size int) *LokiSink {
	return &LokiSink{
		pushURL:     strings.TrimRight(baseURL, "/") + "/loki/api/v1/push",
		client:      &http.Client{},
		log:         log,
		ch:          make(chan []byte, size),
		flushEvery:  lokiFlushEvery,
		reportEvery: lokiReportEvery,
		backoff:     lokiBackoff,
	}
}

// Write queues a copy of the line. It never blocks: when the buffer is full
// the line is dropped and counted. It always reports success, so the other
// writers of the tee are not affected by Loki.
func (s *LokiSink) Write(p []byte) (int, error) {
	// The writer that calls us reuses p after Write returns.
	line := make([]byte, len(p))
	copy(line, p)
	select {
	case s.ch <- line:
	default:
		s.dropped.Add(1)
	}
	return len(p), nil
}

// Run sends the buffered lines until ctx ends. Then it sends what is still
// buffered, with a timeout that outlives ctx, and returns nil. A failing Loki
// never makes Run return an error: the loss is counted and reported.
func (s *LokiSink) Run(ctx context.Context) error {
	flush := time.NewTicker(s.flushEvery)
	defer flush.Stop()
	report := time.NewTicker(s.reportEvery)
	defer report.Stop()

	var batch []pendingLine
	for {
		select {
		case line := <-s.ch:
			batch = append(batch, parseLine(line, time.Now()))
			if len(batch) >= lokiBatchSize {
				s.flush(ctx, batch)
				batch = batch[:0]
			}
		case <-flush.C:
			s.flush(ctx, batch)
			batch = batch[:0]
		case <-report.C:
			s.report()
		case <-ctx.Done():
			batch = s.drain(batch)
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lokiFlushTimeout)
			s.flush(fctx, batch)
			cancel()
			s.report()
			return nil
		}
	}
}

// Close sends the lines still buffered, synchronously, within ctx. It is the
// last step of the sink: Logging.Close calls it after the async writer has
// drained into the buffer, when Run has returned (so it is the only reader).
func (s *LokiSink) Close(ctx context.Context) {
	s.flush(ctx, s.drain(nil))
	s.report()
}

// drain moves every line that is buffered now into batch, without waiting.
func (s *LokiSink) drain(batch []pendingLine) []pendingLine {
	for {
		select {
		case line := <-s.ch:
			batch = append(batch, parseLine(line, time.Now()))
		default:
			return batch
		}
	}
}

// flush sends batch in one push request. A failed batch is counted as lost.
func (s *LokiSink) flush(ctx context.Context, batch []pendingLine) {
	if len(batch) == 0 {
		return
	}
	body, err := encodePush(batch)
	if err == nil {
		err = s.send(ctx, body)
	}
	if err != nil {
		s.lastErr = err
		s.lost.Add(uint64(len(batch)))
	}
}

// send posts body, retrying a failed attempt with doubling backoff. It stops
// early when ctx ends.
func (s *LokiSink) send(ctx context.Context, body []byte) error {
	var err error
	for attempt := range lokiAttempts {
		if attempt > 0 {
			t := time.NewTimer(s.backoff << (attempt - 1))
			select {
			case <-ctx.Done():
				t.Stop()
				return fmt.Errorf("push to loki: %w (after %d attempts)", ctx.Err(), attempt)
			case <-t.C:
			}
		}
		err = s.post(ctx, body)
		if err == nil || errors.Is(err, errRejected) {
			return err
		}
	}
	return err
}

// post makes one push request with its own deadline.
func (s *LokiSink) post(ctx context.Context, body []byte) error {
	rctx, cancel := context.WithTimeout(ctx, lokiRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, s.pushURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("push to loki: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain a little so the connection can be reused; the body is not needed.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return fmt.Errorf("%w: status %d", errRejected, resp.StatusCode)
	default:
		return fmt.Errorf("push to loki: status %d", resp.StatusCode)
	}
}

// report logs the lines dropped and lost since the last report, if any. It
// runs once a minute and at shutdown, so the loss is visible without a line
// per dropped entry.
func (s *LokiSink) report() {
	dropped := s.dropped.Swap(0)
	lost := s.lost.Swap(0)
	if dropped == 0 && lost == 0 {
		return
	}
	s.log.Warn("log lines not sent to loki",
		"dropped", dropped, "lost", lost, KeyError, s.lastErr)
}

// pendingLine is a buffered line with the labels and timestamp read from it.
type pendingLine struct {
	ts        time.Time
	level     string
	component string
	line      string
}

// parseLine reads the labels and the timestamp of a JSON line from the
// handler. A line that does not parse keeps the arrival time and the "none"
// labels; it is still sent, so nothing is lost by a surprise in the format.
func parseLine(raw []byte, arrived time.Time) pendingLine {
	var f struct {
		Time      string `json:"time"`
		Level     string `json:"level"`
		Component string `json:"component"`
	}
	_ = json.Unmarshal(raw, &f) // a failed parse leaves the zero values, as documented above
	p := pendingLine{ts: arrived, level: f.Level, component: f.Component, line: strings.TrimSuffix(string(raw), "\n")}
	if t, err := time.Parse(time.RFC3339Nano, f.Time); err == nil {
		p.ts = t
	}
	if p.level == "" {
		p.level = lokiNone
	}
	if p.component == "" {
		p.component = lokiNone
	}
	return p
}

// pushRequest is the body of Loki's push API (JSON form).
type pushRequest struct {
	Streams []pushStream `json:"streams"`
}

type pushStream struct {
	Stream map[string]string `json:"stream"`
	// Values are [timestamp in nanoseconds, line].
	Values [][2]string `json:"values"`
}

// encodePush groups the batch into one stream per label set: job, level,
// and component.
func encodePush(batch []pendingLine) ([]byte, error) {
	index := map[[2]string]int{}
	var req pushRequest
	for _, p := range batch {
		key := [2]string{p.level, p.component}
		i, ok := index[key]
		if !ok {
			i = len(req.Streams)
			index[key] = i
			req.Streams = append(req.Streams, pushStream{
				Stream: map[string]string{"job": LokiJob, "level": p.level, "component": p.component},
			})
		}
		req.Streams[i].Values = append(req.Streams[i].Values,
			[2]string{strconv.FormatInt(p.ts.UnixNano(), 10), p.line})
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode push: %w", err)
	}
	return body, nil
}
