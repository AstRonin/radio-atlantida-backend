# Phase 19 Plan — Bulk Track Delete (radio-atlantida-backend)

Status: Implemented
Based on: `tech-design.md` (§4 deletion and history, §6), `docs/phase-11-plan.md` (library selection, analyse and transcode bodies), frontend `docs/f17-bulk-delete-plan.md`
Last updated: 2026-10-16

## 1. Goal

The manager selects tracks in the library (up to a thousand after an unwanted import) and deletes them in one action. Today the API has only `DELETE /api/tracks/{id}`; the panel would have to send one request per track, a delete would stop half done when the page is left, and the refusals would come back one by one. This phase adds one request that deletes many tracks and reports, per track, what was deleted and what was refused and why.

The rules of a single delete do not change: the file is never touched; a track in a clock filler pool is refused (`track_in_use`, with `used_by`); a track with play history, in the track plan, or in a one-time hour insert is refused (`track_has_history`).

## 2. Decisions (2026-10-16)

1. **Endpoint.** `POST /api/tracks/delete` (operation ID `delete-tracks`, tag `Tracks`). `POST`, not `DELETE` with a body, because a body on `DELETE` is not reliably passed by proxies and clients, and the route sits next to `POST /api/tracks/analyse` and `/transcode`.
2. **Body** `DeleteTracksBody`: `track_ids`, required, 1 to 1,000 items (the limit of `AnalyseBody`), unique. Empty, too many, or duplicates: 422 with field `track_ids` (huma schema validation). `additionalProperties: false`.
3. **Synchronous, not an operation.** The delete is one database statement, with no file I/O: 1,000 tracks take about 15 ms (30,000 tracks in the library), far inside the 30 s route timeout. An operation (§6.5) carries only a `result_summary` string, but the panel needs a structured list of refusals per track. So the request returns the result directly (200).
4. **One statement, refusals per track.** `DELETE FROM tracks WHERE id IN (...) AND NOT EXISTS (...) RETURNING id` deletes every listed track that no play log row, plan item, hour insert, or filler pool refers to; a refused track does not keep the others (what the manager expects from "delete the selected tracks", the same outcome as N single deletes). One more query then reads, for the tracks not deleted, the title, the history, and the filler pool clocks, and gives each its refusal: missing → `track_not_found`; in a pool → `track_in_use` (as for the single delete, the pool wins over history); otherwise `track_has_history`. A reference committed while the statement runs (a plan build planning a just imported track) fails the statement on the foreign key and deletes nothing; it is run again, at most 3 times, and the next run keeps that track.
5. **Response** `DeleteTracksResult`:
   - `deleted`: IDs of the deleted tracks, in request order (never null);
   - `refused`: one item per track not deleted (never null), each `{ track_id, code, title?, used_by? }`:
     - `code`: `track_in_use`, `track_has_history`, or `track_not_found` (the existing `errcodes`; published as the enum of the field);
     - `title`: the track title, for the panel to name the track without another request (absent for `track_not_found`);
     - `used_by`: the clocks whose filler pool holds the track (`EntityRef`, at most `MaxUsers`), only for `track_in_use`.

   An unknown ID is a refusal with `track_not_found`, not a 404 of the whole request: a track deleted in another tab meanwhile must not fail the batch.
6. **Leaving the page does not stop it.** The service runs on `context.WithoutCancel(ctx)`, so a closed tab or a cancelled request still deletes every listed track; the panel sees the result on its next list load. Each statement keeps `dbTimeout` (5 s): at most 3 delete attempts and the refusal query stay below the 30 s route timeout.
7. **Unexpected errors.** A database error other than a refusal (connection lost, timeout, new references in all 3 attempts) returns 500. The delete is one statement, so it deletes all or none of the deletable tracks; if the refusal query fails after it, the deleted tracks stay deleted. The problem detail says how many were deleted (`"deleted 0 of 1000 tracks before the error"`, `"deleted 666 of 1000 ..."`), and the error is logged.
8. **No lock against media operations.** A single delete is not refused while an import, analysis, or transcode runs, so the bulk delete is not either (parity). An analysis or transcode that reaches a track deleted after it started gets `ErrTrackNotFound` when it stores the result; until this phase that counted as `failed` with an ERROR log. It now counts as a new outcome `track deleted` (logged at info, not a failure); a transcode in replace mode has by then rewritten the file, which no track refers to any more (the file is never deleted). Tested in §6.
9. **No new WebSocket event.** The panel reloads its list after the response; no other screen keeps a live track list. The track plan is not affected: a planned track is refused.
10. **Logging.** One `info` line per request, component `api` (the logger of the library service; there is no `library` component): `tracks deleted` with `requested`, `deleted`, `refused` counts. Refusals are not warnings.

## 3. Service

`internal/library`:

```go
// TrackRefusal is a track a bulk delete did not remove.
type TrackRefusal struct {
	TrackID uint64
	Title   string // empty for ErrTrackNotFound
	Err     error  // ErrTrackInUse, ErrTrackHasHistory, or ErrTrackNotFound
	UsedBy  []Ref  // for ErrTrackInUse
}

type DeleteTracksResult struct {
	Deleted []uint64
	Refused []TrackRefusal
}

// DeleteTracks deletes the tracks that DeleteTrack would delete, in one
// statement, and reports why each other track was kept. It runs to the end
// when the caller goes away.
func (s *Service) DeleteTracks(ctx context.Context, ids []uint64) (DeleteTracksResult, error)

// DeleteTracksError: an unexpected error; Deleted says how many tracks are gone.
type DeleteTracksError struct { Deleted, Requested int; Err error }
```

- `TrackStore.DeleteUnreferenced(ctx, ids) ([]uint64, error)`: the one delete statement (decision 4); a new reference fails it with `ErrInUse`/`ErrTrackHasHistory` and the service runs it again (at most 3 times).
- `TrackStore.Blockers(ctx, ids, limit) (map[uint64]TrackBlockers, error)`: for the existing tracks of `ids`, the title, `History`, and `UsedBy` (filler pool clocks, at most `MaxUsers` per track); two queries for all tracks, not one per refusal.
- Limits (1..1,000, unique) are checked in the service too (`validation.Field("track_ids", ...)`), so a caller other than the HTTP handler cannot bypass them.

`internal/repository/library`: `Tracks.DeleteUnreferenced` (`DELETE ... WHERE id IN ? AND NOT EXISTS (...) RETURNING id`, every `NOT EXISTS` on an index leading with `track_id`) and `Tracks.Blockers` (the pool clocks per track by `row_number() OVER (PARTITION BY track_id)`).

## 4. HTTP

`internal/httpapi/handlers/tracks.go`: register `delete-tracks` after `delete-track`.

- Description: "Deletes several tracks from the library (the files are not touched), each like DELETE /api/tracks/{id}. Refused tracks are listed in `refused` with the reason; the others are deleted. Runs to the end even when the client disconnects."
- Errors: 422 (body), 500.
- DTOs in `internal/httpapi/dto`: `DeleteTracksBody`, `DeleteTracksResult`, `TrackRefusal` (`code` with `enum:"track_in_use,track_has_history,track_not_found"`).
- The error-to-code mapping reuses `problemFor`'s table for the three track errors, so the codes cannot drift from the single delete.

## 5. Files

| File | Change |
|------|--------|
| `internal/library/tracks.go` | `DeleteTracks`, `TrackRefusal`, `DeleteTracksResult`, `DeleteTracksError`, `TrackBlockers`; `TrackStore.DeleteUnreferenced`, `TrackStore.Blockers` |
| `internal/repository/library/tracks.go` | `DeleteUnreferenced`, `Blockers` |
| `internal/httpapi/dto/library.go` | the three DTOs (next to the other track DTOs) |
| `internal/httpapi/handlers/tracks.go` | `delete-tracks` |
| `internal/httpapi/handlers/errors.go` | `trackRefusalCode` (from `inUseCodes` and `refusals`); `*library.DeleteTracksError` → 500 with the count in the detail |
| `internal/ingest/analyse.go`, `transcode.go` | outcome `track deleted` for `ErrTrackNotFound` when storing the result (decision 8) |
| `api/openapi.json` | `make openapi` |
| `docs/tech-design.md` | §6 endpoint table: `POST /api/tracks/delete`; §4 deletion note: the bulk delete applies the same rules per track |
| `CLAUDE.md` | Phase 19 in the required-reading list |

## 6. Tests

- **Unit, service (fake store):** all deleted; a mix of deleted, `track_in_use` (with `used_by`; also with history), `track_has_history`, and `track_not_found` in request order with titles, in one delete statement; a cancelled caller context still deletes; a new reference fails the first statement and the second keeps that track; new references in every attempt, a failed delete (0 deleted), and a failed refusal query (count deleted) give `DeleteTracksError`; limits 0, 1,001, duplicates refused with field `track_ids`.
- **Integration (repository):** `Blockers` (title, history, each clock once in name order, at most `limit`, unknown IDs absent); deleting a track in `clock_filler_pool`, in `play_log`, in `scheduled_items`, and in `hour_inserts` gives the right refusal, and the track-playlist links of a deleted track are gone.
- **HTTP, integration:** `POST /api/tracks/delete` with three tracks, one in a filler pool: 200, `deleted` has two IDs, `refused` one item with `track_in_use` and the clock in `used_by`; an empty list and 1,001 IDs: 422 field `track_ids`; the route is in the OpenAPI route list test (`api_test.go`).
- **Unit, ingest:** an analysis and a transcode whose track list contains a deleted track skip it and complete.

## 7. Done when

- 1,000 imported, never-played tracks are deleted by one request in under 5 s on the dev stack; `GET /api/tracks` no longer lists them; their files are still on disk.
- In a mixed selection the played and pooled tracks are listed in `refused` with the right code, and every other track is deleted.
- `make lint`, `make test`, `make test-integration`, `make openapi-lint` pass.

## 8. Known limits

- If the refusal query fails after the delete, the deleted tracks stay deleted and the response is 500 without the list (decision 7); the panel reloads its list.
- A plan item removed by a rebuild between the delete and the refusal query leaves a kept track with no reference; it is reported as `track_has_history` (it was planned when it was refused). Deleting it again succeeds.
- `used_by` names clocks only (the filler pool), as for the single delete.

## 9. Out of scope

- Deleting the files of the tracks.
- Delete by filter ("every track with issues") without IDs.
- Removing play history or plan entries to make a track deletable.
- An undo.
