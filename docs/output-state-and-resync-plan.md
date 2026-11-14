# Plan — Output State in the Heartbeat (Contract 2) and Plan Alignment After a Liquidsoap Restart (radio-atlantida-backend)

Status: Implemented (2026-11-14). P2 `75c0250` (+ `731ff6e`, an integration test); P1 `bccef39` (script contract 2, backend) + `3b80975` (liqsmoke); "Docs: output state and resync plan status". The dev-stack checks of §5 and the deploy follow-up (§6) are still to do. Deviations: §8.
Based on: `docs/tech-design.md` v7-27 (§6.5, §7.1, §8.1, §8.3, §8.10, §11 items 41–42), `docs/phase-9-plan.md` §2 (pusher), `docs/phase-16-plan.md` §2 (broadcast state), `docs/safety-on-air-state-plan.md` (heartbeat field), `docs/deploy-prereqs-plan.md` A2 (contract header), frontend `docs/output-unknown-plan.md`, `radio-atlantida-docs/deploy/docs/dry-run.md`
Last updated: 2026-11-14 (implemented)

## 1. Goal

Two defects found after the first OrbStack dry run:

- **P1. The Icecast output state goes stale.** `playout.Service.output` is set only by the script's `output` callbacks (`dev/liquidsoap/radio.liq:590-598`, `report_output` at `:163-171`), in `setOutput` (`internal/playout/callbacks.go:257-279`), and is never reset. After a backend restart it is absent until the output changes; after Liquidsoap stops it stays `connected`; in a cut-over where the mount still holds the old source and our output gets 403, no callback ever says "disconnected", so the Live page shows a stale state and the derived broadcast state (`internal/broadcast/tracker.go`) falls back to the mount check, which is OK because the old source holds the mount, so the header says `connected`.
- **P2. The plan drifts early after a Liquidsoap restart.** `resync` (`internal/playout/pusher.go:151-181`) returns the items of an emptied queue to planned, and `fill` (`:233-276`) pushes the first one at once. The hour queue is empty, so Liquidsoap plays it immediately, and the whole rest of the hour moves early by (planned start of that item − now). Example: Liquidsoap dies at xx:01:00 and is back at xx:10:00; the news planned at 30:00 airs at 28:10, the queue runs dry at 58:10 and the emergency playlist fills to the hour's end. With a long track on air at the restart the shift reaches about 8 minutes. It happens on the first start mid-hour (install, cut-over), after every crash, and at every maintenance restart (P1's own rollout is one).

After this plan, the output state corrects itself every heartbeat and becomes `unknown` when Liquidsoap is silent, and an item pushed after a restart starts at its planned second.

## 2. Decisions (2026-11-14)

1. **P1 (a): the heartbeat carries the output state.** The 5 s heartbeat (`radio.liq:325-342`) gains `output_connected` (bool) and `output_since` (RFC 3339 UTC time of the last change, the same `utc_now()` format as `time`). The `output` callbacks stay: they report a change at once; the heartbeat repairs anything missed (a backend restart, a lost callback, a source that never connected).
2. **Contract 2.** The callback payload changes, and the backend refuses unknown body fields (Huma rejects properties not in `dto.PlayoutCallbackBody` with 422), so an older backend would refuse every new heartbeat and the outage detector would lose its heartbeat. The header becomes `# radio-atlantida-contract: 2`; `# min-backend: 1.0.0` stays, because no release exists yet and `v1.0.0` ships with contract 2.
3. **The new fields are optional in the DTO.** A heartbeat without them changes nothing (as `safety_on_air`). This keeps the rollout order safe: a backend with this change runs with a contract 1 script (`radioctl update` replaces the backend first; Liquidsoap gets the new script only at the next maintenance).
4. **P1 (c): silence resets to `unknown`.** When no heartbeat arrives for longer than `outage.heartbeat_timeout_seconds` (setting, default 10 s, range 2–300; already `Settings.HeartbeatTimeout`, used by `safetyExpire`), the output state becomes `unknown` and `icecast.state` is sent with `state: "unknown"`. The check runs at the start of every pass (every 5 s, before the link check), next to `safetyExpire`, so the reset comes 10–15 s after the last heartbeat with the default. No new timeout.
5. **`unknown` at backend start.** Until the first heartbeat or `output` callback, the state is unknown: `GET /api/playout/status` leaves `output` absent (as now; the description says "unknown"), no event is sent. After a silence reset `output` is `{state: "unknown", since: <reset time>}`. The frontend treats both the same.
6. **The broadcast state gets the heartbeat too.** Every heartbeat with the fields goes on to `OutputObserver.OnOutput` (as every `output` callback already does), with the heartbeat's `time`. So after a backend restart the header's state comes from Liquidsoap within 5 s instead of the mount check, and the cut-over case shows `reconnecting`. `Tracker.OnHeartbeat` already forgets the output when the heartbeat is silent; nothing changes there. The outage detector does not read the output state and is unchanged.
7. **P2, option A: hold the first item after a resync.** When a resync finds the queue of the hour on air empty, the pusher holds that hour's first pushable item until its planned start − `holdLead` (500 ms), then fills as usual. The gap before it (at most one track) is covered by the emergency playlist, else the safety tone (existing fallback; `automation_main` is `track_sensitive=false`, so the plan item takes over at the moment it is pushed). No contract change.
8. **P2, option B is deferred.** Resuming the interrupted track mid-way (cue in at the elapsed position, short fade in) would remove the gap but needs a contract change (a new annotation or command), protection against playing the same item twice (the journal already has its start), and new play-log semantics. Not in this plan.
9. **Order.** P2 lands first and alone (no contract change, no API change). P1 follows; it changes the script, the DTO, the WebSocket event, and the OpenAPI snapshot, so the frontend plan (`output-unknown-plan.md`) follows it.

## 3. Steps — P2 (resync alignment)

**Status (2026-11-14): done** (75c0250, 731ff6e). Deviations: §8 items 1–4 and 10.

### P2.1 Pusher state (`internal/playout/pusher.go:41-58`)

Add to `pusherState`:

- `align map[time.Time]bool`: hours (UTC hour starts) whose Liquidsoap queue was empty at the last resync; their first item is held. Created in `Run` (and in `newHarness` through the same constructor path).
- `holdUntil time.Time`: when the held item is due (planned start − `holdLead`); zero when nothing is held. Recomputed by every pass.

New constant next to `pushCutoff`/`lateLimit` (`:16-39`): `holdLead = 500 * time.Millisecond` ("a held item is pushed this long before its planned start: the request must resolve before it plays").

### P2.2 Resync marks the hour on air (`resync`, `:151-181`)

- At the start: `clear(s.push.align)`, `s.push.holdUntil = time.Time{}`.
- In the loop, for `hours[0]` (the hour on air) only, when `len(rids) == 0`: `s.push.align[h] = true`, whether or not `Unqueue` returned items (a first start has nothing queued but the same problem). The next hour (pre-push) is not marked: its queue does not play before the boundary, and the hour switch aligns it (§8.10).
- Update the doc comment: an emptied queue of the hour on air holds its first item until its planned start, so the hour stays on its plan.

### P2.3 Fill holds the first item (`fill`, `:233-276`)

After `queued, after` is computed and before the push loop:

- If `s.push.align[h]` and `queued == 0`: find the first item of `items[after:]` that passes the existing late rules (`it.end().After(now)` and not `it.Start.Before(now.Add(-lateLimit))`). If there is none, return. Let `due := it.Start.Add(-holdLead)`. If `now.Before(due)`: set `s.push.holdUntil = due` (the earliest, if both hours ever held) and return without pushing. Otherwise `delete(s.push.align, h)` and continue to the loop, which pushes it and the rest of the lookahead in the same pass.
- If `queued > 0` (an item was pushed meanwhile), `delete(s.push.align, h)`.

In `pass` (`:99-142`): set `s.push.holdUntil = time.Time{}` before the `fill` loop, and drop `align` entries for hours before `cur` (hour boundary, P2 edge case 2).

Keep it small (about 15–20 lines): a helper `holdFirst(items, after, now) (due time.Time, hold bool)` is fine.

### P2.4 Timer in `Run` (`:62-78`)

After each pass, if `s.push.holdUntil` is set, start a `time.Timer` for `max(0, holdUntil − s.now())` and add `case <-timer.C:` to the `select`; stop it after the `select`. Without it the push would wait for the next 5 s tick. A start callback or the ticker may run a pass earlier; it finds `now` still before `due` and holds again.

### P2.5 Edge cases (documented in the code comments and in tests)

1. **The held item is already late** (planned start ≤ now, within `lateLimit` = 30 s): `due` is in the past, it is pushed at once and airs up to 30 s late, as today (§7.1: no catch-up; an item more than 30 s late is skipped and the next one is the held item).
2. **Hour boundary during the hold.** The held item belongs to the hour on air, so `due` is before that hour's end; `fill` still stops at `pushCutoff`. The next hour is filled normally inside the pre-push window and starts at the boundary by the script's switch. An `align` entry of an hour that has ended is dropped by `pass`.
3. **Live voice during the hold.** The harbor input is mixed over the music (ducking, §8.4–8.5); it does not replace the hour queue, so the hold is unaffected. A music gain of 0 does not change the timing either.
4. **A new resync during the hold** (another restart, or a reconnect of the control link): `resync` clears `align` and `holdUntil` and decides again from the queue it finds.
5. **The held item is an insert** (a template or one-time insert, a rescheduled missed item): it starts at its planned second; this is the case the fix is for (the news at 30:00).
6. **The plan changes during the hold** (a rebuild of a future part of the hour, a reschedule into it): every pass reads the items again, so the held item and `due` follow the current plan.
7. **A reconnect without a Liquidsoap restart, queue empty, a plan track still playing**: the next item is pushed at its planned start − 500 ms instead of at once; the playing track ends at that moment anyway, so nothing changes on air.
8. **The emergency track cut by the held item**: cut at once without a fade (existing behaviour whenever the plan resumes; its end is logged with the time it played).

### P2.6 Tests (`internal/playout/playout_test.go`)

- `TestPassSkipsLateItems` (`:430-441`): the first case would now hold item 3 (planned 6:00, now 3:40) because the first pass is a resync with empty queues. Set `h.svc.push.gen = 1` (no resync) in both cases, so the test stays about the late rules only.
- `TestResyncAfterLiquidsoapRestart` (`:472-490`): stays valid (item 1 is due at the restart, so it is pushed at once with item 2); add the assertion `h.svc.push.holdUntil.IsZero()` and a second part: a restart when the next item is in the future (now = 3:40 into an item planned 0:00–3:00 that has ended, item 2 planned 3:00 started, item 3 at 6:00): nothing pushed, `holdUntil` = 5:59.5; a pass at 5:59.4 pushes nothing; a pass at 5:59.5 pushes items 3 and 4.
- New `TestResyncHoldEdgeCases` (table): held item late by 10 s (pushed at once); held item late by 40 s (skipped, the next one held); next hour inside the pre-push window is pushed during the hold; a new generation during the hold recomputes; a rebuilt hour moves `due`; a pass after the hour boundary drops the old `align` entry.
- New regression `TestResyncKeepsHourAligned` (the drift simulation): a fake Liquidsoap in the test plays pushed requests back to back (a request starts at `max(push time, end of the previous one)`, an empty queue falls back to "emergency"), the clock advances in 100 ms steps, and a pass runs every 5 s and when `holdUntil` is reached. Hour of 3-minute items with a one-time insert at 30:00; the link drops at 01:00 and comes back with empty queues at 10:00. Assert: the insert starts within 1 s of 30:00 (before the fix it started at 28:10), and the last item ends by 60:00 with no emergency fill after 10:00 + one track. Also run it with a 10-minute track on air at the restart (before the fix the shift is about 8 minutes).
- `go test -race ./internal/playout/...`.

## 4. Steps — P1 (output state, contract 2)

**Status (2026-11-14): done** (bccef39; liqsmoke 3b80975). Deviations: §8 items 5–9.

### P1.1 Script (`dev/liquidsoap/radio.liq`)

- Header `:1`: `# radio-atlantida-contract: 2` (`:2` `min-backend: 1.0.0` unchanged). Add one line to the contract comment (`:31-37`): "Contract 2: the heartbeat reports the output state (output_connected, output_since)."
- Near `report_output` (`:162-171`): two refs, `output_up = ref(false)` and `output_changed = ref(utc_now())` (the script start until the first connect). `report_output(connected)` takes `t = utc_now()`, sets `output_up := connected`, `output_changed := t`, and writes `time=t` in its line, so the callback and the heartbeat carry the same moment.
- Heartbeat (`:325-342`): add `output_connected=output_up()` and `output_since=output_changed()`. Update its comment (`:325-329`) and the file header item (`:25`).
- An output switch (`output.set`) shuts the old output down (its `on_disconnect` reports false) and the new one reports true when it connects; the refs are global, so the heartbeat follows the last report. Before the first connect the heartbeat says `false` since the script started: true, the stream is not on Icecast yet.

### P1.2 Callback DTO (`internal/httpapi/dto/playout.go:8-22`)

- `OutputConnected *bool \`json:"output_connected,omitempty" doc:"heartbeat: the Icecast output is connected (contract 2). Absent: unknown, nothing changes."\``
- `OutputSince *time.Time \`json:"output_since,omitempty" doc:"heartbeat: when the output last changed state (UTC). Absent: the heartbeat time."\``
- `PlayoutOutput.State` (`:49-52`): `enum:"connected,disconnected,unknown"`; `PlayoutStatus.Output` doc (`:83`): "Absent until Liquidsoap reports it after the backend started (unknown)."

### P1.3 Handler (`internal/httpapi/handlers/playout.go`)

- `callbackEvent` (`:145-151`): map `OutputConnected` and `OutputSince` (`.UTC()`) to new `playout.Event` fields.
- `playoutStatusDTO` (`:163-180`): map the new `Output.State` (P1.4) instead of the `Connected` bool.

### P1.4 Playout (`internal/playout`)

- `Event` (`callbacks.go:31-48`): `OutputConnected *bool`, `OutputSince *time.Time` (heartbeat).
- `Output` (`playout.go:119-123`): replace `Connected bool` with `State string` (`ws.IcecastConnected`, `ws.IcecastDisconnected`, `ws.IcecastUnknown`) and keep `Since`. Callers: `setOutput`, `Status` (`:297-328`), the handler, tests.
- `Callback` (`callbacks.go:53-81`), heartbeat case: after `safetyHeartbeat`, if `e.OutputConnected != nil`, call `recordOutput(ctx, connected, since, at)` (not `applyOutput`, which is the `output.set` push in `output.go`) with `since = *e.OutputSince` or `e.Time` when absent, and `at = e.Time` for the broadcast tracker. Also record the heartbeat time for P1.5 (`s.outputBeat = at`).
- `setOutput` (`callbacks.go:257-279`) becomes `recordOutput`, used by both the `output` callback (`since = e.Time`) and the heartbeat. It changes the state and publishes `icecast.state` when the state differs or `since` differs (a flap between two heartbeats that lost its callbacks); the event's `time` is `since`, which the panel shows. `OnOutput` is called on every report, as now. Logging (`Warn` on loss, `Info` on connect) only on a state change, so a heartbeat every 5 s does not log.
- New `outputExpire(now, timeout)` in `callbacks.go` (or `safety.go`, renamed `heartbeat.go` if the implementer prefers one file for both heartbeat states), called in `pass` next to `safetyExpire` (`pusher.go:106`): when `outputBeat` is set and `now − outputBeat > timeout`, set the state to unknown with `Since = now`, clear `outputBeat`, publish `icecast.state` with `state: "unknown"`, log `Info` "icecast output state unknown: no heartbeat from liquidsoap". Nothing when the state is already unknown or absent. An `output` callback sets `outputBeat` too, so a script without the heartbeat fields still expires.
- `ws` (`internal/ws/events.go:231-249`): `IcecastUnknown = "unknown"`; `IcecastStateData.State` `enum:"connected,disconnected,unknown"`; doc: "unknown: no heartbeat from Liquidsoap for longer than outage.heartbeat_timeout_seconds."

### P1.5 OpenAPI and docs

- `make openapi` → `api/openapi.json` (`IcecastStateData`, `PlayoutOutput`, `PlayoutCallbackBody`); `make openapi-lint`.
- `CLAUDE.md` dev stack notes: the heartbeat line ("It also POSTs a `heartbeat` every 5 s") gains "with the safety tone and the output state (contract 2)". The contract rule itself is unchanged.

### P1.6 Smoke test (`dev/liqsmoke/main.go`)

- The stub's callback handler (`:136-145`) decodes every callback into `dto.PlayoutCallbackBody` with `DisallowUnknownFields` and records a decode failure; the run fails on any. This catches every future drift between the script and the backend's strict DTO.
- Record heartbeats. After the first output connects: a heartbeat arrives with `output_connected: true` and `output_since` equal to the `time` of the `output` connected callback. After `docker stop icecast` (or the existing step that leaves the output unconnected, whichever is simpler in this file): a heartbeat with `output_connected: false`; after `docker start icecast` a heartbeat with `true` again. Print `pass("heartbeat carries the output state")`.
- `liquidsoap --check` still runs in CI as before.

### P1.7 Tests

- `internal/playout`: extend `TestOutputCallback` (`playout_test.go:599-614`) for `State`; new table test `TestHeartbeatOutput`: a heartbeat with `true` sets connected with `output_since` as `Since` and sends one event; repeated identical heartbeats send nothing but reach `OnOutput` every time; a heartbeat with `false` after `true` sends `disconnected`; same state with a newer `since` sends one event; a heartbeat without the fields changes nothing; after a backend start (no report) the first heartbeat sets the state.
- `TestOutputExpire` (like `TestSafetyExpire`, `safety_test.go:100-171`): silent past the timeout → `unknown` and one event; exactly the timeout → unchanged; link down does not stop the reset; nothing before the first heartbeat; a heartbeat after the reset restores the state and sends it.
- `internal/httpapi/playout_test.go`: extend `TestPlayoutHeartbeatSafetyField` (`:97-…`) or add `TestPlayoutHeartbeatOutputFields`: contract 2 body accepted and mapped; contract 1 body (no fields) accepted with nil fields; a body with an unknown field (for example `"output_state":"x"`) is 422 (the strict body that makes this a contract change). `TestPlayoutOutput`/status test: `output.state` `unknown`.
- `internal/broadcast` is unchanged; one playout test asserts that a heartbeat after a backend start reaches `OnOutput` (the fake `outputs` already records reports).

## 5. Acceptance

- P2: `TestResyncKeepsHourAligned` passes and fails on the current `pusher.go`; on the dev stack, restarting Liquidsoap mid-hour (`docker compose restart liquidsoap`) makes the next plan item start within 1 s of its planned start (journal `start` time vs `planned_start_utc`), the emergency playlist covers the gap, and the hour ends on its plan.
- P1: on the dev stack, the Live page shows `unknown` 10–15 s after `docker compose stop liquidsoap`; after a backend restart with Liquidsoap running the state appears within 5 s with the true `since`; with the Icecast mount held by another source (a second source client on the same mount) the state is `disconnected` and the header `reconnecting`.
- `make test`, `make lint`, `make openapi-lint` pass; `make test-integration` has no failure beyond the known ones; `make liquidsoap-smoke` (the CI job `liquidsoap`) passes with the new checks.
- `dev/liquidsoap/radio.liq` starts with `# radio-atlantida-contract: 2` and `# min-backend: 1.0.0`.

## 6. Deploy follow-up (radio-atlantida-deploy, after P1 and P2 are on the backend ref)

1. `scripts/check-backend-sync.sh --update` (with `--ref` of the backend commit): rewrites `config/liquidsoap/radio.liq` (dev script + the empty `production.patch`) and `config/backend-source.env`; review and commit. `tests/liquidsoap` follows `dev/liqsmoke/compose.yml` as before.
2. `bin/radioctl` needs no change: it does not accept or refuse contract numbers. It records `liquidsoap_contract` in its state and lists "liquidsoap script contract 1 -> 2" as pending (`pending_list`, `bin/radioctl:1257-1266`), which requires `update.bat maintenance` to restart Liquidsoap; it refuses a backend below `min-backend` (`:1404-1406`, `:2222-2223`), still 1.0.0. Check that the dry-run environment, which was set up with contract 1, shows the pending maintenance after the update.
3. Repeat the dry run in the OrbStack VM `radio-dryrun` (`deploy/docs/dry-run.md` §6) with these extra checks:
   - cut-over (`bin/radioctl cutover`) while the test mount still has a source: the Live page shows `disconnected` and the header `reconnecting` until the old source leaves, then `connected`;
   - `update.bat maintenance` (Liquidsoap restart) mid-hour and a plain `docker restart` of `liquidsoap` mid-hour: the next item and a timed one-time insert added for that hour air at their planned second (journal vs plan), the gap is the emergency playlist;
   - `bin/radioctl compose stop liquidsoap`: the Live page shows "unknown" within 15 s and the header `unknown`;
   - backend restart (`bin/radioctl compose restart backend`) with Liquidsoap running: the output state is correct within 5 s, with the `since` from before the restart.
4. Record the results in `dry-run.md` (pull to the docs repository afterwards).

## 7. Follow-ups (not in this plan)

- **§7.2 gap rule.** "A gap between anchors shorter than `rotation.min_gap_ms` gets no music: the next insert starts earlier within its window, else the gap stays." If the gap stays in the plan, the pusher pushes the insert as soon as the item before it is queued, so it starts early by that gap even without a restart. Verify in the builder (`internal/rotation`) whether such a gap can remain, and if so either close it in the builder or extend P2's hold to every empty-queue case (developer decision 2026-11-14: keep the hold scoped to resync in this plan; decide the extension after the builder check).
- **A held mount is not an outage.** With the mount held by another source, the mount check is OK and the outage detector opens nothing; after P1 the header says `reconnecting` indefinitely. Developer decision 2026-11-14: a Grafana alert in radio-atlantida-deploy when the broadcast state stays `reconnecting` longer than 2 minutes (a separate small deploy task).
- Option B (resume the interrupted track), decision 8.

## 8. Deviations (as implemented)

1. **`align` is created in `NewService`**, not in `Run`: `Run` and the test harness both go through the constructor.
2. **`holdUntil` is cleared at the start of `pass`**, not just before the `fill` loop: a pass that fails before `fill` (link down) would otherwise keep a due time in the past, and `Run`'s timer (`max(0, …)`) would pass again at once, in a loop.
3. **The `align` entry is dropped after the first successful push**, not before the push loop; and when `markQueued` refuses an item while the hour is still held (rebuilt or started meanwhile), `fill` returns instead of going on to the next item, which would then be pushed early. The next pass holds again on the current plan.
4. **Tests.** `TestResyncKeepsHourAligned` failed on the old `pusher.go` (insert 2 min early; 8 min with the 10-minute track) and passes now. Its fake Liquidsoap resolves a request at once, so a held item starts `holdLead` early in the simulation and the hour ends `holdLead` early: the "no emergency after the plan resumed" assertion allows that last 0.5 s. The "new resync during the hold" case of `TestResyncHoldEdgeCases` reconnects to a Liquidsoap whose queue still plays (the hold is dropped and the items are pushed).
5. **Liveness for the silence reset.** `outputBeat` is set at the receipt time (backend clock) by every heartbeat, with or without the output fields, and by every `output` callback, not only by heartbeats with the fields: otherwise a contract 1 script (no fields) would show `unknown` 10 s after every connect although its heartbeats keep coming.
6. **`output_since` absent.** A heartbeat with `output_connected` and without `output_since` takes the heartbeat time only when the state changes; with the state unchanged the stored `since` is kept (the heartbeat time differs every time and would send `icecast.state` every 5 s). An `output` callback whose time differs from the stored `since` with the same state now sends one event (a reconnect whose disconnect was lost), as a heartbeat does.
7. **Events under `mu`.** `recordOutput` and `outputExpire` publish `icecast.state` under `Service.mu`, as `setSafetyLocked` does, so the callback goroutine and the pusher send them in the order of the changes.
8. **Handler tests** are new tests, `TestPlayoutHeartbeatOutputFields` (contract 2 body, an offset `output_since` mapped to UTC, contract 1 body, unknown field `output_state` → 422) and `TestPlayoutStatusOutput` (absent, `connected`, `unknown`); `TestPlayoutHeartbeatSafetyField` is unchanged.
9. **liqsmoke.** The Icecast stop/start check is a new last step (8) on a plain output `/smoke-heartbeat`: the TLS proxy resolves `icecast` once at its start, so a restarted Icecast could break it for the TLS steps. Icecast comes back with `docker compose up -d --wait icecast`. The stub answers 422 to a body the strict DTO refuses (as the backend), and the run fails on any. First run: 172 callbacks, all accepted.
10. **An integration test not named in P2.6.** `TestAirWindowOnAir` (`internal/httpapi/airwindow_integration_test.go`) starts a fresh pusher, whose first pass is a resync with an empty queue, so the next item is now held until its planned start − 500 ms; the test clock (fixture time + real time) never reached it within the 10 s wait. The test moves the clock to that moment for the push check and back afterwards (731ff6e). No other test changed behaviour.
11. **Output generation in the script (fix after deploy's smoke test).** `output.set` calls `shutdown()` on the old output, which does not wait: with another mount or host the new output connected first and the old output's `on_disconnect` came about 160 ms later, so its `report_output(false)` overwrote `output_up`/`output_changed`, and every heartbeat said disconnected while the stream was on air (deploy `tests/liquidsoap/run.sh` step 8, `/smoke-tls` to `/smoke-heartbeat`, 2 of 2 runs). Each output now takes the generation current when `make_output` builds it; `set_output` raises it before the shutdown, and only the current generation reports or saves the state file (a replaced output's late connect or disconnect is not journaled, not sent, and leaves the state unchanged). The old output's disconnect is reported by `set_output` itself, before the new output starts, when the state was connected, so an output that never connects is not shown connected. Payload, telnet commands and journal format are unchanged: contract stays 2. liqsmoke step 7b switches three times from `/smoke-tls` (TLS proxy) to the plain `/smoke-switch` and back, and fails on a disconnected report timed after the new connect or a heartbeat that is not connected since it; it failed on the script of `11bb999` (switch 1) and passes now.
