# Pin a planned Rule track inside its hour (backend and frontend)

Status: approved by the developer.

Builds on `track-pin-plan.md` (pinning Fill tracks), which must be in place first.

## Goal

The operator can drag a Rule row (for example a jingle placed by a playlist's
placement rule) to another time in its hour, with the same meaning as a Fill pin:
fixed until the end of the hour, kept on rebuilds, unpinned with the lock.

The hour must keep the same number of Rule tracks. A pin stored like a Fill pin
would add a one-time insert while the builder still placed the rule's slot, so the
hour would get one extra jingle. A Rule pin therefore takes over one slot of its
rule in that hour.

## What exists (checked in code)

- Slots are generated per clock playlist (`internal/rotation/build.go`): `spread`
  gives N soft slots (loop index k, `:544-547`), `middle` one (`:529`), `start`
  one (`:147-151`), `end` one (`:376-380`), `start_end` one of each.
- A slot is identified by clock playlist and k. k is not stored.
  `scheduled_items.playlist_id` (migration 0019) names the playlist. Windows of the
  same playlist do not overlap, so the row's offset identifies k.
- A pin is placed as an insert anchor. Soft slots already keep away from anchors
  (`:575`, `:633-635`): a neighbouring slot moves, or is skipped with
  `rule_slot_skipped`.
- `fillAllowed` already excludes the tracks of the hour's one-time inserts. Rule
  draws use `allowedOf` (`:400`, `:479`, `:637`) and do not.
- A refused anchor is detected late (`placeAnchor`, `:449`), after the slots were
  generated.

## Backend

### Migration

A new migration (next free number, expected `0020_hour_inserts_rule_claim.up.sql`)
adds to `hour_inserts`:

- `rule_playlist_id bigint NULL`, FK to playlists, `ON DELETE SET NULL`;
- `rule_origin_ms integer NULL`, the row's offset when it was pinned, with a range
  check;
- `ck_hour_inserts_rule_fixed`: `rule_playlist_id IS NULL OR placement = 'fixed'`.

No index: the columns are not joined. Apply with `make migrate`, then run
`make test-integration`.

An explicit claim is required. Inferring the slot from playlist membership would
make a manual one-time insert of a jingle consume a slot, and is ambiguous for a
track in several rule playlists.

### Behaviour

1. `POST /api/track-plan/pins` accepts a planned row of `source = 'fill'` or
   `'rule'`. For a Rule row it stores the claim (`rule_playlist_id`,
   `rule_origin_ms`). A Rule row with `playlist_id` NULL is refused with
   `not_movable`. `PinFill` is renamed `Pin`.
2. The builder removes the claimed slot and keeps the others:
   - `spread`: the slot whose window holds the origin, else the nearest. The
     remaining N-1 keep their windows: the random draw for the removed slot is
     still made, and claiming pins are left out of `busy` (`:497`), so `total` does
     not shift. A changed start or end track can still move them slightly.
   - `middle`: the slot is removed.
   - `start`, `end`: the block is one track shorter. One random draw is burnt so the
     targets of later playlists hold.
   - `start_end`: an origin before 30:00 claims the start slot, otherwise the end
     slot.
3. Claims are processed in id order, so the build stays deterministic for the same
   inputs.
4. Rule draws avoid the track of a claiming pin. If it is the only allowed track,
   it may be used.
5. A pin the builder refuses gives its slot back in the same build. `Build` retries
   without the refused pin and merges `Refused`. Without this the hour is stored one
   jingle short and the hash hides it.
6. Moving a pin (`PATCH /api/track-plan/inserts/{id}`) keeps its claim and origin.
   Unpinning (`DELETE`) removes the claim, and the slot is generated again at its
   seeded window, possibly with another track.
7. A claim whose playlist left the clock or is now `fill` claims nothing, so the
   hour has one track more. This raises a new warning (name to be chosen, in
   `WarningCode` and `errcodes` if needed) so the operator can see why.
8. The input hash (`internal/rotation/inputs.go:85`) gains the claim text only when
   a claim is set, so the stored hashes of existing inserts stay valid.

### Where the code goes

- `internal/rotation/rotation.go` (`Insert`), `service.go` (`PlanItemRef` gains
  `PlaylistID`, `OffsetMS`), `inserts.go`, `inputs.go`, `build.go` (claims in
  `index`, `endBlock`, `placeStartRule`, `softAnchors`, and the retry).
- `internal/repository/rotation/inserts.go`, `plan.go` (`PlanItem` also selects
  the playlist and the start). SQL only there.
- `handlers/rotation.go`, DTO doc in `dto/rotation.go`, comments in `errcodes.go`.
  `make openapi`, `make openapi-lint`.

### Tests

Unit: N-1 slots remain and the others' targets stay; the hour has the same number
of Rule tracks as before the pin; a manual insert of the same track consumes
nothing; each placement rule, and both ends of `start_end`; a refused pin restores
the slot; unpin restores the slot; the stale-claim warning; hash stable without a
claim; determinism. Integration: the round trip of a claim through
`hour_inserts`, and a pinned Rule track keeps its offset after `RebuildRange`.
`make test`, `make lint`, `make test-integration`.

## Frontend (Track plan page)

- `src/app/features/track-plan/track-plan-move.ts:24`: `rule` rows become movable.
  Update `track-plan-move.spec.ts:152`.
- Update the unpin warning in `en.json:492` and the help in
  `assets/help/en/track-plan.json:19,44`. Regenerate the API client.
- Add the operator text for the new warning `pin_claim_stale`, next to the other
  track plan warnings.

## Documents to update with this work

- `track-pin-plan.md`: the goal's "rule items and clock anchors are not moved" and
  "Out of scope".
- `tech-design.md`: `hour_inserts` (§ data model) and the insert section §7.2.

## Out of scope

- Template inserts (`source = 'insert'`). Their time is the clock template's
  contract, and a pin away from the template offset does not trigger replacement.
- Moving a track to another hour.

## Decisions (confirmed by the developer)

1. Rule rows may be moved, which lifts the limit in `track-pin-plan.md`.
2. `spread`: the other jingles keep their places, they are not spread again.
3. Rule draws avoid the pinned track, unless it is the only allowed one.
4. A stale claim after a clock edit raises a warning.
5. A Rule row with no `playlist_id` stays refused (`not_movable`). Whether any such
   rows still exist was not checked; the plan is rebuilt on a rolling window.

## Done when

- An operator can drag a Rule row in an open hour. The hour keeps the same number of
  Rule tracks, and the track stays in place after `Rebuild these dates`.
- Unpinning brings the rule's slot back.
- A refused move changes nothing and leaves the hour with all its Rule tracks.
- The migration is applied, and the tests above pass.
