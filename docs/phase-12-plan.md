# Phase 12 Plan — Live Voice

Status: Done
Based on: `base.md` (Remote Live Broadcasting), `tech-design.md` (§2, §3, §6, §6.3, §6.5, §8.4, §8.5, §8.6, §8.9, §9.2, §10, §10.1, §12), `backend-init-plan.md` §9 item 12
Last updated: 2026-10-14

## 1. Goal

The manager goes on air from the panel: the browser captures the
microphone and sends it over WebRTC to the backend, which relays it into
Liquidsoap's harbor input. Liquidsoap mixes the voice over the music and
lowers the music while the microphone is live (ducking, §8.5). The panel
sees the live state over WebSocket, and the update script's live check
(`GET /api/status/live`) finally answers with real values.

Phase 1 left the hooks: the dev script has `input.harbor` (mount `live`,
port 8005, the control credentials) mixed over automation with `add`; the
settings have `liquidsoap.harbor_port`; the log has the `webrtc` component.

## 2. Decisions (2026-10-14)

1. **No transcoding: Opus in, Ogg/Opus out.** Browsers send Opus (48 kHz)
   over WebRTC. The bridge does not decode it: each RTP payload is written
   as an Ogg/Opus page and streamed to the harbor with the Icecast source
   protocol (`SOURCE /live`, basic auth with `LIQUIDSOAP_CONTROL_USER` and
   `LIQUIDSOAP_CONTROL_PASSWORD`, `Content-Type: audio/ogg`). Liquidsoap
   2.4.6 decodes it (checked on the dev stack). The stream header carries
   the channel count of the packets (a microphone is usually mono), because
   Liquidsoap refuses packets that do not match it; a change starts a new
   stream. No cgo, no ffmpeg process,
   no added latency in Go. The harbor host is `liquidsoap.control_host`,
   its port `liquidsoap.harbor_port`; the mount `live` is fixed in both
   scripts.
2. **WebRTC library: `github.com/pion/webrtc/v4`** (new dependency). Go has
   no WebRTC stack in the standard library; pion is the pure-Go
   implementation (ICE, DTLS, SRTP), so the image stays cgo-free.
3. **Signalling is one HTTP exchange, without trickle ICE.** The panel
   gathers its candidates and sends the complete offer to
   `POST /api/live/session`; the answer carries the complete answer SDP
   (the backend has only host candidates, so gathering is immediate,
   bounded by 2 s). The offer must have an audio section with Opus;
   otherwise 422 `live_offer_invalid`. No STUN or TURN: the API and the
   browser are on localhost or the private network (§10).
4. **One UDP port for all sessions.** The bridge listens on one UDP socket
   (ICE UDP mux), so Docker publishes one port. Its address is the
   environment variable `LIVE_UDP_ADDR` (default `127.0.0.1:8189`; in a
   container `0.0.0.0:8189`), like `HTTP_ADDR`: a listen address tied to the
   compose port mapping, not something the manager tunes. The addresses
   the browser can reach that port on are `LIVE_PUBLIC_IPS` (comma
   separated, optional): inside Docker the container's own address is not
   reachable from the browser, so the compose file sets `127.0.0.1` (and
   the LAN address of the host when the panel is used from another PC).
   Empty: the addresses of the host's interfaces, loopback included (for
   `make run`). The browser's mDNS candidates are not resolved; its address
   is learned from its connectivity checks (peer-reflexive).
5. **One session at a time** (one manager). An offer while a session is
   active is refused with 409 `live_session_active`, unless `takeover` is
   true: the old session ends (`replaced`) and the new one starts — a
   reloaded panel can take the microphone back at once.
6. **Session states**: `connecting` (answer sent, no audio yet), `on_air`
   (audio flows to the harbor), `reconnecting` (audio arrives but the
   harbor connection is lost; it is retried every second and packets are
   dropped meanwhile — latency over completeness), and `off`. A session
   ends with a reason: `stopped` (`DELETE /api/live/session`), `replaced`,
   `no_audio` (no packet within 10 s of the answer), `dropout` (no packet
   for 5 s, or the peer connection failed or closed), `shutdown`. The
   state is published as `live.state` on every change and logged
   (`webrtc`; start and end at INFO with the duration, failures at WARN).
7. **The harbor connection opens with the first packet**, so `on_air` means
   audio reaches Liquidsoap. Each connection starts a new Ogg stream (new
   serial number and headers). Writes have a 1 s deadline, so a stalled
   Liquidsoap never blocks the session.
8. **Ducking in Liquidsoap** (§8.5). The script mixes with
   `smooth_add(duration=0.5, p=duck_gain, normal=automation, special=live)`
   instead of `add`: while the harbor has a source, automation fades to
   `duck_gain` of its level, and back afterwards. `duck_gain` is an
   interactive variable; the backend sets it from the new setting
   `liquidsoap.duck_gain` (default 0.2, 0–1, immediate) on every control
   connection and when the setting changes, as it does for `hour_shift`.
   `music_gain` stays the manager's explicit control (music off for an
   interview). The harbor buffer is cut from 12 s to 0.3 s
   (`buffer=0.3, max=2.`), so voice reaches air within the 1 s target
   (§8.4, §12).
9. **Live voice is not an automation item** (§8.4): nothing is written to
   `play_log` or the plan, and no migration is needed. Live time is never
   missed. The log lines are the record of when the station was live
   (§12).
10. **Live status** (`GET /api/status/live`, deploy plan 8.5): `on_air` is
    true while a live session is `on_air` or `reconnecting`;
    `open_outage_id` is the open outage; `current_clock` is the clock the
    grid assigns to the current station hour. A field whose source fails
    stays null (unknown, treated as on air by the update script).
11. **Ownership.** `live.Bridge` owns the UDP socket and at most one
    session (two for the instant of a takeover). A session has one reader
    goroutine (the pion track handler) that writes to the harbor; pion's
    own goroutines end when the peer connection closes. Every session
    context derives from the service context; `Bridge.Run` closes the
    active session and the socket on shutdown. No goroutine per packet.

## 3. Packages

| Package | Contents |
|---------|----------|
| `internal/live` | `Bridge`: UDP mux, sessions (offer/answer, takeover, stop, timers), state and events; `Harbor`: Icecast source client writing Ogg/Opus pages; no SQL, no repository |
| `internal/playout` | Sets `duck_gain` in Liquidsoap (on connect and on change) |
| `internal/outage` | `Service.OpenOutage` for the live status |
| `internal/config` | `LIVE_UDP_ADDR`, `LIVE_PUBLIC_IPS` |
| `internal/settings` | `liquidsoap.duck_gain` |
| `dev/liquidsoap/radio.liq` | `smooth_add` ducking with `duck_gain`, short harbor buffer |
| `docker-compose.dev.yml` | Backend: `LIVE_UDP_ADDR`, `LIVE_PUBLIC_IPS`, UDP port 8189 on 127.0.0.1 |
| `dev/livecheck` | Dev tool: sends an Ogg/Opus file to a running backend as a WebRTC client (`make live-check`), to check the path end to end without a browser |

## 4. API

| Method and path | Purpose |
|-----------------|---------|
| `POST /api/live/session` | Body `offer` (SDP), `takeover`; 201 with `session_id` and `answer` (SDP) |
| `DELETE /api/live/session` | Ends the session (`stopped`); 204, also when there is none |
| `GET /api/live/status` | State, session ID, `started_at` (on air since), harbor connected, packets relayed, last packet time |
| `GET /api/status/live` | Gains real `on_air`, `open_outage_id`, `current_clock` |

New problem codes: `live_session_active` (409), `live_offer_invalid` (422),
`live_unavailable` (503: the UDP port could not be opened). New event
(named schema): `live.state`. No operation kinds (everything is
synchronous).

## 5. Known limits (for later phases)

- Browsers allow the microphone only in a secure context: the panel must
  be opened on `localhost` or over HTTPS. A panel on a LAN address over
  plain HTTP cannot go live (deploy and frontend).
- Ducking follows the harbor connection, not voice activity: the music
  stays low while the microphone is open, also in pauses.
- Lost packets are not concealed (no decoding in Go); a lost packet is a
  20 ms gap.
- The production script (`radio-atlantida-deploy/config/`) must get the
  `smooth_add` ducking, `duck_gain`, and the short harbor buffer in the
  same release, and the production compose must publish the UDP port and
  set `LIVE_UDP_ADDR` and `LIVE_PUBLIC_IPS`.
- Live sessions are not stored; the panel has no history of them.
- The 1 s latency target is not measured end to end; it is to be checked
  with a real microphone and a listener before the first air date.
- A harbor that accepts and then drops the source again and again makes
  the session flap between `on_air` and `reconnecting` (an event and log
  line each second).

## 6. Done when

Unit tests cover the harbor client (handshake, auth, refusal, Ogg pages and
their channel count), the bridge with a pion peer in the test as the
browser (offer/answer, states, `no_audio`, `dropout`, takeover, stop,
shutdown, invalid offers, harbor down and back), the `duck_gain` push in
playout, and the config. API tests cover the session endpoints, their
refusals, and the live status. On the dev stack, `make live-check` puts a
test tone on air over WebRTC and the music ducks under it. `make lint`,
`make test-integration`, and `make openapi-lint` pass.

Checked on the dev stack 2026-10-14 (backend container, UDP 8189
published on 127.0.0.1): `make live-check` connected over ICE through the
port mapping, went `on_air` 27 ms after the answer, and relayed 1,000
packets (20 s) that Liquidsoap decoded without a gap; `GET /api/status/live`
answered `on_air: true` meanwhile. In a recording of the stream, the
automation tone fell to 0.20 of its level within 0.5 s of the voice and
came back afterwards. The first run showed that Liquidsoap refuses mono
packets in a stream whose header says stereo (`Wrong number of
channels`), so the header now follows the packets. The end-to-end latency
was not measured exactly (Icecast's burst on connect hides it); the path
adds the 0.3 s harbor buffer plus Liquidsoap's frame and encoder delay.
