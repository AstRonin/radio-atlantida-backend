# Phase 8 Plan — Rotation Engine

Status: Done
Based on: `base.md`, `tech-design.md` (§4, §6.0, §6.5, §7, §7.2, §7.3, §10.2), `backend-init-plan.md` §9 item 8, `phase-7-plan.md` §5
Last updated: 2026-10-14

## 1. Goal

The backend keeps a rolling track plan (`scheduled_items`) for the next
`rotation.plan_horizon_hours` (24–48 h). Each grid hour is built from its clock
with real tracks: union candidates, separation and play rules against the play
log and the plan, tolerance fit, filler for shortfall, truncation for
overflow, and the emergency steps. The engine is a pure, seeded function, so a
build is deterministic. Clock and grid changes rebuild the affected future
hours asynchronously (`clock_build`). Incomplete clocks and `ask` holders
create filler reminders; their answer or their deadline fills the hour.

Playout (pushing items to Liquidsoap, `queued`, moving aired items to the play
log) is Phase 9; outages and missed items are Phase 10.

## 2. Decisions (2026-10-14)

1. **Selection.** Candidates of an element are the union of its playlists'
   tracks that have a duration, without duplicates. A candidate is *eligible*
   when it passes separation and every play rule at its planned start. Among
   eligible candidates with `|duration − target| ≤ tolerance`:
   - an ordinary element picks one at random, every candidate with the same
     weight (target = nominal duration);
   - the last element of a segment picks the best fit to the time left in the
     segment (target = time left, clamped to nominal ± tolerance); ties are
     broken at random.
   This reconciles "every candidate has the same weight" (§7, decision 17)
   with "the one that fits the remaining time best" (`base.md`).
2. **Determinism.** The engine takes a seed (`math/rand/v2` PCG) and sorts
   every input by ID. The job seeds an hour with its UTC start (Unix
   seconds); the seed is stored with the hour.
3. **Segments with real tracks.** The layout of Phase 7 gives the segments and
   their holders. For a segment closed by a fixed element with target `T` and
   tolerance `tol`:
   - an item ending at or before `T + tol` is kept;
   - an item ending after `T + tol` is the crossing track: it is cut at `T`
     (truncated, faded out by Liquidsoap) if it starts before `T`, otherwise it
     is removed; the remaining elements of the segment are removed;
   - content ending before `T − tol` leaves a gap that filler closes; the fixed
     element starts at `max(content end, T − tol)`.
   The last segment ends at the hour: a crossing track is cut at 60:00.
4. **Filler.** A gap of at least `rotation.min_filler_gap_ms` (new setting,
   default 10 s) gets filler; a smaller gap is left without a warning (a few
   seconds at the end of most hours would otherwise warn every hour). Filler picks
   the best fit to the gap among eligible tracks of the source, repeating up to
   3 tracks; the last may be truncated by the rules above. The mode is the
   holder's; for `none`, the clock default, else `filler.fallback`. Sources:
   `random` and the emergency general pool = every track with a duration;
   `playlist`; `artist`; `pool` = the clock's pool in position order, a
   refused pool track is skipped with a warning and stays unused; an empty
   pool uses the default mode and creates a `pool_empty` reminder.
5. **Reminders.** A short segment whose holder is `none` (incomplete clock) or
   `ask` waits for the operator when the built hour actually has a gap there
   (real tracks can close a nominal shortfall; then nothing is asked): the
   build creates one `filler_requests` row
   (`short_clock`) per clock hour, `deadline_at = hour start −
   filler.reminder_deadline_minutes`, and leaves the gap. If the deadline has
   already passed when the hour is built, the request is stored `defaulted` and
   the default mode fills the gap at once (air never waits).
   - Answer (`POST /api/filler-requests/{id}/answer`, `track_id`): the answer
     track fills the first waiting segment of the hour (`filler_answer`), the
     clock default fills the others. A track that breaks separation or play
     rules at that time is refused (422). Synchronous.
   - Deadline: the plan job sets due requests `defaulted` and fills their
     hours with the default mode.
   - Resolving rebuilds the hour with its stored seed. A rebuild for a change
     expires the hour's pending requests (§7) and creates new ones.
   - Refilling a pool (`PUT /api/clocks/{id}/filler-pool`, or save-with-filler
     with a pool) answers the clock's pending `pool_empty` requests.
6. **Emergency steps** (§7) apply when an element has no fitting eligible
   candidate and not every candidate is blocked by play rules: (1) relax the
   tolerance, doubling it, up to `rotation.max_relaxed_tolerance_ms` (new
   setting, default 120 s); (2) best fit from the general pool; (3) skip the
   element with an `element_emergency` warning — Liquidsoap's emergency
   playlist covers it (§8.6, Phase 9). An element whose candidates are all
   blocked by play rules is skipped (`element_blocked_by_play_rules`, §7.3);
   rules are never relaxed. Separation is never relaxed either.
7. **History** for separation and play rules: `play_log` rows `aired` or
   `truncated` (at `started_at`) and `scheduled_items` rows `planned` or
   `queued` (at `planned_start_utc`), before and after the hour, excluding the
   hour being built. Track separation uses the longest `track` rule, artist
   separation the longest `artist` rule; tracks without an artist have no
   artist separation. `max_per_hour` counts starts in the same station-local
   clock hour.
8. **Plan hours** (design addition to §4): `plan_hours` records each built
   hour — `hour_start_utc` (PK), `clock_id`, `clock_updated_at`,
   `grid_slot_id`, `seed`, `built_at`, `filler_request_id`. An hour is *stale*
   when the grid now assigns another clock (or none) or the clock's
   `updated_at` changed. `scheduled_items` gains `clock_element_id` (panel
   view) and `filler_pool_id` (a rebuild releases the pool rows its items
   took: `used_at` back to null).
9. **Sync.** One function keeps the plan in step with the grid: for every
   grid hour starting in `(now, now + horizon]` it builds missing and stale
   hours; future planned hours the grid no longer assigns are cleared (their
   planned items deleted, pending requests expired). An hour that has started
   is never rebuilt (Phase 9 defines the queued boundary). Each hour is built
   in its own short transaction under a transaction-level advisory lock and
   re-checks staleness after taking it, so the job and request-started
   rebuilds never conflict.
10. **Operations.** The job `track_plan` (every 5 minutes, first 30 s after
    start) processes due reminders, then syncs. A clock save, save-with-filler,
    save-incomplete, a grid assign/unassign, and a slot delete start a
    `clock_build` operation that syncs; its ID is returned as
    `build_operation_id`. Request-started builds are queued to one planner
    goroutine owned by `serve` (no context stored, no goroutine per request).
    `POST /api/track-plan/rebuild` rebuilds the future hours of a range
    (`clock_build`).
11. **Events.** `track_plan.warning` (one per hour with warnings) and
    `filler_request.created`; warnings are also logged at WARN (component
    `rotation`, filler ones `filler`). Event names follow the
    `<kind>.completed` rule of §6.5 (`track_plan.completed`, not
    `track_plan.built`).

## 3. Engine (`internal/rotation`)

`Build(in HourInput) HourPlan` is pure: clock with layout, hour start and
local weekday/hour, the library snapshot (tracks with duration, artist,
playlists, rule IDs; rules), history, separation intervals, pool rows,
resolution of waiting segments, settings, seed. It returns items (track,
element, offset, out point, truncated, source, pool row), warnings, waiting
segments, and the reminders to create. Warning codes:

| Code | When |
|------|------|
| `element_blocked_by_play_rules` | Every candidate is blocked by play rules; element skipped |
| `element_tolerance_relaxed` | Picked with a relaxed tolerance |
| `element_general_pool` | Picked from the general pool |
| `element_emergency` | No track at all; element skipped |
| `pool_track_refused` | A pool track breaks a rule; skipped |
| `pool_empty` | Pool mode with an empty pool; default mode used |
| `filler_waiting` | A segment waits for a reminder answer |
| `gap_unfilled` | A gap no filler could close (or below the minimum) |

## 4. API

| Method and path | Purpose |
|-----------------|---------|
| `GET /api/track-plan?from=&to=` | Built hours (at most 7 days) with their items: track, artist, element, start, out point, truncated, source, status |
| `POST /api/track-plan/rebuild` | Body `from`, `to`: rebuilds the future hours in the range; `accepted` + operation ID |
| `GET /api/filler-requests?status=` | Reminders (default `pending`), newest hour first |
| `POST /api/filler-requests/{id}/answer` | Body `track_id`; fills the hour; refusals per field |
| Clock and grid writes | Responses gain `build_operation_id` |

New problem codes: `filler_request_not_found`, `filler_request_closed`,
`hour_started`. New field code: `blocked` (a track breaks separation or a play
rule).

## 5. Known limits (for later phases)

- Planned items whose time has passed stay `planned` and keep counting for
  separation until playout (Phase 9) moves them to the play log and outages
  (Phase 10) mark the missed ones.
- The queued boundary: Phase 9 pushes the first items of the next hour
  before its start. A rebuild already keeps `queued` items, but it plans the
  hour from its start, so Phase 9 must start the rebuilt part after them.
- A reminder whose deadline passes while the hour has started is only
  closed; the hour keeps its gap.
- One pending reminder per clock hour (the partial unique index): a
  `pool_empty` reminder is not created in an hour that already waits for a
  `short_clock` answer.
- `plan_hours` rows of past hours are not removed yet; retention can take
  them with the play log.

## 6. Done when

Unit tests cover selection, the segment rules (crossing, removal, fixed
window), filler and its modes, reminders, emergency steps, separation and play
rules against history, and determinism per seed. Repository integration tests
run against PostgreSQL. API integration tests cover the plan build after a
grid assignment, the rebuild after a clock change, reminders with answer and
deadline, and the refusals. `make lint`, `make test-integration`, and `make
openapi-lint` pass.
