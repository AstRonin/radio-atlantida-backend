# Artists list paging and sort: backend plan

Frontend counterpart: `frontend/docs/artists-paging-plan.md`.

## Goal

The Artists page and the artist picker must work with thousands of artists. Today `GET /api/artists` returns every artist in one response (`internal/repository/library/artists.go:36-37`: "The library has hundreds of artists, so the list is not paged"). This plan adds offset paging, sort, and a total, following `GET /api/tracks` (`docs/phase-20-plan.md` §2).

The delete rule does not change: an artist with tracks is refused with 409 `artist_in_use` (with `used_by`), an artist with play history with 409 `artist_has_history`. Nothing cascades.

## Decisions

- `GET /api/artists` query parameters:
  - `q` as today (case-insensitive "name contains", at most 200 characters).
  - `sort`: `name` (default) or `track_count`.
  - `order`: `asc` (default) or `desc`.
  - `offset`: 0 to 1,000,000, default 0.
  - `limit`: 1 to 500, default 100.
  - Values outside these ranges or unknown enum values are 422 on the field, the same way as `GET /api/tracks`.
- Order:
  - `name`: `a.name` in the requested order, ties by `a.id` ascending (same tie rule as tracks).
  - `track_count`: the count in the requested order, ties by `name` ascending, then `id` ascending.
- Response `ArtistList` gains `total`: the number of artists matching `q`, the same on every page. An offset past the end returns no items and the correct `total`.
- No cursor. `CLAUDE.md` asks for keyset paging on long lists; this list uses `OFFSET` on purpose, like `GET /api/tracks`: the panel needs page numbers, sort by count, and `total`, and the list stays in the thousands, not millions.
- The track count stays a correlated subquery (it uses `ix_tracks_artist_id`). Sorting by it sorts the whole filtered set in PostgreSQL. Run `EXPLAIN ANALYZE` on a dev database with at least 5,000 artists and 50,000 tracks (generate fixtures if needed, do not commit them) and report the timings of both sorts with `limit=50`. If the `track_count` sort takes more than 200 ms, stop and report before changing the query.
- `GET /api/artists/{id}` stays as it is. The frontend picker starts using it to show the selected artist's name.
- Compatibility: the frontend artist picker today calls `GET /api/artists` without a limit and expects every artist. After this change it receives the first 100. Backend and frontend must be released together; state this in the report.

## Steps

1. Read `internal/httpapi/handlers/artists.go` (list input and operation), `internal/httpapi/dto/library.go` (`ArtistList`), `internal/library/artists.go` (`ListArtists`, store interface), `internal/repository/library/artists.go` (`List`), and, for the pattern, the track list: `internal/httpapi/handlers/tracks.go` (sort, order, offset, limit inputs) and its repository query and count.
2. Handler: add `sort`, `order`, `offset`, `limit` with enums, ranges, defaults, and descriptions in the OpenAPI document. Reuse the track list's shared types or helpers for order/offset/limit if they exist; do not copy them.
3. Service and store: pass a list query struct (q, sort, order, offset, limit) instead of a bare `q`. Return items and total.
4. Repository: build the ORDER BY from the validated sort key only (no string from the request goes into SQL). Count with a separate `SELECT count(*) FROM artists a WHERE <same filter>`. Run both in one read-only transaction or accept two statements; say which in a comment.
5. DTO: add `total` (integer, required) to `ArtistList`. Update the operation description.
6. Clean-up: the doc comment of `DeleteArtist` (`internal/library/artists.go:86-87`) still mentions clock references; those were dropped in migration 0013. Rewrite it: refused while tracks refer to the artist (`artist_in_use`, with `used_by`) or the play log does (`artist_has_history`). Also update the comment at `internal/repository/library/artists.go:36-37`.
7. Tests:
   - unit: input validation (unknown sort, order, limit 0 and 501, offset over the maximum → 422 on the field);
   - repository integration: both sorts in both orders with ties (same name, same count), `q` with paging, `total` independent of offset and limit, offset past the end;
   - httpapi integration: default request returns at most 100 items sorted by name with `total`; `sort=track_count&order=desc&limit=2` returns the two artists with most tracks;
   - delete (keep or add): an artist with tracks → 409 `artist_in_use` with `used_by` listing the tracks; an artist with play history only → 409 `artist_has_history`; an unused artist → 204 and it is gone.
8. Run `make fmt`, `make lint`, `make test`, `make test-integration` (Docker is available), `make openapi`, `make openapi-lint`. Report the exit codes.

## Constraints

- No git commit, push, branch or state change.
- No schema migration is expected. If an index seems needed, stop and report with the `EXPLAIN ANALYZE` output.
- Do not change the delete behaviour or the error codes.
- Do not touch the frontend or docs repositories.

## Done when

- `GET /api/artists` takes `sort`, `order`, `offset`, `limit` and returns `total`; the OpenAPI document shows them.
- The stale comments are fixed.
- All checks pass, with exit codes and the `EXPLAIN ANALYZE` timings reported.
