# Phase 16 Plan — Broadcast Status (radio-atlantida-backend)

Status: Implemented
Based on: `tech-design.md` (§6.5 events, §7.1 outages, §8.3 playout callbacks), `docs/phase-10-plan.md` (outage detector), `docs/phase-9-plan.md` (control link), frontend `docs/f14-broadcast-status-plan.md`
Last updated: 2026-10-15

## 1. Goal

The panel shows on every page whether the stream reaches Icecast, and for how long the current state has lasted. The backend derives one state (`connected`, `reconnecting`, `unreachable`, `unknown`) and stores the moment of its last change in the database, so the duration survives a restart of the backend.

Liquidsoap keeps streaming through a backend restart, and it reports the Icecast output only when that output changes. Without a stored `since`, the timer would start from zero after every restart.

## 2. Decisions (2026-10-15)

1. **`since` is the moment of the last state change.** Any drop resets the timer. The timer measures continuous broadcasting, not total uptime.
2. **`reconnecting`** when Liquidsoap reports the Icecast output disconnected and no outage is open. Liquidsoap retries on its own, so this state covers the time before the detector decides the stream is down.
3. **`unreachable`** while any broadcast outage is open (component `icecast`, `liquidsoap`, `backend` or `network`). The detector already opens an Icecast outage after `outage.failed_checks_to_open` failed mount checks (default 2, about 10 s with the default interval). The label is "Stream down" for every component, because listeners hear nothing in each case.
4. **`connected`** when the output is connected and no outage is open.
5. **`unknown`** after the backend starts until the first evidence arrives: an output callback, or a mount check that finds the source. It also applies when Liquidsoap's heartbeat is silent beyond `outage.heartbeat_timeout_seconds`. Unknown is never stored.
6. **Storage: singleton table `broadcast_state`.** One row (`id = 1`), written only when the derived state changes, in one transaction with the change. No history table: the outage table already records every interruption.
7. **Two events, not one.** `icecast.state` stays the raw Liquidsoap report (connected, disconnected). The new `broadcast.state` carries the derived state, `since`, and the reason. The panel reads only `broadcast.state`.
8. **Restart rule.** On startup the row is loaded. The first mount check that finds the source keeps a stored `connected` state and its `since`. A stored state that the check contradicts is replaced by the check's result, with `since` set to the check time.
9. **Timestamps** are UTC, as everywhere else (tech-design §10).

## 3. Data

Migration `0009_broadcast_state.up.sql` (expand only, one row):

- `id smallint primary key check (id = 1)`
- `state text not null check (state in ('connected', 'reconnecting', 'unreachable'))`
- `since timestamptz not null`
- `reason text` (nullable; the open outage's reason or component, for the tooltip)
- `updated_at timestamptz not null`

The row is inserted by the first transition, so an empty table means `unknown` on startup.

## 4. Wire contract

- Event `broadcast.state`: `data.state` (enum `connected`, `reconnecting`, `unreachable`, `unknown`), `data.since` (nullable for `unknown`), `data.reason` (optional), `data.outage_id` (optional), `data.mount_ok` (nullable: last mount check result), `data.last_check_at` (nullable). Sent only when the derived state changes. The panel reads `GET /api/broadcast/status` when its event connection opens, so it does not depend on events it missed.
- `GET /api/broadcast/status`: the same fields plus the server time (`time`). Used on connect, like `GET /api/outages`.
- Update `tech-design.md` §6.5 (events list: `icecast.state` note that it is the raw report; add `broadcast.state`) and the endpoint table in §6.

## 5. Package layout

- `internal/repository/broadcast`: `Get(ctx)`, `Save(ctx, tx, state)` (upsert of row 1).
- `internal/broadcast`: the `Tracker`. It holds the current state and the last mount check, and exposes:
  - `OnOutput(connected bool, at time.Time)`: from the playout callback (`setOutput`).
  - `OnOutageOpened(o)` and `OnOutageClosed(at time.Time)`: from the detector, inside its open and close transactions.
  - `OnMountCheck(ok bool, at time.Time)`: from `checkIcecast`.
  - `OnHeartbeat(silent bool)`: from the detector's heartbeat evaluation, to derive `unknown`.
  It computes the state, persists it when it changed, and publishes `broadcast.state`.
- `internal/httpapi/handlers/broadcast.go` and `internal/httpapi/dto/broadcast.go`: the status endpoint.
- `internal/ws/events.go`: the event type and payload.

The Tracker has no goroutine of its own. Callers hold their own locks; the Tracker has a mutex, since the playout callback and the detector run in different goroutines.

## 6. Tasks

1. Migration `0009` and its integration test (`migrations_integration_test.go`: the check constraint rejects `id = 2` and unknown states).
2. Repository `broadcast` with an integration test (upsert, read of an empty table).
3. Tracker with table-driven unit tests for every transition in §2: output connected, output lost with and without an open outage, outage opened and closed, mount check contradicting the stored state, heartbeat silence, restart with the source present.
4. Hook the Tracker into `setOutput`, the detector's open and close paths, `checkIcecast`, and the heartbeat evaluation. Each hook runs after the existing logic, so the outage behaviour does not change.
5. Persist in the same transaction as the outage row when a transition comes from an outage.
6. Event `broadcast.state` and the status endpoint, with handler tests.
7. Regenerate `api/openapi.json` and copy it to the frontend's `api/openapi.json` (the frontend's `scripts/sync-api.mjs` regenerates the client; see frontend F14).
8. Update `tech-design.md` §6 and §6.5 and §7.1 (derived states, the timer rule).
9. `make test` and the integration tests against the dev database.

## 7. Done when

- Disconnecting the Icecast output (stop the Icecast container) shows `reconnecting` at once and `unreachable` after about 10 s; restarting it returns `connected`. The timer restarts at each change.
- Restarting the backend while the stream plays keeps `since` from before the restart, and the state becomes `connected` after the first mount check (within `icecast_check_interval_seconds`).
- Restarting the backend while the stream is down shows `unreachable` once the first check fails, with `since` at that check, not at the restart.
- Writes happen only on change: a steady stream produces no writes to `broadcast_state` (checked by counting statements in the integration test).
- All tests pass; the OpenAPI snapshot matches the code.

## 8. Known limits

- Liquidsoap does not report connection attempts. `reconnecting` means "disconnected, no outage yet". The panel cannot tell a retry that is failing from one that has not run yet; the outage tells the difference after about 10 s.
- After a backend restart with the stream down, the true start of the down period is unknown. `since` is the first failed check.
- `since` uses the backend's UTC clock. A browser with a wrong clock is handled in the frontend (F14).
