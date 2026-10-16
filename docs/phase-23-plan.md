# Phase 23 Plan — Playlist Placement Rules, Clock Inserts, No Filler, Separation in Play Rules (radio-atlantida-backend)

Status: Approved by the operator (2026-10-17); not started
Based on: `docs/base.md`, `tech-design.md` (§4, §6.0, §6.2–§6.5, §7, §7.1, §7.2, §7.3), `docs/phase-7-plan.md` (clocks), `docs/phase-8-plan.md` (rotation engine), `docs/phase-10-plan.md` (hour inserts, reschedule), `docs/phase-22-plan.md` (on-air length), frontend `docs/f21-playlist-rules-clock-plan.md`
Last updated: 2026-10-17

## 1. Goal

Today a clock is a list of elements and every element is one track with a nominal duration and a tolerance. A music hour needs about 17 elements, and a playlist of 5–7 minute tracks does not fit an element of 3:30 ± 30 s. Short segments need filler, a blocking dialog, pools, and reminders.

The operator's model replaces it:

1. **A clock is a set of playlists plus timed inserts.** The planner lays out the hour by itself.
2. **Placement rules live on the playlist** (fill the hour, N spread over the hour, at the start, at the end, at the start and the end, in the middle), and a clock may override them.
3. **No nominal duration and no tolerance** on clock content; tracks of any length are used.
4. **Timed inserts** at `mm:ss` with a start tolerance: in the clock template, or once in the plan of a single hour (today's news). The hour is rebuilt around them.
5. **No filler.** The fill playlists fill the hour.
6. **No separation rules.** How often a track or an artist may repeat is a play rule attached to the playlist, like every other limit.

## 2. Decisions (2026-10-17, operator)

1. **Rules on the playlist, overridable in the clock.** `playlists.clock_rule` / `clock_count` hold the default; a clock's playlist row may override both.
2. **The last track before a hard point** (an insert window, an `end` track, the end of the hour) is the best fit to the remaining time; when no track lands in the window, the crossing rule of §7 applies: the track is cut at the target and faded out (`liquidsoap.fade_out_ms`).
3. **Inserts are separate items, one track each**, with seconds. A news block is two or three inserts. Inserts whose windows touch play back to back.
4. **One-time inserts survive every rebuild** of their hour; they live in `hour_inserts` (today: rescheduled missed items), not in the plan.
5. **No manual reordering of planned items** in this phase.
6. **Filler is removed completely**: clock elements, nominal durations and tolerances, filler modes, the clock's default filler mode, the global fallback, the filler pool, the save dialog (`save-with-filler`, `save-incomplete`), filler reminders, the `incomplete` status.
7. **`middle`** is a track that starts at about 30:00, within ±5 minutes.
8. **Existing clocks are deleted**, not converted: the station is in development. The migration removes clocks, grid slots, and the plan (§3.1).
9. **Weights of fill playlists**: each fill playlist row of a clock may have `fill_weight` (1–100). Either all fill rows have a weight or none. With weights, every fill track first draws a playlist by weight, then a track from it. Without weights, every fill playlist has weight 1 (the playlist is drawn first, so a small playlist plays as often as a large one).

10. **Separation rules are removed; their meaning moves into play rules** (operator, 2026-10-17). A play rule gains `artist_min_interval_minutes` next to `min_interval_minutes`, and both are applied to playlists or tracks as today. "Music rotation: a track at most once per 72 h, an artist at most once per 2 h" is one play rule attached to the music playlists; "Jingles: at least 20 minutes apart, at most 2 per hour" another. Every track is checked the same way, by the rules of the track and of its playlists. The placement rule says where and how many; the play rule says how often a track may repeat.
11. **Intervals are capped at the play log retention** (`retention.play_log_days`), as separation was: a longer interval would be checked against history that is gone.
12. **The migration keeps today's rotation**: it creates a play rule "Music rotation" from the current `separation_rules` values and attaches it to every playlist of the system type `music`, so the station does not lose its separation silently.
13. **A fill playlist without a minimum interval** (no rule of it sets `min_interval_minutes`) gets a dry-check warning on the clock save: its tracks may repeat within the hour.

## 3. Work

### 3.1 Data

Migration `0013_clock_playlists_inserts.up.sql` (forward-only). It deletes data, which is acceptable only because no release has aired (decision 8); the release notes say so.

```sql
-- Data reset (decision 8). The backend is stopped while migrations run.
UPDATE play_log SET resolution = 'open', resolved_item_id = NULL, resolved_at = NULL
  WHERE resolution = 'rescheduled';             -- their inserts are deleted below
DELETE FROM hour_inserts;
DELETE FROM scheduled_items;                    -- planned, queued, replaced; aired and missed live in play_log
DELETE FROM plan_hours;
DELETE FROM filler_requests;
UPDATE play_log SET clock_id = NULL, grid_slot_id = NULL;
DELETE FROM grid_slots;
DELETE FROM clocks;                             -- cascades elements, element playlists, pools

-- Separation moves into play rules (decisions 10, 12).
ALTER TABLE play_rules
  ADD COLUMN artist_min_interval_minutes integer,
  ADD CONSTRAINT ck_play_rules_artist_min_interval_minutes_positive CHECK (artist_min_interval_minutes > 0),
  DROP CONSTRAINT ck_play_rules_any_constraint,
  ADD CONSTRAINT ck_play_rules_any_constraint CHECK (
    window_from IS NOT NULL OR weekdays IS NOT NULL OR min_interval_minutes IS NOT NULL
    OR artist_min_interval_minutes IS NOT NULL OR max_per_hour IS NOT NULL);
INSERT INTO play_rules (name, min_interval_minutes, artist_min_interval_minutes, notes)
  SELECT 'Music rotation',
         (SELECT 60 * max(min_interval_hours) FROM separation_rules WHERE scope = 'track'),
         (SELECT 60 * max(min_interval_hours) FROM separation_rules WHERE scope = 'artist'),
         'Created from the separation rules (phase 23).'
  WHERE NOT EXISTS (SELECT 1 FROM play_rules WHERE name = 'Music rotation');
INSERT INTO playlist_play_rules (playlist_id, rule_id)
  SELECT p.id, r.id FROM playlists p
    JOIN playlist_types t ON t.id = p.type_id AND t.code = 'music'
    JOIN play_rules r ON r.name = 'Music rotation'
  ON CONFLICT DO NOTHING;
DROP TABLE separation_rules;

DROP TABLE clock_filler_pool, filler_requests, clock_element_playlists, clock_elements;
ALTER TABLE plan_hours DROP COLUMN filler_request_id;
ALTER TABLE clocks
  DROP COLUMN status, DROP COLUMN default_filler_mode,
  DROP COLUMN default_filler_playlist_id, DROP COLUMN default_filler_artist_id;

ALTER TABLE playlists
  ADD COLUMN clock_rule  text     NOT NULL DEFAULT 'fill',
  ADD COLUMN clock_count smallint,
  ADD CONSTRAINT ck_playlists_clock_rule CHECK (clock_rule IN ('fill','spread','start','end','start_end','middle')),
  ADD CONSTRAINT ck_playlists_clock_count CHECK (
    (clock_rule = 'spread') = (clock_count IS NOT NULL) AND (clock_count IS NULL OR clock_count BETWEEN 1 AND 12));

CREATE TABLE clock_playlists (
  clock_id    bigint   NOT NULL,   -- fk ON DELETE CASCADE
  playlist_id bigint   NOT NULL,   -- fk ON DELETE RESTRICT (a playlist in a clock cannot be deleted)
  position    integer  NOT NULL,   -- order of start / end rule tracks of several playlists
  clock_rule  text,                -- null: the playlist's own rule
  clock_count smallint,            -- spread only, 1..12
  fill_weight smallint,            -- 1..100; checked against the effective rule by the service
  -- pk (clock_id, playlist_id), uq (clock_id, position), ix (playlist_id), checks as on playlists
);

CREATE TABLE clock_inserts (
  id           bigint GENERATED ALWAYS AS IDENTITY,
  clock_id     bigint  NOT NULL,                -- fk ON DELETE CASCADE
  offset_ms    integer NOT NULL,                -- 0..3 599 000, whole seconds (offset_ms % 1000 = 0)
  tolerance_ms integer NOT NULL DEFAULT 60000,  -- 0..600 000
  track_id     bigint,                          -- fk ON DELETE RESTRICT
  playlist_id  bigint,                          -- fk ON DELETE RESTRICT
  -- pk, uq (clock_id, offset_ms), check exactly one of track_id / playlist_id, partial ix on track_id and playlist_id
);

ALTER TABLE hour_inserts
  DROP CONSTRAINT ck_hour_inserts_fixed_minute, DROP COLUMN fixed_minute,
  ADD COLUMN offset_ms integer,                 -- whole seconds; set only for 'fixed'
  ADD CONSTRAINT ck_hour_inserts_offset_ms CHECK (
    (placement = 'fixed') = (offset_ms IS NOT NULL)
    AND (offset_ms IS NULL OR (offset_ms BETWEEN 0 AND 3599000 AND offset_ms % 1000 = 0)));
ALTER TABLE hour_inserts RENAME COLUMN fixed_tolerance_ms TO tolerance_ms;

ALTER TABLE scheduled_items
  DROP CONSTRAINT ck_scheduled_items_filler_pool, DROP COLUMN filler_pool_id, DROP COLUMN clock_element_id,
  ADD COLUMN clock_insert_id bigint,            -- fk ON DELETE SET NULL: the template insert of an 'insert' item
  ADD COLUMN hour_insert_id  bigint,            -- fk ON DELETE SET NULL: the one-time insert of an 'hour_insert' or 'reschedule' item
  DROP CONSTRAINT ck_scheduled_items_source,
  ADD CONSTRAINT ck_scheduled_items_source CHECK (source IN ('fill','rule','insert','hour_insert','reschedule','general'));
```

- Settings rows: delete `filler.fallback`, `filler.reminder_deadline_minutes`, `rotation.default_duration_tolerance_ms`, `rotation.max_relaxed_tolerance_ms`; rename `rotation.min_filler_gap_ms` to `rotation.min_gap_ms`. `rotation.default_fixed_tolerance_ms` stays (the default tolerance of a new insert).
- If a play rule named "Music rotation" already exists, the migration leaves it as it is and attaches it; the column and type names above are checked against the schema when the migration is written.
- **Apply the migration** to the dev database right after writing it: `make migrate`; check with `psql` that the tables, columns, and constraints exist, that `clocks` and `grid_slots` are empty, that `separation_rules` is gone, and that "Music rotation" is attached to the music playlists; then `make test-integration` (the migration test runs every migration on a fresh database). Rebuild test clocks and the grid in the panel afterwards.

### 3.2 Clocks (`internal/clocks`)

- `Clock`: `ID`, `Name`, `UpdatedAt`, `Playlists []ClockPlaylist` (`PlaylistID`, `Rule *Rule`, `Count *int`, `FillWeight *int`, in position order), `Inserts []Insert` (`ID`, `OffsetMS`, `ToleranceMS`, `TrackID *uint64`, `PlaylistID *uint64`, by offset).
- `Rule` (`fill`, `spread`, `start`, `end`, `start_end`, `middle`) and `EffectiveRule(cp, playlist)`: the override, else the playlist's rule.
- **Validation** (field errors, `validation.Error`):
  - `name` as today;
  - `playlists`: at least one row whose effective rule is `fill` (`fill_playlist_required`); no duplicate playlist; `playlists[i].clock_count` only and required for `spread`, 1–12; `playlists[i].fill_weight` only on fill rows, 1–100; all fill rows weighted or none (`fill_weights_partial`, on the first row without a weight);
  - `inserts[i].offset_ms` 0–3 599 000, whole seconds, unique in the clock; `inserts[i].tolerance_ms` 0–600 000; exactly one of `track_id`, `playlist_id`; unknown references (`ErrUnknownTrack`, `ErrUnknownPlaylist`) as today.
- `Arrange`, `LayoutOf`, segments, holders, fixed replacement, `WithInsert`, filler checks, `SaveWithFiller`, `SaveIncomplete`, `Pool`, `SetPool` are deleted. The save is one step: validate, store in a transaction (replace the playlist and insert rows), return the result (§3.7) and start a rebuild of the clock's hours as today.
- `Delete`: refused while grid slots assign the clock (as today); the history check stays (`play_log.clock_id`).
- **Dry check** (`check.go`, warnings, never blocking): a playlist without a playable track (`playlist_no_tracks`); fewer fill tracks than the minimum interval of their rules needs over the clock's grid hours (`fill_few_tracks`, the current `fewTracks` count with the interval of the play rules instead of separation); a fill playlist none of whose rules sets `min_interval_minutes` (`fill_no_interval_rule`, decision 13); a rule or insert playlist whose tracks are all blocked by play rule windows in some grid hours of the clock (`rule_blocked_by_play_rules`, the current `blockedHours` with the playlist's tracks); and the warnings of the sample build (§3.7).

### 3.3 Building an hour (`internal/rotation/build.go`)

`Build` stays pure and seeded. Input: the clock (effective rules resolved by the service), the template inserts, the one-time inserts (`hour_inserts`), the library, history, settings (`MinGapMS`), seed. Output: items in time order, warnings, the one-time inserts that could not be placed. `Separation` leaves the input; the history window is the longest track or artist interval of the play rules.

1. **Anchors.** Template and one-time inserts are merged by target offset. A one-time insert at exactly the offset of a template insert replaces it for that hour (warning `insert_replaced`). Each insert has a window `[target − tol, target + tol]`, clipped to the hour. A playlist insert draws its track now (play rules); none allowed → skipped with `insert_skipped`.
2. **Chains.** Inserts are placed in time order. An insert starts when the content before it ends, but not before its window opens; it may start up to `target + tol`. If the previous insert ends later than that, the later insert does not fit: a template insert is skipped (`insert_skipped`), a one-time insert is refused (the build reports it; the add operation fails, a rebuild drops it with `hour_insert_dropped` and reopens a rescheduled missed item as today).
3. **Gaps.** The time between anchors is free. A gap shorter than `rotation.min_gap_ms` is not filled: the next insert starts earlier within its window, else the gap stays.
4. **Rule slots.** For each playlist with a non-fill rule, in clock position order:
   - `start`: the first track of the hour (after an insert that starts at 0:00);
   - `end`: a hard anchor that ends at 60:00 (the fill before it best-fits to its start);
   - `start_end`: both;
   - `middle`: a soft anchor with target 30:00 and window 25:00–35:00;
   - `spread` N: the free time is cut into N equal parts; each gets a soft anchor at a random point of the middle half of its part, window ± a quarter of the part.
   A soft anchor is reached by a best-fit fill track, never by cutting. If it would start next to an insert or another rule track, it moves one fill track later inside its window; outside the window it is skipped (`rule_slot_skipped`). A rule track is a random allowed track of its playlist.
5. **Fill.** Before each fill track a playlist is drawn by weight (decision 9), then a random allowed track of it; a drawn playlist with no allowed track is left out of the draw for this track. When the drawn track would pass the opening of the next hard anchor's window, the best fit among the allowed tracks of all fill playlists is chosen to end inside the window, closest to the target; if none lands in it, the crossing rule cuts the track at the target with fade-out (decision 2). For a soft anchor the same best fit is used without the cut.
6. **Eligibility.** Every track passes the play rules of the track and of all its playlists (never relaxed): window, weekdays, plays per hour, the track interval, and the artist interval (no start of any track of the same artist within `artist_min_interval_minutes`, aired and planned plays counted, missed not). The separation checks of `eligibility.go` are replaced by the artist interval.
7. **Emergency.** No allowed fill track in any fill playlist → a track of the whole library that passes the rules (`fill_general_pool`, source `general`); none → the gap is left for the emergency source (`fill_emergency`). The tolerance relaxation steps are deleted with the tolerances.
8. Hours that have started are never rebuilt (unchanged). The seed is the hour start, as today.

Warning codes after the change: `insert_replaced`, `insert_skipped`, `hour_insert_dropped`, `rule_slot_skipped`, `rule_blocked_by_play_rules`, `fill_general_pool`, `fill_emergency`, `gap_unfilled` (a gap no track can serve), `reschedule_dropped` (kept for the missed item). The filler and element codes are deleted.

### 3.4 One-time inserts (`internal/rotation`)

- `Insert` (`hour_inserts`): `Placement` (`start`, `end`, `fixed`), `OffsetMS *int`, `ToleranceMS`, `TrackID`, `MissedID *uint64`. A `start` insert is the first item after the template inserts at 0:00; an `end` insert ends at 60:00 like an `end` rule track (before it, if both exist).
- `Service.AddInsert(ctx, hourStart, offsetMS, toleranceMS?, trackID)`: the request checks of `Reschedule` (`checkTarget`: a station hour start, not frozen, inside the horizon; the grid assigns a clock), then an operation `hour_insert` stores the insert, rebuilds the hour in the same transaction, and fails without a change when the track cannot be placed whole (its rules at its start, the chain, the hour end).
- `Service.RemoveInsert(ctx, id)`: refused for a frozen hour (`hour_started`); an operation `hour_insert` deletes it and rebuilds the hour. Removing the insert of a rescheduled missed item reopens the missed item.
- `Reschedule` keeps its flow and uses `offset_ms` (whole seconds) instead of `fixed_minute`; `clocks.WithInsert` is replaced by the chain check of §3.3.
- The pinned build (`applyInserts`, `pinned`, `settleInserts`) is simplified: inserts are anchors, not elements inserted into a clock.

### 3.5 Removing filler

- `rotation`: `FillerRequest`, `NewRequest`, `RequestStatus`, `Waiting`, `DefaultFiller`, `Pool`, `fill`, `fillerTrack`, `poolTrack`, `resolveDue`, `closeDue`, `Answer`, `PoolRefilled`, `Requests`, the reminder creation in `buildTx`, `PlanHour.FillerRequestID`; the plan job (`KindTrackPlan`) only syncs.
- `repository/rotation/requests.go`, the pool reads, the filler parts of `plan.go` and `library.go`; `repository/clocks` element, pool, and filler columns.
- `settings`: group `filler`, `FillerFallback`, the deleted keys (§3.1).
- `ws`: `filler_request.created`; the warning enum of `track_plan.warning` (§3.3).
- `errcodes`: `filler_request_not_found`, `filler_request_closed`, `filler_request_not_answerable`, the filler field codes; new codes in §3.7. Published codes are never renamed, so the removed ones are listed in the release notes.
- `library`: playlist, artist, and track in-use refusals no longer name filler; a playlist is in use by `clock_playlists` and `clock_inserts`, a track by `clock_inserts` and `hour_inserts`, an artist only by tracks.
- `logging`: the component `filler` is removed from the list (§10 of the design) and from the Grafana Logs dashboard filter (`dev/grafana`).

### 3.6 Play rules instead of separation

- `playrules.Rule` gains `ArtistMinIntervalMinutes *int`; validation: ≥ 1; both intervals at most `retention.play_log_days` × 1 440 minutes (field error `interval_beyond_retention`, decision 11); "at least one constraint" counts it. The cap function replaces `separationLimit` in `cmd/backend/serve.go`.
- `repository/playrules` reads and writes the column; the rotation library snapshot (`repository/rotation/library.go`) carries it in `playrules.RuleInput`.
- Deleted: `internal/separation`, `internal/repository/separation`, the separation handlers and DTOs (`handlers/rules.go`, `dto/rules.go`), `errcodes.SeparationRuleNotFound`, `rotation.Separation` and the separation reads of `repository/rotation` and `repository/clocks/check.go`, the wiring in `serve.go`. Comments that name separation (retention, artists, playout) are updated.
- The retention setting's description says the play log must cover the longest interval of the play rules; lowering it below an existing interval is refused (`interval_beyond_retention` on `retention.play_log_days`).

### 3.7 API

| Endpoint | Change |
|----------|--------|
| `GET/POST/PUT /api/playlists…` | `clock_rule` (default `fill`), `clock_count` (spread only). `PUT` returns `build_operation_id` when the rule changed and clocks use the playlist (their `updated_at` is touched in the same transaction, so their hours are stale) |
| `GET /api/clocks` | summary: `id`, `name`, `playlist_count`, `insert_count`, `updated_at`; no `status` |
| `GET /api/clocks/{id}` | `playlists[]` (`playlist_id`, `position`, `clock_rule?`, `clock_count?`, `fill_weight?`, and read-only `effective_rule`, `effective_count`), `inserts[]` (`id`, `offset_ms`, `tolerance_ms`, `track_id?`, `playlist_id?`) |
| `POST /api/clocks`, `PUT /api/clocks/{id}` | body `name`, `playlists[]`, `inserts[]` → `ClockSaveResult`: `clock`, `sample` (`hour_start_utc`, `items[]`: `offset_ms`, `length_ms`, `track_id`, `title`, `artist`, `source`, `playlist_id?`, `insert_index?`, `truncated`), `warnings[]` (`code`, `message`, `playlist_index?`, `insert_index?`, `hours?`), `build_operation_id?` |
| `DELETE /api/clocks/{id}` | unchanged |
| `/api/clocks/{id}/save-with-filler`, `/save-incomplete`, `/filler-pool`, `/api/filler-requests…` | deleted |
| `GET /api/track-plan` | hours gain `inserts[]` (`id`, `placement`, `offset_ms?`, `tolerance_ms`, `track_id`, `missed_id?`); items lose `element_id`, gain `insert_id?` (template) and `hour_insert_id?`; `source` values of §3.1; no filler request fields |
| `POST /api/track-plan/inserts` | new: `hour_start_utc`, `offset_ms`, `tolerance_ms?`, `track_id` → `accepted` + operation ID (`hour_insert`). 422: `hour_start_utc` (`hour_started`, not an hour start, beyond the plan, no clock), `offset_ms`, `track_id` (unknown, no duration) |
| `DELETE /api/track-plan/inserts/{id}` | new → `accepted` + operation ID; 404 `hour_insert_not_found`; 409 `hour_started` |
| `POST /api/missed-items/{id}/reschedule` | `offset_ms` instead of `fixed_minute` |
| `GET/POST/PUT /api/play-rules…` | `artist_min_interval_minutes`; field code `interval_beyond_retention` on either interval |
| `/api/separation-rules…` | deleted |

- **Sample**: the save builds, without storing, the next grid hour of the clock inside the plan horizon, or the next station hour when the grid does not use the clock yet, with the real history (§3.3). It shows a typical hour; the planned hours may differ by seed and history.
- New codes: problem `hour_insert_not_found`; field `fill_playlist_required`, `interval_beyond_retention`, `fill_weights_partial`, `clock_count_required`, `clock_count_not_allowed`, `fill_weight_not_allowed`, `insert_offset_taken`, `insert_source_required`; operation failure reasons for `hour_insert` (`insert_does_not_fit`, `insert_blocked`).
- Operation kind `hour_insert` in `operations.Kinds()` with `hour_insert.completed` / `hour_insert.failed`.

### 3.8 Documents

The requirements and the design change, so they are updated in this phase, before the code, and copied verbatim to the frontend repository:

- `docs/base.md`, "Clock Rules & Rotation": the text of Appendix A of frontend `docs/f21-playlist-rules-clock-plan.md`; "Artist/Track Separation" becomes part of "Play Rules" (a rule may also set a minimum interval between tracks of the same artist; the default rule "Music rotation" is a track at most once per 72 hours); "Play Rules": "A rule limits when a track may play; how often a playlist appears in the hour is its placement rule"; "Rules are checked … including for inserts and rescheduled items".
- `docs/tech-design.md`: §1 and §2 (filler service removed), §4 (tables of §3.1; `separation_rules` removed, `play_rules` column), §6.0 (one-step save, sample), §6.1 (help texts), §6.2 (clock editor and playlist rule inputs), §6.3 (settings; separation values removed from the Rotation group), §6.4 (endpoints of §3.7), §6.5 (events and operation kind), §7 (building an hour, §3.3; "Filler" and "Filler reminders" removed), §7.2 rewritten as "Inserts and anchors", §7.3 ("a play rule limits, a placement rule demands"; the artist interval; separation folded in, every "separation and play rules" in §7 becomes "play rules"), §10 (log components), the decision list (14 kept for the crossing rule; 24 replaced).
- `CLAUDE.md`: project context (Clocks, Rotation), layout lines of `clocks` and `rotation`, Phase 23 in the reading list.

## 4. Tests

- **Clocks (unit):** validation of every field of §3.2, including `fill_weights_partial`, effective rule with and without override, duplicate offsets, track xor playlist.
- **Play rules (unit):** `artist_min_interval_minutes` validated (≥ 1, ≤ retention); an interval longer than `retention.play_log_days` refused on its field; a rule with only an artist interval is valid.
- **Build (unit, seeded):**
  - an hour from one fill playlist of 6-minute tracks ends at 60:00 with a best-fit or cut last track;
  - `spread` 2: two rule tracks in different halves of the hour, never adjacent to each other or to an insert;
  - `start`, `end`, `start_end`, `middle` (starts within 25:00–35:00; skipped with a warning when an insert covers the window);
  - weights 30 / 70 over 1 000 seeded hours: the share of the first playlist is 30 % ± 3; without weights, a playlist of 5 tracks and one of 500 get about half each;
  - chained inserts 29:40 (20 s), 30:00, and a third: back to back, music before them ends in the window or is cut at the target;
  - a one-time insert at the offset of a template insert replaces it; a chain that runs past a window skips a template insert and refuses a one-time one;
  - a gap under `min_gap_ms` is not filled;
  - the artist interval blocks a second track of the same artist from any playlist; the track interval of a jingle rule lets jingles repeat after 20 minutes;
  - emergency: blocked fill playlists → general pool → gap left, with their warnings;
  - same input and seed → same plan.
- **Rotation service (integration):** add and remove a one-time insert (operation, rebuild, frozen hour refused); a grid change keeps the insert; a playlist rule change rebuilds the hours of its clocks.
- **Repository (integration):** play rules with the artist interval round trip; clock playlists and inserts round trip; a playlist in a clock and a track in an insert cannot be deleted (in-use refusal with `used_by`).
- **Migration (integration):** `0013` on a database with clocks, elements, pools, requests, grid, plan, and a rescheduled missed item: everything listed is deleted, the missed item is open again, the play log keeps its rows; "Music rotation" carries the separation values in minutes and is attached to the music playlists; `separation_rules` is gone.
- **API:** `make openapi-lint`; handler tests for the new endpoints and refusals.

## 5. Order of work

1. `docs/base.md`, `docs/tech-design.md`, `CLAUDE.md` (§3.8); copy both documents to the frontend repository.
2. Migration `0013` and `make migrate` (§3.1); repositories.
3. `playrules` (§3.6), then `clocks` (§3.2), then `rotation` build (§3.3) with its unit tests, then inserts (§3.4), then removal (§3.5).
4. API (§3.7), `make openapi`; the frontend runs `npm run api:sync` and starts F21 §5.2–§5.5.
5. `make lint`, `make test`, `make test-integration`, `make openapi-lint`; release backend and frontend of the same version.

## 6. Done when

- A clock of "Music" (fill) and "Jingles" (2 × spread) and no inserts plans full hours with two jingles apart from each other; 6-minute tracks are used.
- Inserts at 29:40 (jingle), 30:00 (news playlist), and a jingle after it play back to back; the music before them ends inside the window or fades out at the target.
- The operator adds today's news at 18:30:00 through `POST /api/track-plan/inserts`; the hour is rebuilt around it; a later grid change keeps it.
- No filler or separation endpoint, table, setting, event, or code remains; "Music rotation" keeps the old separation values.
- `make lint`, `make test`, `make test-integration`, `make openapi-lint` pass.

## 7. Out of scope

- Converting existing clocks (decision 8).
- Manual reordering of planned items (decision 5).
- Placement rules on single tracks; inserts as multi-track blocks.
- Changing the playout path: items still reach Liquidsoap as today (§8).
