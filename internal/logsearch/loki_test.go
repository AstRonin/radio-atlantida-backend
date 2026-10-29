package logsearch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// lastRequest holds the URL of the last request a stub received.
type lastRequest struct {
	mu  sync.Mutex
	url *url.URL
}

// URL returns the URL of the last request, or nil before the first one.
func (l *lastRequest) URL() *url.URL {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.url
}

// lokiStub answers every request with status and body and records the URL
// of each request.
func lokiStub(t *testing.T, status int, body string) (*Client, *lastRequest) {
	t.Helper()
	last := &lastRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last.mu.Lock()
		last.url = r.URL
		last.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL+"/", srv.Client()), last
}

func TestClientQueryRangeSendsParamsAndDecodesStreams(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/loki/api/v1/query_range", r.URL.Path)
		q := r.URL.Query()
		require.Equal(t, `{job="backend"} |= "x"`, q.Get("query"))
		require.Equal(t, "1700000000000000000", q.Get("start"))
		require.Equal(t, "1700003600000000000", q.Get("end"))
		require.Equal(t, "11", q.Get("limit"))
		require.Equal(t, "backward", q.Get("direction"))
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"level":"INFO","component":"api"},"values":[["1700000001000000001","{\"msg\":\"a\"}"],["1700000000000000002","{\"msg\":\"b\"}"]]},
			{"stream":{"level":"ERROR"},"values":[["1700000002000000000","{\"msg\":\"c\"}","{\"trace\":\"t\"}"]]}
		]}}`))
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, srv.Client())

	streams, err := c.QueryRange(t.Context(), RangeQuery{
		Expr:  `{job="backend"} |= "x"`,
		Start: time.Unix(0, 1700000000000000000),
		End:   time.Unix(0, 1700003600000000000),
		Limit: 11,
	})

	require.NoError(t, err)
	require.Len(t, streams, 2)
	require.Equal(t, "api", streams[0].Labels["component"])
	require.Len(t, streams[0].Lines, 2)
	require.Equal(t, `{"msg":"a"}`, streams[0].Lines[0].Text)
	require.Equal(t, time.Unix(0, 1700000001000000001).UTC(), streams[0].Lines[0].Time)
	require.Len(t, streams[1].Lines, 1, "a third element (metadata) is ignored")
	require.Equal(t, `{"msg":"c"}`, streams[1].Lines[0].Text)
}

func TestClientCountDecodesVectorAndEmptyIsZero(t *testing.T) {
	t.Parallel()
	c, last := lokiStub(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"42"]}]}}`)
	n, err := c.Count(t.Context(), CountQuery{Expr: "sum(count_over_time({job=\"x\"} [60s]))", At: time.Unix(1700000000, 0)})
	require.NoError(t, err)
	require.Equal(t, int64(42), n)
	require.Equal(t, "/loki/api/v1/query", last.URL().Path)
	require.Equal(t, "1700000000000000000", last.URL().Query().Get("time"))

	c, _ = lokiStub(t, http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	n, err = c.Count(t.Context(), CountQuery{Expr: "x", At: time.Unix(1, 0)})
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestClientStatusMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		body   string
		want   error // nil: an unexpected error, neither sentinel
	}{
		{"server error", http.StatusInternalServerError, "boom", ErrUnavailable},
		{"bad gateway", http.StatusBadGateway, "", ErrUnavailable},
		{"rate limited", http.StatusTooManyRequests, "", ErrUnavailable},
		{
			"range over the limit", http.StatusBadRequest,
			"the query time range exceeds the limit (query length: 2000h0m0s, limit: 721h0m0s)", ErrRangeTooLong,
		},
		{"other bad request is a bug", http.StatusBadRequest, "parse error at line 1", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, _ := lokiStub(t, tt.status, tt.body)
			_, err := c.QueryRange(t.Context(), RangeQuery{Expr: "x", Start: time.Unix(0, 1), End: time.Unix(0, 2), Limit: 1})
			require.Error(t, err)
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				return
			}
			require.NotErrorIs(t, err, ErrUnavailable)
			require.NotErrorIs(t, err, ErrRangeTooLong)
		})
	}
}

func TestClientUnreachableIsUnavailable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close() // nothing listens any more

	_, err := NewClient(base, nil).QueryRange(t.Context(), RangeQuery{Expr: "x", Start: time.Unix(0, 1), End: time.Unix(0, 2)})

	require.ErrorIs(t, err, ErrUnavailable)
}

func TestClientDeadlineIsUnavailable(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	_, err := NewClient(srv.URL, nil).Count(ctx, CountQuery{Expr: "x", At: time.Unix(1, 0)})

	require.ErrorIs(t, err, ErrUnavailable)
	require.True(t, errors.Is(err, context.DeadlineExceeded))
}

func TestClientWithoutURLIsUnavailable(t *testing.T) {
	t.Parallel()
	_, err := NewClient("", nil).QueryRange(t.Context(), RangeQuery{Expr: "x", Start: time.Unix(0, 1), End: time.Unix(0, 2)})
	require.ErrorIs(t, err, ErrUnavailable)
}
