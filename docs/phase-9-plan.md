# Phase 9 Plan — Liquidsoap Integration (Playout)

Status: Done
Based on: `base.md`, `tech-design.md` (§4, §4.1, §6.5, §7, §7.1, §8.1–§8.10, §10.1, §10.2), `backend-init-plan.md` §9 item 9, `phase-8-plan.md` §5
Last updated: 2026-10-14

## 1. Goal

The backend puts the track plan on air. It keeps Liquidsoap's request queue
2–3 items ahead (`request.push`), starts every clock exactly on its hour
boundary, turns Liquidsoap's track start and end callbacks into `play_log`
rows, shows what is playing, sets the music gain, and feeds the emergency
playlist when the plan has nothing to play.

Outage detection, missed items, and journal reconciliation are Phase 10. This
phase leaves the hooks they need: the control link state, the last callback
time, the Icecast output state, and plan items that did not air left in
`scheduled_items` untouched.

## 2. Decisions (2026-10-14)

1. **Two hour queues.** Liquidsoap has two request queues, `hour_0` and
   `hour_1`. Hour `H` (a station-local hour starting at UTC instant `t`) is
   fed into `hour_<p>` with `p = ((t − shift) / 3600) mod 2`, where `shift` is
   the station UTC offset modulo one hour (0 for `Asia/Yerevan`). A
   `switch(track_sensitive=false)` in the script selects the queue of the
   current hour by the same formula on its own clock, so the clock change is
   a source change at the boundary (§8.10), not a late push. The backend
   sets `shift` on every connect (`var.set hour_shift = <seconds>`). The
   script flushes the queue it leaves (`flush_and_skip`), so leftovers of an
   hour never play in the next hour of the same parity.
2. **Pre-push.** Items of the next hour are pushed into its queue up to
   `playout.prepush_minutes` (new setting, default 5, 1–15) before its
   start; the queue is not selected yet, so they wait. An hour that starts
   within this window is *frozen*: Sync never rebuilds or clears it (a
   missing hour is still built), a reminder of it cannot be answered
   (`hour_started`), and a due reminder is only closed. Marking an item
   queued takes the plan lock, so it never races a build. This closes the
   queued boundary of Phase 8 §5: a rebuild never meets queued items.
3. **Lookahead.** The pusher keeps each active hour queue
   `rotation.lookahead_items` (2–3) items ahead of the playing one, in
   planned order. An item is marked `queued` before the push and set back to
   `planned` if the push fails, so a crash between the two never pushes an
   item twice. An item more than 30 s late, or whose planned end has passed,
   is never pushed (no catch-up, §7.1); it stays `planned` for Phase 10.
   Nothing is pushed into an hour's queue in its last 5 seconds (the switch
   flushes it; a request still resolving would survive the flush).
4. **Request URI.** `annotate:item_id="<id>",liq_cue_in="<s>",liq_cue_out="<s>",liq_fade_out="<s>":<media root>/<file_path>`.
   `liq_cue_out` and `liq_fade_out` are set only for a truncated item; the
   fade length is `liquidsoap.fade_out_ms` and ends at the out point
   (§8.1). The media root as Liquidsoap sees it is a new connection setting,
   `liquidsoap.media_root` (default `/media`, as in the compose files).
5. **Control client** (`internal/liquidsoap`). Telnet line protocol, one
   connection owned by the client, commands serialized by a mutex, a
   deadline per command, reconnect with backoff. Commands: push, queue,
   flush, `var.set`, `uptime` (heartbeat). The link state (connected,
   last success) is published as `liquidsoap.state` on change and kept for
   Phase 10. Telnet has no authentication; the port stays on the private
   network (§8.9).
6. **Callbacks** (`POST /api/playout/callback`, called by Liquidsoap only).
   The body is the journal line: `event` (`start`, `end`, `output`), `time`,
   `rid`, `item_id` (empty for emergency), `track_id` (emergency), `source`
   (`plan` or `emergency`), `elapsed_ms`, and for `output` `connected`.
   - `start` of a plan item: in one transaction the item is moved to
     `play_log` (`aired`, or `truncated` if the plan truncated it;
     `started_at` = callback time; `actual_duration_ms` = planned length
     until the end arrives) and deleted from `scheduled_items` (§4.1).
   - `end` of a plan item: `actual_duration_ms = elapsed_ms`; the status
     becomes `truncated` when the item was cut more than 1 s before its out
     point (a switch at the boundary or a skip). An `end` without a `start`
     (lost callback) moves the item with `started_at = time − elapsed`.
   - Emergency tracks are logged on `end` only (`source = emergency`, no plan
     fields).
   - Idempotent: a unique partial index on `play_log (scheduled_item_id)`;
     a repeated `start` or `end` changes nothing. An unknown `item_id` is
     logged at WARN and answered 204. Phase 10 reconciliation reuses the same
     function on journal lines.
   - `output` events update the Icecast output state (`icecast.state` event,
     §6.5); the outage detector uses it in Phase 10.
7. **Now playing.** The playout service keeps the current item in memory
   (one owner, mutex) and sends `playout.now_playing` on every `start`:
   item, track, artist, source, started_at, planned out point. The panel
   computes progress itself; no progress events.
8. **Music gain** (§8.5). `PUT /api/playout/music-gain` (`gain` 0–1) sets
   the live value (`var.set music_gain`). It is kept in memory, not in the
   database: on every new connection the backend sets the last value (at
   startup, the setting `liquidsoap.music_gain`), and a change of the
   setting is applied at once. Refused with `liquidsoap_unavailable` (503)
   when the control link is down.
9. **Emergency** (§8.2, §8.6). Behind the hour switch the script has
   `request.dynamic`, which asks `GET /api/playout/emergency/next` for a
   track of the `playout.emergency_playlist_id` playlist (random, equal
   weights, not the previous emergency track; separation and play rules are
   not applied — this is the last resort). 204 when there is no playlist or
   it is empty; then the safety source plays (silence or a tone). If the
   backend is down, the safety source plays. The hour queue takes over as
   soon as it has an item (`track_sensitive=false`, short fade).
10. **Resync on connect.** On every (re)connect the backend sets
    `hour_shift` and `music_gain`, then reads the queues of the hours it
    feeds. `hour_shift` is also set again whenever the station offset
    changes (a timezone change or DST). If the queue of an
    hour that is not playing is empty while the database holds `queued`
    items for it (Liquidsoap restarted), those items go back to `planned` and
    are pushed again.
11. **Ownership.** The pusher is one goroutine owned by `serve` (errgroup):
    it runs a pass every 5 seconds (each pass reads the settings from the
    database; the switch itself runs in Liquidsoap) and at once after every
    start callback. Its `uptime` ping is the heartbeat of the link. The control client has no
    goroutine of its own; it connects on use. No goroutine per callback or
    per item.

## 3. Packages

| Package | Contents |
|---------|----------|
| `internal/liquidsoap` | Telnet control client: `Push`, `Queue`, `Flush`, `SetVar`, `Ping`; link state |
| `internal/playout` | Service: pusher (`Run`), queue parity, request URIs, callbacks, now playing, music gain, emergency pick, resync |
| `internal/repository/playout` | pgx: items to push, mark queued/planned, move to `play_log`, end update, emergency tracks |
| `internal/rotation` | Sync and Answer respect the frozen window (decision 2) |
| `dev/liquidsoap/radio.liq` | Two hour queues and the switch, flush on leave, `item_id` and `source` in the reports, `output` events, `request.dynamic` emergency |

Migration `0006_play_log_playout.up.sql`: `play_log.ended_at` (null until
the end is recorded, so the first end wins) and the unique partial index
`uq_play_log_scheduled_item_id ON play_log (scheduled_item_id) WHERE
scheduled_item_id IS NOT NULL` (a repeated start is a no-op).

New settings: `playout.prepush_minutes` (default 5, 1–15) and
`liquidsoap.media_root` (default `/media`).

## 4. API

| Method and path | Purpose |
|-----------------|---------|
| `POST /api/playout/callback` | Liquidsoap track `start`/`end` and `output` events; 204 |
| `GET /api/playout/emergency/next` | Liquidsoap: next emergency track as a request URI (`text/plain`), or 204 |
| `GET /api/playout/status` | Control link, Icecast output, now playing, queued items per hour queue, last callback time |
| `PUT /api/playout/music-gain` | Body `gain` (0–1); sets the live music gain |

New problem code `liquidsoap_unavailable`. New events (named schemas):
`playout.now_playing`, `liquidsoap.state`, `icecast.state`.

## 5. Known limits (for later phases)

- Items that did not air (backend or Liquidsoap down, flushed at the
  boundary) stay `planned`/`queued` and keep counting for separation until
  Phase 10 marks them missed or removes them.
- A gap below `rotation.min_filler_gap_ms` before a fixed element is not
  held: the queue plays on, so the fixed element may start up to that gap
  before its window. The hour boundary stays exact (the switch).
- A short gap at the end of an hour plays the emergency source for those
  seconds.
- On resync, if the start callback of the last queued item is in flight
  while its queue reads empty, that item can be pushed and play twice. A
  new connection happens only after a failure, so this is rare.
- A reminder whose deadline falls inside the frozen window is only closed;
  the hour keeps its gap (as for a started hour in Phase 8).
- The boundary accuracy (≤ 1 s, §8.10) is checked by hand on the dev stack;
  there is no automated audio test. Measured 2026-10-14: the first item of
  the hour started 18 ms after the boundary.

## 6. Done when

Unit tests cover the control client (fake telnet server), queue parity with
offsets, URIs, the pusher (lookahead, pre-push, no catch-up, mark and revert,
resync), the callback mapping (start, end, lost start, cut item, emergency,
repeats), and the frozen window in rotation. Repository integration tests run
against PostgreSQL. API integration tests cover the callback flow from a
built hour to `play_log`, the status, the music gain refusal, and the
emergency endpoint. On the dev stack, a built hour plays through the
boundary and lands in `play_log`. `make lint`, `make test-integration`, and
`make openapi-lint` pass.
