# Phase 17 Plan — Air Window (radio-atlantida-backend)

Status: Implemented
Based on: `tech-design.md` (§4, §4.1, §6.3, §7.1, §8.3), `docs/phase-9-plan.md` (lookahead, now playing), `docs/phase-10-plan.md` (played-tracks list, missed items), frontend `docs/f15-air-window-plan.md`
Last updated: 2026-10-15

## 1. Goal

One list of the air as the manager needs it now: the last items that were due (aired, truncated, missed), the item on air, and the next planned items. It is one endpoint, so the panel does not combine three sources, and it stays correct at the hour switch and during an outage.

## 2. Decisions (2026-10-15)

1. **One endpoint**, `GET /api/air-window`, with no query parameters. The counts come from settings (§3), so the API and the panel always agree.
2. **Rows in air order**, top to bottom: `played` rows oldest first, then one `now` row, then `queued` rows in planned order. The list is not paginated. It has at most `air.played_count + 1 + air.queued_count` rows (101 at the maximum settings).
3. **Played block.** The last N rows of `play_log` with status `aired`, `truncated` or `missed`, ordered by the moment they were due, `COALESCE(started_at, planned_start_utc)`, descending, then reversed for display. A missed row has no `started_at`, so its moment is `planned_start_utc`. Missed rows are included, with their `resolution` (`open`, `rescheduled`, `dismissed`), because the manager must see what did not air. The item on air is in `play_log` already (playout moves an item there when it starts), so its row is left out of the played block: the store reads `air.played_count + 1` rows and the one whose `scheduled_item_id` is the now row's item is dropped.
4. **Now row.** The track on air, taken from the playout state (`playout.Service.NowPlaying`). It is absent when the state has no track: between tracks, or when Liquidsoap reports nothing. Its end is `started_at + out_ms`; null for an emergency track without a duration.
5. **Control link.** The response carries `control_link_connected` (from the playout state). When it is false, the now row is absent because the state is unknown, not because nothing plays, even if the playout state still holds a track. The queued boundary is then the current time. The panel shows that difference (frontend plan §2.8).
6. **Queued block.** The next M rows of `scheduled_items` with status `planned` or `queued` and `planned_start_utc >= boundary`, ordered by `planned_start_utc` ascending. The boundary is the start of the now row when there is one, otherwise the current time. Items whose start has passed but which did not air are not shown here: the outage code owns them (phase 10). `in_liquidsoap` is true for `queued` rows (already pushed) and for the now row, false for `planned` and played rows.
7. **End times.** Aired and truncated rows: `started_at + actual_duration_ms` (the cut that aired). Missed rows: `planned_start_utc + planned_out_ms`. Queued rows: `planned_start_utc + planned_out_ms - planned_in_ms` (the planned end, as playout computes it; the in point is 0 for every item the rotation builds). The end is the station-time end the manager reads on the list.
8. **One row shape** for every kind: `kind` (`played`, `now`, `queued`), `status` (`aired`, `truncated`, `missed`, `now`, `queued`, `planned`), `item_id` (the `play_log` or `scheduled_items` ID; null for a now row without a plan item, such as an emergency track), `track_id`, `title`, `artist_name` (nullable), `source` (`plan`, `emergency`, `live`: the play log's `auto` is `plan`, the source of a now row is playout's, queued rows are `plan`), `start_utc`, `end_utc` (nullable, see 4), `truncated`, `resolution` (nullable, missed rows only), `outage_id` (nullable, missed rows only), `in_liquidsoap`.
9. **Response.** `generated_at` (UTC), `control_link_connected` (bool), `rows`.
10. **Playout down is not an error.** With the control link down the endpoint returns 200, no now row, and the played and queued rows from the database.
11. **Read only.** No writes, no operation. The panel refreshes on events (frontend plan §2.4); one event was added for the end of a track (§9).
12. **Hot path in pgx.** Two indexed queries in the repository, each with its `EXPLAIN ANALYZE` in a doc comment (`TestExplain`). No migration: the played block uses `ix_play_log_sort_time`, the queued block `ix_scheduled_items_planned_start_utc` (items before the boundary are skipped by the index condition, and nearly every item after it is `planned` or `queued`). A `(status, planned_start_utc)` index was planned as migration `0010`, but PostgreSQL 17 does not choose it for this query, so it would be a speculative index (CLAUDE.md) and was not added.

## 3. Settings

New group `air` (settings registry, `internal/settings/definitions.go`, apply `immediate`):

| Key | Type | Default | Allowed | Label (admin panel) |
|-----|------|---------|---------|---------------------|
| `air.played_count` | integer | 10 | 1 to 50 | Played items shown |
| `air.queued_count` | integer | 9 | 0 to 50 | Queued items shown |

A change takes effect on the next request. No rebuild, no restart.

## 4. Files

| File | Change |
|------|--------|
| `internal/settings/definitions.go` | Group `air` with the two keys (also in the group enum of `dto/settings.go`) |
| `internal/playout/playout.go` | `Service.NowPlaying()`: the track on air, without a database read |
| `internal/airwindow/` | `Service.Window(ctx)`; pure `Assemble(played, now, queued, nowInfo)` that builds the rows; `Store` and playout-state interfaces. No SQL |
| `internal/repository/airwindow/` | pgx: `Played(ctx, limit)`, `Queued(ctx, from, limit)` |
| `internal/httpapi/dto/airwindow.go` | Response DTOs |
| `internal/httpapi/handlers/airwindow.go` | `GET /api/air-window`, calls the service only |
| `internal/httpapi/router.go`, `handlers/handlers.go` | Register the operation |
| `cmd/backend/serve.go` | Wire the service; `airWindowSettings` reads the counts on every request |
| `docs/tech-design.md` | Endpoint line in §6, settings group in §6.3 |

The architecture test keeps SQL in `repository/airwindow` and the handler away from repositories.

## 5. Tests

- **Unit, `Assemble`:** order (played oldest first, now, queued ascending); fewer rows than the counts when the data is short; zero counts; missed row end from the planned length; truncated row end from the actual length; no now row gives the queued boundary at the current time; `in_liquidsoap` for queued and planned rows.
- **Repository, integration (testcontainers):** the played block is the last N across aired, truncated and missed by due time, and ignores planned and queued rows; queued ignores aired items and items whose start has passed.
- **HTTP, integration:** 200 with the control link down (no now row, rows still present); a changed `air.played_count` is visible on the next call.
- **Settings:** out-of-range values are refused per field (422); defaults are 10 and 9.

## 6. Done when

- `GET /api/air-window` returns 10 played rows, the now row and 9 queued rows on a station with a plan and playout running, with the default settings.
- With Liquidsoap stopped, the same call returns played and queued rows and no now row.
- Changing `air.played_count` changes the next response.
- `make openapi`, `make openapi-lint`, `make lint` and `make test-integration` pass.
- The OpenAPI snapshot in `api/openapi.json` is updated, so the frontend can generate its client (frontend plan F15).

## 7. Known limits

- The now row comes from the in-memory playout state, which callbacks feed. If the backend restarts in the middle of a track, the now row stays absent until the next track starts. Restoring it from the playout journal is not part of this phase; it was left out. Until the next track starts, the item on air shows as the newest played row.
- If the grid assigns no clock to the coming hours, the queued block is shorter. The manager already sees that as a grid warning.
- An hour that is frozen (phase 9, decision 2) shows its items as they are in the plan: `planned` or `queued`.

## 8. Out of scope

- The Live page (voice broadcasting stays as it is).
- The played-tracks and track-plan pages (unchanged).
- A WebSocket event for the window itself (the panel reloads on the events of §9 and frontend plan §2.4).
- An event when playout pushes an item to Liquidsoap (`planned` → `queued`): it happens right after the previous track starts, so the reload on `playout.now_playing` shows it within seconds.
- Track preview in the list.

## 9. Addendum: `playout.track_ended` (2026-10-15)

The existing events were not enough for one case: the end of a track sent nothing. When no track followed (an empty plan, the end of an emergency track) the panel kept showing the ended track on air, and a reload on the next start could run before the end (and a `truncated` status) was written.

1. **New event `playout.track_ended`** (`internal/ws/events.go`, registered in `httpapi/api.go`): `item_id` (absent for an emergency track), `track_id`, `source` (`plan`, `emergency`), `status` (`aired`, `truncated`: the play log status after the end), `started_at`, `ended_at` (Liquidsoap's time of the end), `actual_duration_ms`.
2. **Sent by playout** after the end is in the play log, only for a live callback (journal replay sends nothing), once per plan item: a repeated end (the hour switch reports a cut track twice) sends nothing. An emergency track is logged `aired` on its end and reported the same way.
3. **No change to `playout.now_playing`**: a null payload for "nothing playing" would break its schema for the Live page.
4. **Tests:** playout unit tests (first end only, status after the end, replay sends nothing, emergency end), the OpenAPI event list, and the air window integration test (after the end, no now row and the item is the newest played row, truncated).
