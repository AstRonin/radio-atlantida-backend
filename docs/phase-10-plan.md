# Phase 10 Plan — Outages and Missed Items

Status: Done
Based on: `base.md`, `tech-design.md` (§4, §4.1, §6.4, §6.5, §7.1, §7.2, §8.3, §8.6, §9.2, §10.1), `backend-init-plan.md` §9 item 10 and §10.4 item 7, `phase-9-plan.md` §5
Last updated: 2026-10-14

## 1. Goal

The backend notices when the broadcast is interrupted, stores outage periods,
and records which plan items did not reach the air ("missed"). After the
backend itself was down, it rebuilds what aired from Liquidsoap's playout
journal. The manager sees aired, truncated, and missed items in one list,
and decides what to do with each missed item: reschedule it into a later
hour (start, end, or a fixed minute) or dismiss it.

Phase 9 left the hooks: the control link state, the last callback time, the
Icecast output state, and plan items that did not air are still in
`scheduled_items` and count for separation. This phase resolves them.

## 2. Decisions (2026-10-14)

1. **Signals** (§7.1). One detector goroutine evaluates three signals every
   second:
   - **Icecast**: the backend reads `/status-json.xsl` of the Icecast server
     (settings `stream.icecast_host`, `stream.icecast_port`) every
     `outage.icecast_check_interval_seconds` and looks for the mount
     (`stream.mount`). It fails when the request fails or the mount is
     missing.
   - **Liquidsoap callbacks**: the script sends a `heartbeat` callback every
     5 seconds (callback only, not written to the journal). The signal fails
     when no callback of any kind arrived within
     `outage.heartbeat_timeout_seconds`.
   - **Control link**: the signal fails when the link has had no successful
     command within the heartbeat timeout **and plan items are due** (an
     item should be playing now, or starts within the pre-push window). With
     the link down, Liquidsoap keeps streaming (queue leftovers, then the
     emergency playlist, then the safety tone), so listeners hear audio, but
     the planned items never air. Decided 2026-10-14 to treat this as an
     outage, so these items become missed and can be rescheduled. This goes
     beyond §7.1 ("only if the callbacks also stop"); §7.1 is updated.
2. **Open and close.** A signal counts as down after
   `outage.failed_checks_to_open` consecutive failed checks (Icecast) or
   once its timeout passed (callbacks, control link). The first signal down
   opens the outage: `started_at` is the time of its first failed check, the
   last callback, or the last successful command; `detected_by` and
   `component` come from that signal (callbacks: `liquidsoap_callback` /
   `liquidsoap`; control: `liquidsoap_control` / `liquidsoap`; Icecast:
   `icecast` / `icecast`); `reason` is a short text. There is at most one
   open outage (`uq_broadcast_outages_open`). It closes when every signal is
   up again; `ended_at` is that moment. Events:
   `broadcast_outage.started` on open; `broadcast_outage.ended` after the
   missed items are marked, with their count.
3. **What counts as aired: the journal decides** (§7.1, decision 12 of
   §11). An item Liquidsoap started counts as aired (or truncated), also
   when Icecast was down at that moment (decided 2026-10-14). Only items
   that never started can be missed.
4. **Marking** (`missed_items` operation; its events are
   `missed_items.completed` / `missed_items.failed`, the `<kind>.completed`
   convention of §6.5). When an outage closes:
   1. The journal is replayed over the outage window (decision 6), so starts
      whose callback was lost are recorded first.
   2. Under the plan lock, every `planned` or `queued` item whose planned
      window overlaps the outage, that starts before `ended_at` and more
      than 30 s before now (the pusher's no-catch-up limit), moves to
      `play_log` with `status = missed`, `resolution = open`, and the
      outage's ID, and is deleted from `scheduled_items` (§4.1). Queued
      items are included: a stalled or restarted Liquidsoap lost them.
   3. `broadcast_outages.reconciled_at` is set. A closed outage with
      `reconciled_at` null (the marking failed, or the backend stopped
      first) is marked again by the detector's sweep and at startup.
   Pool rows of missed filler items stay used: the track was planned.
5. **Unaired items without an outage are dropped** (decided 2026-10-14).
   A queued leftover flushed at the hour switch after drift, or an item
   skipped as more than 30 s late while the stream was fine, is treated like
   a track removed by overflow (§7): deleted from the plan, not missed, no
   separation count, its pool row released. The manager builds the clocks
   and knows that this can happen. A sweep every 30 seconds drops `planned`
   and `queued` items that started more than 2 minutes ago and overlap no
   outage, after replaying the journal over their window (a start whose
   callback was lost is recorded, not dropped). Items that overlap an open
   or an unmarked outage wait for the marking. Each sweep that drops items
   logs one WARN with the count.
6. **Journal replay** (`internal/outage`, reconcile). Liquidsoap writes
   `journal-YYYY-MM-DD.jsonl` (UTC date) under `PLAYOUT_JOURNAL_ROOT`, a new
   required environment variable (a host path, like `MEDIA_ROOT`; the
   backend mounts it read-only). A replay reads the files of the dates that
   cover a window, in line order, and passes `start` and `end` lines of the
   window to playout's callback logic without its live effects (now
   playing, last callback). The callback logic is idempotent, so no cursor
   is kept: replaying a window twice changes nothing. Emergency tracks
   become idempotent as well: a unique partial index on `play_log
   (track_id, started_at) WHERE source = 'emergency'`. Lines that cannot be
   parsed are counted and logged once per replay (§10.1).
7. **Backend downtime** (`playout_reconcile` operation, at startup). The
   detector writes `backend_heartbeat.alive_at` every 30 seconds. At
   startup the backend replays the journal from the older of `alive_at`
   and the oldest unaired item, then:
   - if it was down longer than the heartbeat timeout and items planned in
     the downtime did not air, it records a closed outage
     (`detected_by = backend`, `component = backend`, from `alive_at` to the
     startup) and marks them missed (§9.2 "recorded as an outage with
     component backend if the stream is affected");
   - closed outages that are not marked yet are marked;
   - an outage still open (the backend stopped during it) stays open; the
     detector closes it when the signals are up.
   The detector starts evaluating only after this operation finished.
8. **Played-tracks list** (`GET /api/play-history`). Aired, truncated, and
   missed rows of `play_log` in one list, newest first by
   `COALESCE(started_at, planned_start_utc)`, keyset paginated (`limit`,
   opaque `cursor`); filters `from`, `to`, `status`. Missed rows carry their
   outage (component, reason) and their resolution. Expression index
   `ix_play_log_sort_time`.
9. **Missed items** (`GET /api/missed-items?resolution=`): open by default,
   at most 500, planned time descending. **Dismiss** is synchronous:
   `resolution = dismissed`, `resolved_at = now`; refused with 409
   `missed_item_resolved` when it is not open. The row then follows the
   10-day retention from `resolved_at` (already in `internal/retention`).
10. **Reschedule** (`reschedule` operation, §7.1, §7.2). Body:
    `target_hour_start_utc`, `placement` (`start`, `end`, `fixed`),
    `fixed_minute`, `fixed_tolerance_ms` (default the setting
    `rotation.default_fixed_tolerance_ms`).
    - Checked synchronously (422 per field, 409 when the item is not open):
      the item is open; the target is a station hour start, after the
      frozen window (`hour_started` otherwise) and within the plan horizon;
      the grid assigns a clock to it; the track still has a duration; the
      clock's layout with the one-time element is valid (a fixed window
      inside the hour, start/end elements that fit). The refusal is
      addressed to the request fields.
    - The one-time element is a row of the new table `hour_inserts`
      (`hour_start_utc`, `track_id`, `missed_id` → `play_log`, placement,
      fixed minute and tolerance). The build of that hour adds it to the
      clock's elements (`clocks.WithInsert`: start elements after the
      clock's start elements, end elements after its end elements, a fixed
      element in minute order and replacing an overlapping fixed element,
      §7.2), with its track pinned, nominal duration equal to the track,
      tolerance 0. The segment keeps the filler holder it had without the
      insert. A replaced fixed element is simply not planned in that hour
      (the build warns `element_replaced`); no `replaced` row is written,
      because planned items of a rebuilt hour are deleted anyway.
    - The operation, in one transaction under the plan lock: store the
      insert, build the hour (forced), and require that the pinned item is
      placed whole: eligible by separation and play rules at its start, not
      truncated, not removed. Otherwise it fails (`reschedule.failed` with
      the reason) and nothing changes. On success the missed row gets
      `resolution = rescheduled`, `resolved_at`, and `resolved_item_id` (the
      new plan item, source `reschedule`, `reschedule_of_id` set).
    - The insert stays with the hour: every later rebuild (clock or grid
      change, reminder answer) places it again. If a later rebuild cannot
      place it, the insert is deleted, the missed row is open again, and the
      build warns `reschedule_dropped`.
11. **Outages list** (`GET /api/outages?from=&to=`): outages that overlap
    the range (at most 7 days), newest first, with the number of items
    marked missed. Open outages are shown with `ended_at` absent.
12. **Ownership.** The detector is one goroutine owned by `serve`
    (errgroup); it runs the startup reconcile, then a 1-second evaluation
    loop with the Icecast check, the heartbeat write and the sweep on their
    own intervals. Marking runs as an operation in the detector goroutine
    (`Tracker.Do`), so nothing else touches its state and detection simply
    pauses for the few milliseconds it takes. A reschedule runs on the
    rotation planner goroutine, which already serializes plan writes. No
    goroutine per signal or per item.

## 3. Packages

| Package | Contents |
|---------|----------|
| `internal/outage` | Detector (signals, open/close, heartbeat, sweep), Icecast mount check (`net/http`), journal replay, marking, startup reconcile, missed items (list, dismiss), play history, outages list |
| `internal/repository/outage` | pgx: outages, heartbeat, mark missed, drop stale items, due items, play history (keyset), missed items, dismiss |
| `internal/playout` | `heartbeat` event, `Replay` (callback logic without live effects), `LastCallback`, idempotent emergency log |
| `internal/rotation` | `Reschedule`: hour inserts in the build (pinned tracks), refusal and drop; source `reschedule` |
| `internal/clocks` | `WithInsert`: the clock's elements with one one-time element (§7.2 placement and replacement) |
| `internal/config` | `PLAYOUT_JOURNAL_ROOT` |
| `dev/liquidsoap/radio.liq` | `heartbeat` callback every 5 s; journal files older than 10 days deleted |

Migration `0007_outages.up.sql`: `backend_heartbeat` (one row), `hour_inserts`,
`uq_play_log_emergency`, `ix_play_log_sort_time`.

## 4. API

| Method and path | Purpose |
|-----------------|---------|
| `GET /api/play-history?from=&to=&status=&limit=&cursor=` | Played-tracks list (aired, truncated, missed), newest first |
| `GET /api/missed-items?resolution=` | Missed items (default `open`) |
| `POST /api/missed-items/{id}/dismiss` | Dismiss; synchronous |
| `POST /api/missed-items/{id}/reschedule` | Reschedule; `accepted` with an operation ID (`reschedule`) |
| `GET /api/outages?from=&to=` | Outage periods |
| `POST /api/playout/callback` | Gains the `heartbeat` event |

New problem codes: `missed_item_not_found`, `missed_item_resolved`. New
field rule: none (`does_not_fit`, `blocked`, `out_of_range` exist). New events
(named schemas): `broadcast_outage.started`, `broadcast_outage.ended`. New
operation kinds: `missed_items`, `playout_reconcile`, `reschedule`.

## 5. Known limits (for later phases)

- Items in the last 30 s before an outage ends are not marked missed (they
  might still be pushed); if they do not air, the sweep drops them.
- The Icecast check sees the mount from the backend's network. A problem
  only between Icecast and the listeners is not detected.
- A replaced fixed element of a rescheduled hour is not written as a
  `replaced` row; the build warning names it.
- Journal files are deleted by the Liquidsoap script after 10 days (the
  backend mounts the directory read-only), independent of
  `retention.play_log_days`.

- When the grid stops assigning an hour that holds a rescheduled item, the
  insert is deleted and the missed item is open again (logged at WARN);
  the same happens when the sweep drops a rescheduled item that did not air.
- A refused reschedule names its reason in `reschedule.failed`; the panel
  shows the operation's error message.
- The production Liquidsoap script (`radio-atlantida-deploy/config/`) must
  get the `heartbeat` callback and the journal cleanup in the same release
  (§10.5 of the init plan). Without heartbeats the detector opens a
  `liquidsoap_callback` outage whenever no track starts or ends for 10 s.

## 6. Done when

Unit tests cover the detector (each signal, thresholds, open/close order,
started_at, one open outage, items-due rule), the Icecast status parsing,
journal replay (dates, order, window, bad lines), marking and the sweep
(fake stores), the startup reconcile (downtime with and without unaired
items), `clocks.WithInsert`, and the build with pinned inserts (placed,
refused, replaced, dropped). Repository integration tests run against
PostgreSQL. API integration tests cover the play history (filters,
keyset), missed items, dismiss, reschedule (accepted, refused, built), and
outages. On the dev stack, stopping Icecast opens and closes an outage and
the items of that time show as missed. `make lint`, `make test-integration`,
and `make openapi-lint` pass.

Checked on the dev stack 2026-10-14: with the backend running, stopping
Icecast opened an `icecast` outage about 4 s later, and starting it again
closed the outage and ran the `missed_items` operation; `playout_reconcile`
ran at startup. The dev database had no plan, so no item was missed there;
marking, dropping, and rescheduling are covered by the integration tests.
