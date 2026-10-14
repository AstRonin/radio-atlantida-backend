package outage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// icecastTimeout bounds one mount check; a slower answer counts as a
	// failed check.
	icecastTimeout = 3 * time.Second
	// maxStatusBytes bounds the status document read; a few sources take a
	// few kilobytes.
	maxStatusBytes = 1 << 20
)

// IcecastTarget is the mount the check looks for (settings
// stream.icecast_host, stream.icecast_port, stream.mount).
type IcecastTarget struct {
	Host  string
	Port  int
	Mount string
}

// errMountMissing: Icecast answers, but no source streams to the mount.
var errMountMissing = errors.New("mount not found on the icecast server")

// IcecastChecker checks that a source streams to the station's mount, as a
// listener would find it (§7.1): it reads the server's /status-json.xsl.
type IcecastChecker struct {
	client *http.Client
}

// NewIcecastChecker returns a checker that uses client.
func NewIcecastChecker(client *http.Client) *IcecastChecker {
	return &IcecastChecker{client: client}
}

// Check returns nil when the mount has a source.
func (c *IcecastChecker) Check(ctx context.Context, t IcecastTarget) error {
	ctx, cancel := context.WithTimeout(ctx, icecastTimeout)
	defer cancel()
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(t.Host, strconv.Itoa(t.Port)), Path: "/status-json.xsl"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return fmt.Errorf("icecast status request: %w", err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("icecast status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("icecast status: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBytes))
	if err != nil {
		return fmt.Errorf("read icecast status: %w", err)
	}
	ok, err := hasMount(body, t.Mount)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", errMountMissing, t.Mount)
	}
	return nil
}

// hasMount reports whether the Icecast status document lists a source on
// mount. Icecast 2.4 sends "source" as an object for one source, as an
// array for several, and leaves it out when there is none.
func hasMount(body []byte, mount string) (bool, error) {
	var doc struct {
		Icestats struct {
			Source json.RawMessage `json:"source"`
		} `json:"icestats"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, fmt.Errorf("parse icecast status: %w", err)
	}
	raw := bytes.TrimSpace(doc.Icestats.Source)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false, nil
	}
	type source struct {
		ListenURL string `json:"listenurl"`
	}
	var sources []source
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &sources); err != nil {
			return false, fmt.Errorf("parse icecast sources: %w", err)
		}
	} else {
		var one source
		if err := json.Unmarshal(raw, &one); err != nil {
			return false, fmt.Errorf("parse icecast source: %w", err)
		}
		sources = []source{one}
	}
	for _, s := range sources {
		u, err := url.Parse(s.ListenURL)
		if err == nil && u.Path == mount {
			return true, nil
		}
	}
	return false, nil
}
