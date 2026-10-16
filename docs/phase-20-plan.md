# Phase 20 Plan — Track List Pages and Sort (radio-atlantida-backend)

Status: Implemented
Based on: `tech-design.md` (§6, track list), `docs/phase-11-plan.md` (quality filter), `docs/phase-19-plan.md` (bulk delete), frontend `docs/f18-library-pagination-sort-plan.md`
Last updated: 2026-10-16

## 1. Goal

The library holds thousands of tracks. `GET /api/tracks` returns them in title order with a keyset cursor, so the panel can only "Load more": it cannot jump to page 40, does not know how many tracks match, and cannot sort by another column. Sorting the loaded rows in the browser would be wrong, because it would sort only the pages already read.

This phase lets the list be read as numbered pages (`offset`, `limit`, and a `total`) in an order the client chooses (`sort`, `order`), with the sort done by PostgreSQL over the whole filtered set. The cursor stays for the pickers and the playlist pages, which only scroll.

## 2. Decisions (2026-10-16)

1. **Same endpoint, new query parameters.** `GET /api/tracks` (operation `list-tracks`) gains `sort`, `order`, and `offset`, and its response gains `total`. No new route: the filters (`type_id`, `artist_id`, `playlist_id`, `q`, `quality`) and `limit` (1..500, default 100) stay as they are and combine with the sort.
2. **Sort keys** (`sort`, enum, default `title`), one per sortable column of the library table:

   | `sort` | SQL key (then the tie-breaker) |
   |--------|--------------------------------|
   | `title` | `tr.title` |
   | `artist` | `a.name` (tracks without an artist last) |
   | `type` | `t.name` |
   | `duration` | `tr.duration_ms` (tracks without a duration last) |
   | `format` | `tr.format` (empty last) |
   | `quality` | rank: no issues 0, with issues 1, not analysed 2; within "with issues", more issues first |
   
   The SQL for each key is a fixed string in a Go map keyed by the enum value; the parameter never reaches the SQL text. Huma rejects any other value with 422 (`query.sort`).
3. **Direction** `order`: `asc` (default) or `desc`. Missing values (no artist, no duration, empty format) are **last in both directions** (`NULLS LAST`, empty strings mapped to `NULL`), so a descending sort does not open with a page of blanks.
4. **Stable order.** Every sort ends with `tr.title, tr.id` (or `tr.id` when the key is `title`), always ascending for the tie-breaker. Without it, rows with equal keys could move between pages and a track could appear on two pages or on none.
5. **Pages by offset.** `offset` (0 to 1,000,000, default 0) skips that many rows of the sorted, filtered set. Offset is chosen over a keyset because the panel jumps to any page and goes back; for tens of thousands of rows the cost is one sort of the filtered set (see §3). An offset past the end returns `items: []` with the correct `total`, not an error, so a page that empties after a delete can be corrected by the client.
6. **`total`** is the number of tracks matching the filters, ignoring `offset` and `limit`, always present (`int`, required). It is read in the same query with `count(*) OVER ()`; when the page is empty (offset past the end) one `SELECT count(*)` with the same `WHERE` gives it. The two reads are not in one snapshot; a track imported in between can make `total` off by a few, which the next load corrects (acceptable for a list view).
7. **Cursor stays, only for the default order.** `cursor` keeps working with `sort=title&order=asc` (or both absent), so `track-picker`, `playlist-tracks`, and `playlist-edit` (`readMembers`) do not change. `cursor` together with `offset > 0`, or with another sort or order, is a 422 on `query.cursor` ("a cursor works only with the default order and without offset"). `next_cursor` is returned only in the default order; in another order the client pages with `offset` and `total`. No keyset over arbitrary nullable keys: it would need a cursor format per key for a case nobody uses.
8. **No new indexes.** The existing note on `Tracks.List` holds: a sequential scan with a sort is fast at this size. The sort with an offset sorts `offset + limit` rows (top-N heapsort while it fits in `work_mem`), at worst the whole filtered set. Measured with `EXPLAIN ANALYZE` on 30,000 tracks before merging (§7); an index is added only if the last page of an unfiltered sort is above 50 ms.
9. **The title sort follows the database collation**, as today. `a.name` and `t.name` too. No `COLLATE` override.
10. **Contract.** The OpenAPI description of `list-tracks` changes to: "Tracks matching the filters, sorted by `sort` and `order` (ties by title and ID), a page at a time: read the next page with `offset`, or with `next_cursor` as `cursor` in the default title order. `total` counts every matching track." `TrackPage.total` is documented. The change is additive for current clients: they send none of the new parameters and get the same items and cursor.

## 3. Service and repository

`internal/library`:

```go
// TrackSort is a sort key of the track list.
type TrackSort string

const (
	SortTitle    TrackSort = "title"
	SortArtist   TrackSort = "artist"
	SortType     TrackSort = "type"
	SortDuration TrackSort = "duration"
	SortFormat   TrackSort = "format"
	SortQuality  TrackSort = "quality"
)

type TrackFilter struct {
	// ... the existing filters
	Sort   TrackSort // empty: SortTitle
	Desc   bool
	Offset int
	After  *TrackCursor // only with the default order and Offset 0
	Limit  int
}

// TrackPage is a page of tracks. Next is set in the default order when more
// tracks follow; Total counts every track matching the filters.
type TrackPage struct {
	Items []Track
	Next  *TrackCursor
	Total int
}
```

- `ListTracks` checks the combination (decision 7) with `validation.Field("query.cursor", ...)`, so the rule holds for any caller, not only the handler.
- `Next` is computed as today (read `limit + 1`) only in the default order.

`internal/repository/library/tracks.go`:

- `List` builds `ORDER BY` from a `map[library.TrackSort][]string` of fixed SQL keys plus the direction and the tie-breaker. An inner query sorts only the IDs with `count(*) OVER () AS total`, `LIMIT ? OFFSET ?`; the outer one joins the columns of the page rows. Sorting the full rows took twice as long (§7).
- A count with the same `WHERE` (unexported, called by `List` itself, so `List` returns the total): used when `List` returns no rows and `Offset > 0`, and with a cursor (the cursor narrows the `WHERE` of the window). The `WHERE` builder is one function shared by both, so the filters cannot drift.
- The comment with the `EXPLAIN ANALYZE` figures is updated with the new measurements.

## 4. HTTP

`internal/httpapi/handlers/tracks.go`, `listTracksInput`:

```go
Sort   string `query:"sort" enum:"title,artist,type,duration,format,quality" default:"title" doc:"Sort key; ties are ordered by title and ID."`
Order  string `query:"order" enum:"asc,desc" default:"asc"`
Offset int    `query:"offset" minimum:"0" maximum:"1000000" default:"0" doc:"Tracks to skip in the sorted list."`
```

`internal/httpapi/dto/library.go`, `TrackPage`: `Total int \`json:"total" minimum:"0" doc:"Tracks matching the filters, on every page."\``. The `next_cursor` doc says it is set only in the default order.

## 5. Files

| File | Change |
|------|--------|
| `internal/library/tracks.go` | `TrackSort`, `TrackFilter.Sort/Desc/Offset`, `TrackPage.Total`, the cursor check |
| `internal/repository/library/tracks.go` | sort map, offset, `count(*) OVER ()`, `Count`, shared `WHERE` builder |
| `internal/httpapi/handlers/tracks.go` | `sort`, `order`, `offset`; description |
| `internal/httpapi/dto/library.go` | `TrackPage.total` |
| `api/openapi.json` | `make openapi` |
| `docs/tech-design.md` | §6 track list: the new parameters and `total`; the cursor only in the default order |
| `CLAUDE.md` | Phase 20 in the required-reading list |

## 6. Tests

- **Unit, service (fake store):** default sort is title asc; `cursor` with `sort=duration`, with `order=desc`, or with `offset=50` is a 422 on `query.cursor`; `Next` is absent in a non-default order.
- **Integration, repository** (fixture of ~30 tracks with equal titles, missing artists, missing durations, all three quality states):
  - each sort key in both directions gives the expected order; missing values are last in both;
  - equal keys are ordered by title then ID; reading all pages with `limit=7` by offset returns every track exactly once;
  - `quality` order: no issues, then most issues first, then not analysed;
  - every filter combined with a sort and an offset; `total` equals the filtered count on the first, middle, and last page, and on an offset past the end (`items` empty);
  - the cursor in the default order still returns the same pages as before this phase (regression).
- **HTTP, integration:** `GET /api/tracks?sort=duration&order=desc&offset=10&limit=5` returns 5 items and `total`; `sort=bitrate` and `order=up` are 422 on the field; the OpenAPI route list test passes.

## 7. Done when

- With 30,000 tracks on the dev stack, the panel reads page 1 and the last page in every sort in under 50 ms of query time (`EXPLAIN ANALYZE` figures in the `List` comment).
- `GET /api/tracks?sort=artist&order=desc&offset=200&limit=50` returns tracks 201..250 of that order and `total`; the next offset returns the next 50 with no overlap.
- The pickers and playlist pages work unchanged with the cursor.
- `make lint`, `make test`, `make test-integration`, `make openapi-lint` pass.

## 8. Known limits

- Offset pages can shift by a row when tracks are added or deleted between two page reads; the panel reloads on import, analysis, and transcode events.
- `total` and the page are not one snapshot when the page is empty (decision 6).

## 9. Out of scope

- Sorting by several columns at once.
- Sorting by a field the library table does not show (date added, bitrate).
- Sorting by playlist membership, play count, or last played.
- A keyset cursor for non-default orders.
- Full-text search or new filters.
