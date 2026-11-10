# Plan — First Build of a Started Hour That Has No Plan (radio-atlantida-backend)

Status: Requested by the operator (2026-11-10); not started
Based on: `docs/tech-design.md` (§7, §8.6), `docs/phase-8-plan.md` (sync), `docs/phase-9-plan.md` (freeze), `docs/phase-23-plan.md` (build)
Last updated: 2026-11-10

## 1. Goal

Today an hour that has started and has no plan can never get one. The operator assigns a clock to the current hour, the grid saves the slot without a word, every sync skips the hour, and the air stays on the emergency source until the next planned hour.

After this change, a grid hour that has started and has no `plan_hours` row is built once, for what is left of the hour.

An hour that has started and has a plan keeps it, exactly as today.

## 2. Decisions

1. **Only a missing hour.** The new path runs only when the current hour is on the grid and has no `plan_hours` row. A stored row, even one with no items, blocks it. Stale started hours are not rebuilt.
2. **One build.** Once the row exists, `found && frozen` keeps the partial hour. A later clock edit or a rebuild does not touch it.
3. **The current hour only.** Earlier hours are over. The sync window for everything else stays `(now, horizon]`.
4. **Sync only.** The new path lives in `Service.sync`, so a grid change and the 5-minute job both reach it. The full pass keeps `SkipStarted`.
5. **Start with a lead.** The partial plan starts at `now + startedHourLead` (10 s, a named constant). The pusher ticks every 5 s and skips items more than 30 s late, and the planner does not wake it.
6. **Minimum remainder.** If less than `startedHourMinRemainder` (2 minutes, a named constant) is left after the lead, nothing is built and **no row is written**.
7. **No schema change.** The start offset of a partial hour is not stored. The seed stays the hour start, so the seed alone no longer reproduces a partial hour; this is accepted.
8. **The grid still accepts a started hour.** No new refusal.

## 3. Work

Line numbers are from 2026-11-10 and are hints only.

### 3.1 Sync (`internal/rotation/service.go`)

- In `sync`, after the window read (`after := now.Add(time.Nanosecond)`, about line 345), look up the grid hour that contains `now`. Add it to the work list only if it has no `plan_hours` row and the remainder rule of decision 6 passes.
- Do not move `after` back: lines 349–390 rely on this window, and stale started hours would join the work list on every pass.
- A `force` rebuild (`RebuildRange`) follows the same rule: the current hour joins only when it has no row.

### 3.2 Build under the lock (`buildTx`, about line 512)

- Keep `found && frozen` (about line 526) as it is. It is what makes the build one-shot, including when two syncs race.
- Compute the start offset from the `now` taken under `LockPlan`, not from the one sync read earlier. Re-check decision 6 there; on failure skip the hour without writing a row.
- Pass the offset as `HourInput.FromMS`.

### 3.3 Builder (`internal/rotation/build.go`)

Add `HourInput.FromMS` (default 0, so `simulate`, `sample` and every future hour are unchanged). With `FromMS > 0`:

- The cursor starts at `FromMS` (it is the zero value today, about line 96).
- `start` and the start part of `start_end` are skipped.
- Timed inserts (template and one-time) and `middle` whose window has passed are skipped. An insert whose window contains `FromMS` is placed if it still fits its window.
- `spread` slots before `FromMS` are dropped. The count of the remaining slots is not raised to make up for them.
- `end`, the crossing rule and the hard point at `HourMS` are unchanged.
- Passed slots raise no "slot is passed" warnings (about lines 756–757): they are expected here. Real warnings (no allowed track, gap left) stay.

### 3.4 Play rules in the partial hour

The hour may already have aired tracks, for example from the emergency playlist. Check how the history window is bounded for a build. Play log rows from the hour start up to `now` must count for the track and artist intervals and for max per hour. If the window ends at the hour start, extend it to `now` for a partial build only.

### 3.5 Pusher check (no change expected)

Confirm that an item of the current hour that appears mid-hour is pushed to the current parity queue (`hour_<p>`), the same as after a resync. If that is not so, stop and report it; do not redesign the pusher in this task.

### 3.6 Not in this task

- Frontend text ("Hours that have started keep their plan") and a warning on saving a slot for a started hour.
- `tech-design.md`: updated in the docs repository after this task.

## 4. Tests

Unit (`rotation`, fake stores, injected `Clock`), table-driven:

- Started hour, on the grid, no row: built; first item at or after `now + lead`; nothing after `HourMS`.
- Started hour with a row, with items and with none: not touched, also with `force`.
- Remainder below the minimum: no build, no row; a later hour in the same sync is still built.
- Started hour not on the grid: nothing.
- Second sync after a partial build: no rebuild.
- Freeze window before the start: behaves as today.
- `FromMS = 0`: the plan of a fixed seed is identical to today's.
- Builder with `FromMS`: passed `start`, `middle`, inserts and `spread` slots are skipped with no warning; an insert whose window contains `FromMS` is placed; `end` is placed.
- A track aired in the hour before `now` counts for the interval and max-per-hour rules.

Integration (`-tags integration`): grid assign into a started hour, then sync, stores one `plan_hours` row with items only in the remainder; concurrent syncs produce one row.

## 5. Order of work

1. `FromMS` in the builder, with its tests.
2. Sync and `buildTx`, with their tests.
3. History window check (§3.4).
4. Pusher check (§3.5).
5. Manual check on the dev stack: assign a clock to the current hour with the emergency playlist playing. Record whether the switch happens within a few seconds and how the emergency track's end is reported when it is cut.

## 6. Done when

- `make test`, `make test-integration`, `make lint` pass.
- `FromMS = 0` builds are unchanged.
- On the dev stack, assigning a clock to the started hour makes the hour appear in `GET /api/track-plan` with items only in the remainder, and those items air.
- No migration and no API change.
