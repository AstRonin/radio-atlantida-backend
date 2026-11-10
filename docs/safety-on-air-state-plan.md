# Plan — Report When the Safety Tone Is on Air (radio-atlantida-backend)

Status: Requested by the operator (2026-11-10); not started
Based on: `docs/tech-design.md` (§8.3, §8.6), frontend `docs/on-air-indicator-plan.md`
Last updated: 2026-11-10

## 1. Goal

When the plan and the emergency playlist have nothing to play, Liquidsoap airs the safety tone. The backend does not know this: the tone is not reported, and for the backend it looks the same as the moment between two tracks. The panel cannot warn the operator.

After this change the backend knows whether the safety source is on air, and the panel can read it and follow its changes.

## 2. Decisions

1. **The heartbeat carries the state.** The script already POSTs a `heartbeat` every 5 s. It gains a boolean: the safety source is on air. A state that is repeated every 5 s heals by itself after a backend restart. Track start and end events are not added for the tone, and the heartbeat stays out of the journal.
2. **The script derives it from readiness.** The tone is on air when the source before it in the fallback (the hour queues and the emergency source) is not ready, for example `source.is_ready`. Use the exact mechanism that works in Liquidsoap 2.4.6.
3. **Two reports in a row.** `on_air` becomes true only after two consecutive heartbeats say so (about 10 s), so a short gap between tracks raises no warning. It becomes false at the first heartbeat that says false.
4. **A missing field means unknown.** A heartbeat without the field does not change anything and raises no warning. A script that does not report it keeps working.
5. **Lost heartbeat.** When heartbeats stop for longer than the existing heartbeat timeout, the state is reset to not on air. The outage banner covers that case.
6. **Kept in memory.** No migration and no journal change.
7. **Only the dev script.** The production script does not exist yet. When it is written it must report the same field (`dev/liquidsoap/radio.liq` is its reference).

## 3. Work

### 3.1 Script (`dev/liquidsoap/radio.liq`)

Add the boolean to the heartbeat body per decisions 1 and 2.

### 3.2 Backend (`internal/playout`, `internal/httpapi`, `internal/ws`)

- Heartbeat DTO: an optional boolean field.
- `playout`: the state with the two-reports rule, on the injected `Clock`, guarded the same way as `nowPlaying`, plus `on_air_since`.
- `GET /api/playout/status`: adds `safety_on_air` (boolean) and `safety_since` (optional UTC time).
- New WebSocket event `playout.safety` with `on_air` and optional `since`: a named schema registered in `httpapi/api.go`, sent only when the state changes, including the reset of decision 5.
- Regenerate `api/openapi.json` (`make openapi`).

### 3.3 Not in this task

- The now-playing restore after a backend restart mid-track.
- Warning about an unset emergency playlist.

## 4. Tests

Unit (table-driven, injected `Clock`):

- One true heartbeat: not on air, no event.
- Two true heartbeats: on air, one event, `since` set.
- More true heartbeats: no more events, `since` unchanged.
- A false heartbeat: off at once, one event.
- True, false, true: not on air after the last.
- A heartbeat with no field: no change, in either state.
- Heartbeats stop past the timeout: reset, one event.
- `GET /api/playout/status` returns both fields.

Check the script without touching the running stack: `liquidsoap --check` in a throwaway container of the pinned image. If possible, run the script in a throwaway container with no plan and no emergency URL response, and confirm that the heartbeat reports true.

## 5. Done when

- `make test`, `make lint`, `make openapi-lint` pass, and `make test-integration` has no failure beyond the three known ones (`TestPipeline`, `TestImportFolder`, `TestRotationSimulate`).
- The script passes the check.
- No migration.
