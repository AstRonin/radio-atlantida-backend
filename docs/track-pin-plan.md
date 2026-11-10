# Pin a planned track by dragging it inside its hour (backend and frontend)

## Goal

On the Track plan page the operator drags a planned track to another position
inside its hour. The track is then fixed at that position until the end of the
hour: a rebuild of that hour keeps it there, and the rest of the hour is built
around it. This replaces the current way of moving a track, which is to delete
it and add an insert in a modal.

Decision (from the operator's discussion): the pin lasts until the end of the
hour, with no separate duration. The move stays inside the clock: clock anchors
are not moved. Moving Rule rows was left out of this plan and is added by
`track-pin-rule-plan.md`.

## What already exists (backend, checked in code)

- A one-time insert (`hour_inserts`, `placement = fixed`) is a fixed point in one
  planned hour. It survives rebuilds. `AddInsert` (`internal/rotation/inserts.go:111`)
  and `RemoveInsert` (`inserts.go:154`) run a forced rebuild of that hour.
- A rebuild (`RebuildRange`, `internal/rotation/planner.go:48`) deletes all
  `status = 'planned'` rows of the hour, including Fill, Rule and insert rows
  (`internal/repository/rotation/plan.go:72-74`), and builds the hour again with
  a seed equal to the hour start in Unix seconds. The builder's random draws are
  sequential, so a new fixed point can change the Fill tracks placed after it in
  the same hour.
- Start times are computed when the hour is built (`place()`,
  `internal/rotation/build.go:717`). Items are not moved after they are built.
- Hours that have started (or start within `playout.prepush_minutes`, default 5)
  are frozen (`service.go:216-218`, `fullpass.go:86-100`, `inserts.go:165`).
- There is no lock, pin or manual flag on `scheduled_items` or `plan_hours`.

Consequence: a pin is an input of the build, not an edit of a stored row. The
design below reuses the one-time insert mechanism and does not add a new flag to
`scheduled_items`.

## Backend

### Behaviour

1. **Pin a planned Fill track at a new time.** The request names a planned item of
   the hour (its `id` in `scheduled_items`), the new `offset` inside the hour
   (milliseconds, `0 ≤ offset < 3600000`) and the start tolerance (default from
   the one-time insert, 60 s). The service:
   - refuses if the hour is frozen;
   - refuses if the item is not `source = 'fill'` (see Out of scope for Rule
     items);
   - creates a one-time insert with `track_id` of that item, the new offset, and
     `placement = fixed`;
   - rebuilds the hour with `force` in the same transaction as the insert
     (the planner queue is used as today; the insert and the rebuild are one
     unit of work).
2. **Move an existing one-time insert** (already pinned by the operator, or added
   through the modal). The request names the insert id and the new offset. The
   service updates the offset and rebuilds the hour. Same refusals as in 1.
3. **Unpin.** Existing `DELETE /api/track-plan/inserts/{id}` (`RemoveInsert`).
   Unpinning a Fill track returns it to the builder, which may choose another
   track at that place. This is expected and is stated in the UI.
4. **Conflict refusal.** A move is accepted only if the builder places the insert
   within its tolerance. If the builder cannot (for example the new place falls
   on a Rule slot or breaks a play rule), the whole change is rolled back and the
   response is a refusal with the reason. The change is never saved without a
   successful build. The builder already refuses an unplaced one-time insert
   (`placeAnchor`, `internal/rotation/build.go:427-447`). For a new or moved
   insert, `service.go:530-531` already returns `InsertError` and the hour is not
   replaced (`service.go:532-550`). Pins and moves must use this same path, not
   the later silent drop (`settleInserts`, `service.go:561-580`, which turns
   refused inserts into `hour_insert_dropped` warnings). Only inserts that already
   existed and fail on a later rebuild are dropped with a warning.

### Endpoints (huma, `internal/httpapi/handlers/rotation.go`)

- `POST /api/track-plan/pins`: body `{ "plan_item_id", "offset_ms", "tolerance_ms" }`
  (`tolerance_ms` as in the existing `POST /api/track-plan/inserts`; the frontend shows seconds and sends milliseconds).
  Creates the one-time insert and rebuilds the hour. Like `AddInsert` and the
  rebuild route, it answers `202` and runs as an operation (`clock_build` or the
  same kind as `AddInsert`); the refusal of an unplaced pin is reported through
  the operation's failure, with the reason.
- `PATCH /api/track-plan/inserts/{id}`: body `{ "offset_ms", "tolerance_ms" }`.
  Moves an insert; same `202` and operation behaviour.
- `DELETE /api/track-plan/inserts/{id}`: existing.

DTOs in `internal/httpapi/dto/rotation.go`. Validation errors per field with
existing validation codes. New refusal codes are added to `errcodes` in the same
change (names to be chosen with the team, for example `hour_frozen`,
`insert_not_placed`, `not_movable`). Domain errors map to HTTP only in
`handlers.problemFor`.

### Where the code goes

- `internal/rotation`: `PinFill` and `MoveInsert` methods on `Service`, next to
  `AddInsert`. They open one transaction (`d.WithTx`), write the insert through
  the rotation store, and enqueue the forced rebuild of the hour in the same
  transaction. Freezing and item checks are in the domain, not the handler.
- `internal/repository/rotation`: a method to update an insert's offset (the
  store interface is declared by the domain). No SQL in the domain.
- No migration is expected: `hour_inserts` already has `offset_ms` and
  `placement`. If the table definition needs a column (for example the link
  from a pin to the original plan item), that is a separate migration and the
  work order must say so. Apply it with `make migrate` before running tests.
- The OpenAPI document is regenerated with `make openapi` and checked with
  `make openapi-lint`.

### Tests

- Unit tests in `internal/rotation` with fake stores: pin of a Fill item creates
  an insert with the same track; move of a frozen hour is refused; move that the
  builder cannot place is rolled back and returns the refusal.
- Determinism: the same pin, twice, gives the same hour (seeded build).
- Integration test (`make test-integration`): pinned track keeps its offset after
  `RebuildRange` for that hour; an unpinned Fill track may change (documented
  expectation, not a bug).
- `make test` and `make lint` pass.

## Frontend (Track plan page)

Use the drag-drop module of `@angular/cdk`, which is already a dependency. Add no
new library. The backend part is done: regenerate the TypeScript client from the
backend's OpenAPI document. Pins and moves answer `202` with an operation ID of
kind `hour_insert`; the page waits for `hour_insert.completed` or
`hour_insert.failed` as the "Add insert" flow does, and shows the reason on failure.

- Planned rows of an hour that is not frozen get a drag handle. Dropping a row at
  another position calls `POST /api/track-plan/pins` (for a Fill row) or
  `PATCH /api/track-plan/inserts/{id}` (for an insert row). The new offset is
  the start of the row the track lands on when it moves up, and the end of that
  row when it moves down, relative to the hour start and rounded to whole seconds
  (the backend accepts whole seconds only). If the track would not end by 60:00
  from that offset, it starts at 60:00 minus its own length, rounded down to whole
  seconds, so it is the last track of the hour. Decided by the developer.
- After the drop a small dialog asks for the start tolerance (in seconds,
  default 60, the value the one-time insert uses). It has a checkbox "Don't show
  again for this hour". When it is checked, later drops in the same hour use the
  last value without the dialog. The choice is kept in page memory only (not in
  the backend and not in browser storage), so it is lost on reload.
- The button "Rebuild these dates" calls `POST /api/track-plan/rebuild` with
  `{ from, to }` (`track-plan-page.component.ts:91, 339-357`). This is the
  existing rebuild; pins do not replace it.
- Each row gets a keyboard alternative: move up and move down buttons. They use
  the same calls, so the behaviour is the same with mouse, touch and keyboard.
- A pinned row shows a lock with the text "fixed until the end of the hour".
  Pressing it unpins (`DELETE /api/track-plan/inserts/{id}`).
- Frozen hours show no handle. Rows that are Rule items show no handle.
- During a move the list shows a busy state. On a refusal the list is not
  changed and the error text from the error code is shown next to the hour.
- The existing "Add insert" modal stays. It is the way to add a new track.
- Undo is out of scope for this step.

## Out of scope

- Moving Rule items (see `track-pin-rule-plan.md`), clock anchors or inserts that
  belong to a clock (not a one-time insert).
- Moving an item to another hour.
- A pin with a duration other than "until the end of the hour".
- Changing the builder itself beyond what the conflict refusal needs.

## Decisions (resolved)

1. Refusal of an unplaced pin or move: the same path as a new one-time insert
   (`InsertError`, the hour is not replaced). No silent drop. Decided by the
   developer.
2. A pinned Fill track keeps its track on a later rebuild of the same hour. It is
   never replaced by another track. If it can no longer be placed, see decision 7.
   Decided by the developer.
3. The rebuild button calls `POST /api/track-plan/rebuild` with `{ from, to }`
   (checked in the frontend, see above).
4. The start tolerance is chosen by the operator at the drop, default 60 s, with
   the checkbox "Don't show again for this hour" (see the frontend section).
   Decided by the developer.

5. Tolerance units: milliseconds in the API (`tolerance_ms`), as in the existing
   one-time insert endpoint. The operator still sees seconds in the dialog.
   Decided by the developer.
6. A pinned track is excluded from the rest of its hour. The builder treats it as
   already played in that hour, so the other fill draws pick the other tracks
   (for example, with a rule of 3 different tracks per hour and one pinned, the
   other two slots are filled from the remaining tracks). This applies even where
   no play rule covers the track. Decided by the developer.
7. Unpinning removes the pinned track from the hour and recomputes the rest of the
   hour. The track is not kept as a fixed point. A recomputed hour may choose
   another track for the freed slot. The same applies when a pin can no longer be
   kept on a later rebuild: the pin is removed, the hour is recomputed, and a
   warning (`hour_insert_dropped`) is shown. Confirmed by the developer.

8. There is no stored pin flag, and none is added. Decision 6 applies to the
   one-time inserts of the hour, including manual inserts and reschedules: their
   tracks are also excluded from the fill draws of that hour. Decided by the
   developer.

## Open points

- None.

## Done when

- An operator can drag a planned Fill track to another place in an open hour;
  after `Rebuild these dates` the track stays at that place until the end of the
  hour.
- Dragging in a frozen hour is not possible, and a move the builder cannot place
  shows the refusal and changes nothing.
- Unpinning returns the track to the builder.
- The backend tests above pass; the OpenAPI document and lint are up to date.
