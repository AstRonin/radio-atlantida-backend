# Phase 7 Plan — Clocks and Grid

Status: Done
Based on: `base.md`, `tech-design.md` (§4, §6.0, §6.2, §7, §7.2, §7.3, §10.2), `backend-init-plan.md` §9 item 7
Last updated: 2026-10-14

## 1. Goal

The manager builds 60-minute clock templates and assigns them to hours of the
grid, up to 30 days ahead. Saving a clock runs the dry check and, when the clock
is short, leads to the filler choice (§6.0). The track plan, filler reminders,
and the rebuild of planned hours are Phase 8 (rotation); this phase leaves clear
hooks for them.

## 2. Decisions (confirmed 2026-10-14)

1. **Save is synchronous** (init plan 10.4 item 6). `POST/PUT /api/clocks` run the
   dry check and the save in the request and return the result. The async rebuild
   of planned hours (`clock_build.completed/failed`) arrives with rotation in
   Phase 8; its operation ID is added to the save response then.
2. **Shortfall is measured on nominal durations.** Tracks are not chosen at save
   time. A segment is short when the sum of its elements' `nominal_duration_ms`
   does not reach its end. `duration_tolerance_ms` only steers track selection.
3. **The filler choice is per segment.** `save-with-filler` takes
   `segments: [{segment, filler_mode, filler_source_id}]` and an optional ordered
   `pool_track_ids`. Each choice is stored on the segment's *holder* element (§3.3).
   This replaces the single `filler_mode` of §6.0, which cannot cover several short
   segments (§7.2 requires the dialog to cover each).
4. **Segments by position.** Elements are ordered by `position`. The flow
   elements between two fixed elements belong to the segment between them. Fixed
   elements must be in ascending minute order. `start` elements open the first
   segment, `end` elements close the last.

## 3. Clock rules

### 3.1 Element checks (refusal, per field)

- At least one element. Each element has at least one playlist; playlists of any type.
- `fixed_minute` is set if and only if `placement = fixed`.
- A fixed element's window (`fixed_minute` ± `fixed_tolerance_ms`) lies inside the
  hour, and the element fits before the end of the hour from its earliest start.
- Filler source: `filler_source_id` is required for `playlist` and `artist` and
  forbidden otherwise; it names an existing playlist or artist.
- Omitted tolerances take the defaults from the settings
  `rotation.default_duration_tolerance_ms` and `rotation.default_fixed_tolerance_ms`.

### 3.2 Replacement of fixed elements (§7.2)

Two fixed windows overlap when `|start₁ − start₂| < tol₁ + tol₂` (touching
windows do not overlap). In a save, an element with an `id` is an existing element
of the clock, and one without is new. A new fixed element that overlaps an
existing one replaces it: the existing one is removed and named in
`replaced_elements`. Two new or two existing elements that overlap are refused.

### 3.3 Segments and fitting

The hour is simulated on nominal durations. Content of a segment starts where
the previous fixed element ends. At a fixed element with target `T` and
tolerance `tol`:

- content ending after `T + tol` is refused: on the first `start`/`end` element
  that does not fit when those alone overrun, otherwise on the fixed element
  ("the material before it does not fit");
- content ending before `T − tol` leaves `shortfall = T − tol − end`, and the fixed
  element starts at `T − tol`;
- otherwise the fixed element starts where the content ends (no shortfall).

The last segment ends at the hour: `shortfall = 3600000 − end`. Overrunning the
hour is not refused (overflow truncates the crossing track, §7); it is reported as
`overflow_ms` with a warning. `start`/`end` elements that alone overrun the hour
are refused.

**Holder** of a segment's filler choice: its last element; if it has none, the
fixed element that opens it; otherwise the fixed element that closes it.

**Status**: `complete` when every short segment's holder has a `filler_mode`
other than `none`, otherwise `incomplete`. `requires_filler_choice` is true
exactly when the clock is incomplete. A choice already stored on the clock is not
asked again.

### 3.4 Dry check warnings (do not block the save)

| Code | When |
|------|------|
| `element_no_tracks` | No track of the element's playlists has a duration |
| `element_no_fitting_track` | No track lies within nominal ± tolerance |
| `element_few_tracks` | Fewer fitting tracks than the element draws within the track separation window, counted from the grid hours of the clock |
| `element_blocked_by_play_rules` | In an hour (weekday, hour) to which the grid assigns the clock, every fitting track is blocked by the time windows and weekdays of its play rules (§7.3) |
| `segment_overflow` | The last segment runs past the end of the hour; the crossing track is cut |

### 3.5 Endpoints

| Method and path | Purpose |
|-----------------|---------|
| `GET /api/clocks` | Clocks with status and element count |
| `GET /api/clocks/{id}` | A clock with its elements and layout (segments, shortfall) |
| `POST /api/clocks`, `PUT /api/clocks/{id}` | Save with the dry check: `clock`, `check` (`shortfall_ms`, `requires_filler_choice`, `segments`, `warnings`, `replaced_elements`) |
| `POST /api/clocks/{id}/save-with-filler` | Per-segment filler choice plus pool; the clock becomes complete |
| `POST /api/clocks/{id}/save-incomplete` | Acknowledges the shortfall; the clock stays incomplete (reminders: Phase 8) |
| `DELETE /api/clocks/{id}` | Refused while grid slots use it (`clock_in_use`), or it has history (`clock_has_history`) |
| `GET /api/clocks/{id}/filler-pool`, `PUT /api/clocks/{id}/filler-pool` | The ordered pool: used rows stay as history; PUT replaces the unused rows |

## 4. Grid rules

- A slot is weekday (1 = Monday … 7 = Sunday) + hour (0–23) in station local time
  + a clock + `valid_from`/`valid_to` (local dates, inclusive; open end allowed).
- `POST /api/grid/assign` takes `weekdays[]`, `hours[]`, `clock_id`,
  `valid_from`, `valid_to` and creates one slot per weekday and hour in one
  transaction. `POST /api/grid/unassign` takes the same body without a clock.
- `valid_from` lies between today and today + `rotation.grid_horizon_days`
  (station local). The past is never rewritten.
- Overlapping slots are replaced for the overlapping dates (§4): trimmed, split,
  or deleted. The result names each replaced slot and what happened to it.
- `GET /api/grid/slots?clock_id=&from=&to=` lists slots. `DELETE /api/grid/slots/{id}`
  deletes one; history keeps its clock and loses only `grid_slot_id`.
- `GET /api/grid?from=&to=` resolves the grid to hours: local date, weekday, hour,
  the UTC start of the hour, slot, and clock (at most 62 days per request). A local
  hour that does not exist (a DST gap) is skipped. Armenia has no DST (§10.2).

## 5. Phase 8 hooks (not built now)

- A clock or grid change recalculates `planned` items in the affected hours and
  expires their pending filler requests (§7). There are no planned items before
  Phase 8.
- Reminders for incomplete clocks and pool refill reminders (§7).

## 6. Done when

Unit tests cover layout, replacement, holders, status, the play rule window logic,
and grid replacement. Repository integration tests run against PostgreSQL. API
integration tests cover the save flow (short → save-with-filler → complete), the
refusals with field paths, the grid assign/unassign replacement, and the schedule
view. `make lint`, `make test-integration`, and `make openapi-lint` pass.
