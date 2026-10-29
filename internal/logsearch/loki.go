package logsearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrUnavailable means Loki did not answer: it is down, unreachable, or did
// not answer within the deadline, or no Loki is configured.
var ErrUnavailable = errors.New("logs unavailable")

// ErrRangeTooLong means Loki refused the time range because it is longer than
// its own query limit (max_query_length).
var ErrRangeTooLong = errors.New("time range too long")

// maxResponse bounds the body read from Loki. A page of 500 lines is far
// below it.
const maxResponse = 16 << 20

// Loki is what the search needs from Loki. The search defines it because the
// search is its only user; Client implements it over HTTP.
type Loki interface {
	// QueryRange returns the streams that match the query, newest first
	// when Direction is backward.
	QueryRange(ctx context.Context, q RangeQuery) ([]Stream, error)
	// Count returns the single number of an instant metric query.
	Count(ctx context.Context, q CountQuery) (int64, error)
}

// RangeQuery is a log query over a window of time.
type RangeQuery struct {
	Expr       string
	Start, End time.Time
	Limit      int
}

// CountQuery is an instant metric query.
type CountQuery struct {
	Expr string
	At   time.Time
}

// Stream is the lines of one label set.
type Stream struct {
	Labels map[string]string
	Lines  []Line
}

// Line is one log line with its timestamp.
type Line struct {
	Time time.Time
	Text string
}

// Client talks to Loki's HTTP API. An empty base URL means that no Loki is
// configured: every call fails with ErrUnavailable.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a client for the Loki at baseURL (for example
// http://127.0.0.1:3100). Deadlines come from the context of each call.
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), http: hc}
}

// QueryRange calls /loki/api/v1/query_range.
func (c *Client) QueryRange(ctx context.Context, q RangeQuery) ([]Stream, error) {
	v := url.Values{}
	v.Set("query", q.Expr)
	v.Set("start", nanos(q.Start))
	v.Set("end", nanos(q.End))
	v.Set("limit", strconv.Itoa(q.Limit))
	v.Set("direction", "backward")
	body, err := c.get(ctx, "/loki/api/v1/query_range", v)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Stream map[string]string   `json:"stream"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode loki query_range: %w", err)
	}
	streams := make([]Stream, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		s := Stream{Labels: r.Stream, Lines: make([]Line, 0, len(r.Values))}
		for _, v := range r.Values {
			line, err := parseValue(v)
			if err != nil {
				return nil, err
			}
			s.Lines = append(s.Lines, line)
		}
		streams = append(streams, s)
	}
	return streams, nil
}

// Count calls /loki/api/v1/query for a sum of counts. No series means zero
// matches.
func (c *Client) Count(ctx context.Context, q CountQuery) (int64, error) {
	v := url.Values{}
	v.Set("query", q.Expr)
	v.Set("time", nanos(q.At))
	body, err := c.get(ctx, "/loki/api/v1/query", v)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Data struct {
			Result []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err = json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("decode loki query: %w", err)
	}
	if len(resp.Data.Result) == 0 {
		return 0, nil
	}
	if len(resp.Data.Result[0].Value) != 2 {
		return 0, errors.New("decode loki query: unexpected sample shape")
	}
	var s string
	if err = json.Unmarshal(resp.Data.Result[0].Value[1], &s); err != nil {
		return 0, fmt.Errorf("decode loki count: %w", err)
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("decode loki count: %w", err)
	}
	return int64(n), nil
}

// get makes one GET request. The context carries the deadline of the call.
func (c *Client) get(ctx context.Context, path string, v url.Values) ([]byte, error) {
	if c.base == "" {
		return nil, fmt.Errorf("%w: LOKI_URL is not set", ErrUnavailable)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+v.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build loki request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: loki request: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("%w: read loki response: %w", ErrUnavailable, err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return body, nil
	case resp.StatusCode == http.StatusBadRequest && refusesRange(body):
		return nil, fmt.Errorf("%w: %s", ErrRangeTooLong, strings.TrimSpace(string(body)))
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: loki status %d", ErrUnavailable, resp.StatusCode)
	default:
		// A query the backend built wrongly: a bug, not a state of Loki.
		return nil, fmt.Errorf("loki refused the query: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

// refusesRange reports whether Loki's refusal is for the length of the time
// range. Loki words it "the query time range exceeds the limit".
func refusesRange(body []byte) bool {
	return strings.Contains(string(body), "time range exceeds the limit")
}

// parseValue reads one [timestamp, line] pair. Loki may add a third element
// (structured metadata), which is not used.
func parseValue(v []json.RawMessage) (Line, error) {
	if len(v) < 2 {
		return Line{}, errors.New("decode loki line: want [timestamp, line]")
	}
	var ts, text string
	if err := json.Unmarshal(v[0], &ts); err != nil {
		return Line{}, fmt.Errorf("decode loki timestamp: %w", err)
	}
	if err := json.Unmarshal(v[1], &text); err != nil {
		return Line{}, fmt.Errorf("decode loki line: %w", err)
	}
	ns, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return Line{}, fmt.Errorf("decode loki timestamp: %w", err)
	}
	return Line{Time: time.Unix(0, ns).UTC(), Text: text}, nil
}

// nanos renders t as the Unix nanoseconds Loki takes.
func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }
