// Command livecheck goes live through a running backend without a browser:
// it sends an Ogg/Opus file over WebRTC, as the panel sends the microphone,
// and prints the live state while it plays (docs/phase-12-plan.md). Used by
// `make live-check` on the dev stack.
//
// The file must hold one Opus packet per Ogg page (ffmpeg -page_duration
// 20000), as dev/scripts/gen-live.sh writes it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/oggreader"
)

func main() {
	api := flag.String("api", "http://127.0.0.1:8090", "backend base URL")
	file := flag.String("file", ".dev/live-check.ogg", "Ogg/Opus file, one packet per page")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, *api, *file); err != nil {
		fmt.Fprintln(os.Stderr, "livecheck:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, api, file string) error {
	f, err := os.Open(file) //nolint:gosec // G304: a dev tool reads the file it is given
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	ogg, _, err := oggreader.NewWith(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}

	pc, track, err := newPeer()
	if err != nil {
		return err
	}
	defer func() { _ = pc.Close() }()
	connected := make(chan struct{})
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		fmt.Println("ice:", s)
		if s == webrtc.ICEConnectionStateConnected {
			close(connected)
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	<-gathered

	var session struct {
		SessionID uint64 `json:"session_id"`
		Answer    string `json:"answer"`
	}
	body, _ := json.Marshal(map[string]any{"offer": pc.LocalDescription().SDP, "takeover": true})
	if err := call(ctx, http.MethodPost, api+"/api/live/session", body, &session); err != nil {
		return err
	}
	fmt.Println("session:", session.SessionID)
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: session.Answer}); err != nil {
		return err
	}
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		return errors.New("ICE did not connect within 5 s")
	case <-ctx.Done():
		return ctx.Err()
	}

	start := time.Now()
	sent, lastStatus := 0, time.Time{}
	var granule uint64
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		page, hdr, err := ogg.ParseNextPage()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read page: %w", err)
		}
		if _, isHeader := hdr.HeaderType(page); isHeader {
			continue
		}
		// Granule positions count 48 kHz samples; one page is 20 ms.
		d := time.Duration(hdr.GranulePosition-granule) * time.Second / 48000 //nolint:gosec // G115: a page's samples fit int64
		granule = hdr.GranulePosition
		if err := track.WriteSample(media.Sample{Data: page, Duration: d}); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		sent++
		if time.Since(lastStatus) >= time.Second {
			lastStatus = time.Now()
			printStatus(ctx, api)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	fmt.Printf("sent %d packets in %s\n", sent, time.Since(start).Round(time.Millisecond))
	printStatus(ctx, api)
	if err := call(ctx, http.MethodDelete, api+"/api/live/session", nil, nil); err != nil {
		return err
	}
	printStatus(ctx, api)
	return nil
}

// newPeer is the browser's side: Opus out, loopback only (the dev backend
// listens on 127.0.0.1).
func newPeer() (*webrtc.PeerConnection, *webrtc.TrackLocalStaticSample, error) {
	var se webrtc.SettingEngine
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, nil, err
	}
	pc, err := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, nil, err
	}
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "livecheck")
	if err != nil {
		return nil, nil, err
	}
	if _, err := pc.AddTrack(track); err != nil {
		return nil, nil, err
	}
	return pc, track, nil
}

func printStatus(ctx context.Context, api string) {
	var st map[string]any
	if err := call(ctx, http.MethodGet, api+"/api/live/status", nil, &st); err != nil {
		fmt.Println("status:", err)
		return
	}
	fmt.Printf("status: state=%v harbor_connected=%v packets=%v\n", st["state"], st["harbor_connected"], st["packets"])
}

func call(ctx context.Context, method, url string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s %s", method, url, resp.Status, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
