# Plan — Library Statistics and Stored On-Air Length (radio-atlantida-backend)

Status: Approved by the operator (2026-11-10); not started
Based on: `docs/tech-design.md` (§4 `tracks`, §6 tracks API), frontend `docs/track-stats-plan.md`
Last updated: 2026-11-10

## 1. Goal

The Library page gets a small statistics row: tracks, reviewed, with issues, not analysed, and total on-air time. No endpoint returns these figures, and the total time cannot be obtained without paging through every track.

After this change, `GET /api/tracks/stats` returns the figures for the same filters as `GET /api/tracks`, and `tracks` stores the on-air length of each track in its own column.

## 2. Decisions

1. **On-air length is a generated column.** `tracks.air_duration_ms` is `GENERATED ALWAYS AS (...) STORED`. All writes to `duration_ms` and the cue columns (`Create`, `SetCue`, the import insert, `SaveAnalysis`) are then covered with no change to them, and the value cannot drift from its sources.
2. **The expression equals the Go method.** `Track.AirDurationMS()` returns nil when `duration_ms` is NULL, even with `cue_out_ms` set. The column does the same. The bare `COALESCE(...)` form used by the rotation query differs for that row and is not used.
3. **"With issues" has one definition, in one place.** It is `cardinality(tr.quality_issues) > 0`, the condition of the `quality=issues` filter. The filter and the statistics use the same named SQL constant, so changing the definition later changes both. The same applies to "not analysed" and "reviewed". `quality_ok` is not used, and a track with a manual "ok" override and open issues counts as with issues, as it does in the filter today.
4. **The statistics follow the filters.** With no filters they describe the whole library. The endpoint takes exactly the filter parameters of `GET /api/tracks` and no `sort`, `order`, `offset`, `cursor`, or `limit`. Both operations share one input struct for the filters, so they cannot drift apart.
5. **A track without a duration adds nothing to the total time** and still counts in the track count.
6. **Readers of the on-air length are unchanged.** `Track.AirDurationMS()`, `air_duration_ms` in track responses, `Cue.Check`, and the SQL in `rotation` and `clocks` stay as they are; rotation and clocks also apply validity rules that the column does not.
7. **Sort by duration uses the on-air length.** The Library column shows the on-air length, so the sort key `duration` of `GET /api/tracks` moves from `tr.duration_ms` to `tr.air_duration_ms`. Tracks without a value stay last in both orders, and ties stay ordered by title and ID.
8. **No new index.** The query scans the filtered tracks, as the existing `count` does.

## 3. Work

### 3.1 Migration

`internal/migrations/0021_track_air_duration.up.sql`, forward-only, with the usual comment header:

```sql
ALTER TABLE tracks
  ADD COLUMN air_duration_ms integer
  GENERATED ALWAYS AS (
    CASE WHEN duration_ms IS NOT NULL
      THEN COALESCE(cue_out_ms, duration_ms) - cue_in_ms
    END
  ) STORED;
```

- It rewrites the table once under an exclusive lock. Migrations run before the backend starts, and the table is small enough for this.
- Do not add the column to `trackWrite` or to any `INSERT` or `UPDATE`: PostgreSQL refuses an explicit value.
- **A schema change is applied, not only written:** run `make migrate` against the dev database before running or checking anything, then `make test-integration`.

### 3.2 Repository (`internal/repository/library/tracks.go`)

- Move the quality and reviewed conditions of `trackWhere` (lines 141–185) into named constants, for example `condTrackHasIssues`, `condTrackUnanalysed`, `condTrackReviewed`. `trackWhere` uses them; its results do not change.
- Add `Stats(ctx, filter)`, built like `count()` (lines 273–280), with `fromTracks` because `in_plan` needs its joins:

```sql
SELECT count(*),
       count(*) FILTER (WHERE <reviewed>),
       count(*) FILTER (WHERE <has issues>),
       count(*) FILTER (WHERE <unanalysed>),
       COALESCE(sum(tr.air_duration_ms), 0)
```

  The sum is `bigint`; scan it into `int64`. Cursor and offset do not take part.
- Change `library.SortDuration` in the sort whitelist (line 103) to `tr.air_duration_ms`. Update the description of the `sort` parameter if it says file length.

### 3.3 Domain (`internal/library/tracks.go`)

- `TrackStats` type: `Total`, `Reviewed`, `WithIssues`, `Unanalysed`, `AirDurationMS` (`int64`).
- `Stats` on `TrackStore`, `Service.TrackStats(ctx, filter)`. Update the fakes that implement these interfaces.

### 3.4 API (`internal/httpapi`)

- `handlers/tracks.go`: move the filter fields of `listTracksInput` (`type_id`, `artist_id`, `playlist_id`, `q`, `quality`, `reviewed`, `in_plan`, `in_playlist`) to an embedded struct with a method that builds `library.TrackFilter`. Apart from the sort of decision 7, `GET /api/tracks` behaves and documents as before; compare `api/openapi.json` before and after.
- Add `GET /api/tracks/stats`, OperationID `get-track-stats`, with that struct as its input.
- `dto/library.go`: `TrackStats` with `total`, `reviewed`, `with_issues`, `unanalysed`, `air_duration_ms`, all required and documented. The doc comments state that the figures match the filters and that `with_issues` means the `quality=issues` filter.
- Add `TrackStats` to the `handlers.Library` interface and update the fake.
- Regenerate `api/openapi.json` with `make openapi`.

### 3.5 Order of work

1. Migration, then `make migrate`.
2. Repository constants and `Stats`, with integration tests.
3. Domain, handler, DTO, `make openapi`.
4. `make test-integration`.

## 4. Tests

Integration, migration and repository (real PostgreSQL), table-driven:

- `air_duration_ms` equals `Track.AirDurationMS()` for: no cue; cue in only; cue out only; both; `duration_ms` NULL; `duration_ms` NULL with `cue_out_ms` set.
- The column changes after `SetCue` and after `SaveAnalysis` with a new duration and with a cue reset.
- `Stats` on an empty table: all zeros, no NULL scan error.
- `Stats` on a mixed set: reviewed, with issues, not analysed, a track with no duration, and a track with a manual `quality_ok = true` and open issues (counts as with issues).
- `Stats` equals `total` of `GET /api/tracks` for each filter, including `in_plan` and `in_playlist=false`, and for a combination of filters.
- Sort `duration`: a track with a long file and a short cue sorts by its on-air length, in both orders; tracks without a duration are last in both orders.

Unit:

- Handler (`humatest`, fake): `/api/tracks/stats` is not taken by `/api/tracks/{id}`, filters reach the service, and `sort` or `limit` do not take part. The service error maps through `problemFor`.

## 5. Done when

- The migration is applied to the dev database with `make migrate`.
- `make test`, `make test-integration`, `make lint`, `make openapi-lint` pass.
- No SQL outside `internal/repository`, and the architecture test passes.
- `api/openapi.json` shows the new path and, for `GET /api/tracks`, at most a changed description of `sort`.
