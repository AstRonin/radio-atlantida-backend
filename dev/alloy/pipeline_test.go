//go:build integration

// Package alloy_test runs the journal pipeline of the dev stack, Alloy with
// config.alloy sending to Loki with ../loki/loki.yaml, on the images pinned in
// docker-compose.dev.yml, and checks what reaches Loki
// (docs/phase-15-plan.md §5, task 3; docs/logs-loki-plan.md §5.1). The files
// are copied to radio-atlantida-deploy, so this test guards both.
package alloy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.yaml.in/yaml/v3"
)

const (
	// collectTimeout bounds the wait for a line to reach Loki: the 10 s
	// sync_period of local.file_match plus polling and the push.
	collectTimeout = time.Minute
	// journalTime is the journal's timestamp format (milliseconds, dev/liquidsoap/radio.liq).
	journalTime = "2006-01-02T15:04:05.000Z"
)

func TestPipeline(t *testing.T) {
	t.Parallel()
	journal := t.TempDir()
	now := time.Now().UTC()
	tomorrow := now.Add(24 * time.Hour)

	tests := []struct {
		name string
		// afterStart: the file appears while Alloy runs; otherwise it is on
		// disk before the first start.
		afterStart bool
		dir, file  string
		line       string
		selector   string
		want       time.Time
		wantLabels map[string]string
	}{
		{
			name: "journal start",
			dir:  journal, file: "journal-" + day(now) + ".jsonl",
			line: fmt.Sprintf(`{"event":"start","time":%q,"source":"plan","rid":"0","item_id":"42","track_id":"","uri":"/media/a.mp3","filename":"/media/a.mp3","elapsed_ms":0}`,
				now.Format(journalTime)),
			selector: `{job="playout-journal", event="start"} |= "a.mp3"`,
			want:     now.Truncate(time.Millisecond),
			wantLabels: map[string]string{
				"job": "playout-journal", "event": "start", "source": "plan", "file": "journal-" + day(now) + ".jsonl",
			},
		},
		{
			name: "journal output change",
			dir:  journal, file: "journal-" + day(now) + ".jsonl",
			line:     fmt.Sprintf(`{"event":"output","time":%q,"connected":false}`, now.Add(time.Second).Format(journalTime)),
			selector: `{job="playout-journal", event="output"}`,
			want:     now.Add(time.Second).Truncate(time.Millisecond),
			wantLabels: map[string]string{
				"job": "playout-journal", "event": "output", "file": "journal-" + day(now) + ".jsonl",
			},
		},
		{
			// The daily switch at 00:00 UTC: a new file name while Alloy runs.
			// The line time is now, as Loki refuses lines from the future.
			name:       "next day's journal file",
			afterStart: true,
			dir:        journal, file: "journal-" + day(tomorrow) + ".jsonl",
			line: fmt.Sprintf(`{"event":"output","time":%q,"connected":true,"rid":"next-day"}`,
				now.Add(2*time.Second).Format(journalTime)),
			selector: `{job="playout-journal", event="output"} |= "next-day"`,
			want:     now.Add(2 * time.Second).Truncate(time.Millisecond),
			wantLabels: map[string]string{
				"job": "playout-journal", "event": "output", "file": "journal-" + day(tomorrow) + ".jsonl",
			},
		},
	}
	for _, tt := range tests {
		if !tt.afterStart {
			appendLine(t, filepath.Join(tt.dir, tt.file), tt.line)
		}
	}

	s := startStack(t, journal)
	from, to := now.Add(-48*time.Hour), now.Add(time.Hour)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.afterStart {
				appendLine(t, filepath.Join(tt.dir, tt.file), tt.line)
			}
			got := waitEntries(t, s.loki, tt.selector, from, to, 1)
			require.Equal(t, tt.line, got[0].Line)
			require.Equal(t, tt.want.UnixNano(), got[0].Time, "the entry's own time")
			require.Equal(t, tt.wantLabels, got[0].Labels)
		})
	}

	t.Run("labels are small closed sets", func(t *testing.T) {
		// item_id, track_id, uri and filename stay in the line.
		require.Equal(t, []string{"event", "file", "job", "source"},
			labelNames(t, s.loki, from, to))
	})

	t.Run("restart reads nothing twice", func(t *testing.T) {
		all := `{job="playout-journal"}`
		before := len(waitEntries(t, s.loki, all, from, to, len(tests)))

		s.restartAlloy(t)
		appendLine(t, filepath.Join(journal, "journal-"+day(now)+".jsonl"),
			fmt.Sprintf(`{"event":"output","time":%q,"connected":true,"rid":"after-restart"}`,
				now.Add(3*time.Second).Format(journalTime)))
		waitEntries(t, s.loki, `{job="playout-journal"} |= "after-restart"`, from, to, 1)

		// Alloy's counter starts at zero with the process: only the new line
		// was read. Loki may merge identical entries at query time, so its
		// count alone would not show a repeat.
		require.InDelta(t, 1, metricSum(t, s.alloy, "loki_source_file_read_lines_total"), 0)
		require.Len(t, waitEntries(t, s.loki, all, from, to, before+1), before+1)
	})
}

// stack is Loki and Alloy on one network, as in docker-compose.dev.yml.
type stack struct {
	alloyCtr *testcontainers.DockerContainer
	loki     string // base URL from the host
	alloy    string // base URL from the host; changes on restart
}

func startStack(t *testing.T, journal string) *stack {
	t.Helper()
	ctx := t.Context()
	lokiImage, alloyImage := pinnedImages(t)

	nw, err := network.New(ctx)
	testcontainers.CleanupNetwork(t, nw)
	require.NoError(t, err)

	lokiCtr, err := testcontainers.Run(ctx, lokiImage,
		network.WithNetwork([]string{"loki"}, nw),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath: "../loki/loki.yaml", ContainerFilePath: "/etc/loki/loki.yaml", FileMode: 0o644,
		}),
		testcontainers.WithCmd("-config.file=/etc/loki/loki.yaml"),
		testcontainers.WithExposedPorts("3100/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/ready").WithPort("3100/tcp").WithStartupTimeout(2*time.Minute)),
	)
	testcontainers.CleanupContainer(t, lokiCtr)
	dumpLogsOnFailure(t, "loki", lokiCtr) // cleanups run last first: before the removal
	require.NoError(t, err)

	alloyCtr, err := testcontainers.Run(ctx, alloyImage,
		network.WithNetwork([]string{"alloy"}, nw),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath: "config.alloy", ContainerFilePath: "/etc/alloy/config.alloy", FileMode: 0o644,
		}),
		// The command of the alloy service in docker-compose.dev.yml.
		testcontainers.WithCmd("run", "--storage.path=/var/lib/alloy/data",
			"--server.http.listen-addr=0.0.0.0:12345", "/etc/alloy/config.alloy"),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Binds = append(hc.Binds, journal+":/journal:ro")
		}),
		testcontainers.WithExposedPorts("12345/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/-/ready").WithPort("12345/tcp").WithStartupTimeout(time.Minute)),
	)
	testcontainers.CleanupContainer(t, alloyCtr)
	dumpLogsOnFailure(t, "alloy", alloyCtr) // cleanups run last first: before the removal
	require.NoError(t, err)

	s := &stack{alloyCtr: alloyCtr}
	s.loki, err = lokiCtr.PortEndpoint(ctx, "3100/tcp", "http")
	require.NoError(t, err)
	s.alloy, err = alloyCtr.PortEndpoint(ctx, "12345/tcp", "http")
	require.NoError(t, err)
	return s
}

// restartAlloy stops Alloy gracefully (it saves its positions) and starts
// the same container again, with the same storage path.
func (s *stack) restartAlloy(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	require.NoError(t, s.alloyCtr.Stop(ctx, new(30*time.Second)))
	require.NoError(t, s.alloyCtr.Start(ctx))
	var err error
	s.alloy, err = s.alloyCtr.PortEndpoint(ctx, "12345/tcp", "http")
	require.NoError(t, err)
}

// pinnedImages reads the loki and alloy images of docker-compose.dev.yml, so
// the test runs what the stack runs.
func pinnedImages(t *testing.T) (lokiImage, alloyImage string) {
	t.Helper()
	b, err := os.ReadFile("../../docker-compose.dev.yml")
	require.NoError(t, err)
	var compose struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	require.NoError(t, yaml.Unmarshal(b, &compose))
	lokiImage, alloyImage = compose.Services["loki"].Image, compose.Services["alloy"].Image
	require.NotEmpty(t, lokiImage)
	require.NotEmpty(t, alloyImage)
	return lokiImage, alloyImage
}

func dumpLogsOnFailure(t *testing.T, name string, ctr *testcontainers.DockerContainer) {
	t.Helper()
	if ctr == nil {
		return
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		r, err := ctr.Logs(ctx)
		if err != nil {
			t.Logf("%s logs: %v", name, err)
			return
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		if err != nil {
			t.Logf("%s logs: %v", name, err)
		}
		t.Logf("%s logs:\n%s", name, b)
	})
}

// appendLine appends one line like Liquidsoap does.
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G302,G304: a test file in t.TempDir, read by Alloy in a container
	require.NoError(t, err)
	_, err = f.WriteString(line + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func day(t time.Time) string { return t.Format(time.DateOnly) }

// entry is one log line returned by Loki.
type entry struct {
	Labels map[string]string
	Time   int64 // Unix nanoseconds
	Line   string
}

// waitEntries polls Loki until the query returns exactly n entries or
// collectTimeout passes.
func waitEntries(t *testing.T, loki, query string, from, to time.Time, n int) []entry {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), collectTimeout)
	defer cancel()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		got, err := queryRange(ctx, loki, query, from, to)
		if err == nil && len(got) == n {
			return got
		}
		select {
		case <-ctx.Done():
			require.NoError(t, err, query)
			require.Len(t, got, n, "%s: %+v", query, got)
			return got
		case <-tick.C:
		}
	}
}

func queryRange(ctx context.Context, loki, query string, from, to time.Time) ([]entry, error) {
	q := url.Values{
		"query":     {query},
		"start":     {strconv.FormatInt(from.UnixNano(), 10)},
		"end":       {strconv.FormatInt(to.UnixNano(), 10)},
		"limit":     {"1000"},
		"direction": {"forward"},
	}
	var resp struct {
		Data struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := getJSON(ctx, loki+"/loki/api/v1/query_range?"+q.Encode(), &resp); err != nil {
		return nil, err
	}
	var out []entry
	for _, r := range resp.Data.Result {
		for _, v := range r.Values {
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("timestamp %q: %w", v[0], err)
			}
			out = append(out, entry{Labels: r.Stream, Time: ns, Line: v[1]})
		}
	}
	return out, nil
}

func labelNames(t *testing.T, loki string, from, to time.Time) []string {
	t.Helper()
	q := url.Values{
		"start": {strconv.FormatInt(from.UnixNano(), 10)},
		"end":   {strconv.FormatInt(to.UnixNano(), 10)},
	}
	var resp struct {
		Data []string `json:"data"`
	}
	require.NoError(t, getJSON(t.Context(), loki+"/loki/api/v1/labels?"+q.Encode(), &resp))
	slices.Sort(resp.Data)
	return resp.Data
}

// metricSum adds up every series of a counter on Alloy's /metrics.
func metricSum(t *testing.T, alloy, name string) float64 {
	t.Helper()
	body, err := get(t.Context(), alloy+"/metrics")
	require.NoError(t, err)
	var sum float64
	found := false
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		v, err := strconv.ParseFloat(line[strings.LastIndexByte(line, ' ')+1:], 64)
		require.NoError(t, err, line)
		sum += v
		found = true
	}
	require.NoError(t, sc.Err())
	require.True(t, found, "metric %s not exported", name)
	return sum
}

func getJSON(ctx context.Context, u string, v any) error {
	body, err := get(ctx, u)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode %s: %w", u, err)
	}
	return nil
}

func get(ctx context.Context, u string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", u, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", u, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", u, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: %s: %s", u, resp.Status, body)
	}
	return body, nil
}
