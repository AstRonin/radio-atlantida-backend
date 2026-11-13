# Plan — Complex Track Queries from GORM to pgx (radio-atlantida-backend)

Status: Approved (2026-11-14). Starts after `docs/settings-guards-domain-plan.md` is done.
Based on: `docs/backend-split-plan.md` §5 item 2, CLAUDE.md (GORM for admin CRUD, pgx for complex queries; repository layer), Plan agent analysis (2026-11-13)
Last updated: 2026-11-14

## 1. Goal

`internal/repository/library/tracks*.go` runs complex SQL through `gorm.Raw(...).Scan` (CTE, `count(*) OVER ()`, dynamic WHERE/ORDER, UNION, `DELETE ... RETURNING`, aggregates). Move it to pgx in a separate store. Simple CRUD stays on GORM. API behaviour does not change.

## 2. Decisions (developer, 2026-11-14)

1. **Two stores, not one with two handles.** A new pgx store `TrackQueries` beside the GORM `Tracks`, like `clocks.CheckStore`. The domain interface `library.TrackStore` is split accordingly.
2. **`quality_issues` is scanned as `text[]` into `[]string`.** Drop `array_to_string` from the select and the `issues()` helper (as in `repository/ingest`).
3. **`Total` under strict scanning** (main agent): `List` scans a `listRow` that embeds `trackRow` plus `Total int64 \`db:"total"\``; `Get` scans `trackRow` (no `total`). Check that pgx flattens the embedded struct; if not, use explicit fields and record it in §7.

## 3. Scope

Moves to `TrackQueries` (pgx), because they are complex or share SQL/rows with complex ones:

| Method / helper | Why |
|---|---|
| `List`, `count`, `trackWhere`, `whereClause`, `notIf`, `trackOrder`, `withLinks` | CTE, window count, dynamic WHERE/ORDER, keyset cursor |
| `Stats` | aggregates over `trackWhere` |
| `Get` | shares `selectTracks`/`trackRow`/`withLinks` with `List` |
| `Users`, `Blockers` (`usersOfTracks`) | UNION |
| `DeleteUnreferenced` | `DELETE ... RETURNING` with constraint mapping |
| `PlannedHours`, `MissedHours` | aggregates with intervals |

Stays on GORM `Tracks`: constructor, `Create`, `Update`, `SetPlaylists`, `Delete`, `SetReviewed`, `Titles`, `SetTitles`, `CueState`, `SetCue`, and `TableName`.

## 4. Work

1. **Domain (`internal/library`).** Split `TrackStore` into `TrackStore` (GORM methods above, `WithTx`) and `TrackQueries` (pgx methods above, `WithTx(tx db.Tx) TrackQueries`). The track service takes both through its constructor and, inside `d.WithTx`, binds both to the same `tx` (cue save: `SetCue` + `PlannedHours` in one transaction, `library/cue.go`; delete retry loop around `DeleteUnreferenced`, `library/tracks.go`). Update fakes in domain unit tests (split the fake or let one fake implement both).
2. **Repository (`internal/repository/library`).**
   - `tracks_queries.go`: `type TrackQueries struct{ q db.Querier }`, `NewTrackQueries(q db.Querier)`, `WithTx(tx db.Tx) library.TrackQueries` returning `&TrackQueries{q: tx.Pgx()}`. Pattern: `repository/ingest/ingest.go`.
   - The moved methods keep their topic files (`tracks_list.go`, `tracks_usage.go`, `tracks_cue.go`, `tracks_sql.go`); only the receiver changes to `*TrackQueries`.
   - Placeholders `?` → `$n`. `trackWhere` uses a small local arg builder (`add(v) string` returning `$n`), kept in `tracks_list.go`, no shared helper package. The `q` filter uses one `$n` twice. Cursor and `LIMIT`/`OFFSET` are numbered after the WHERE args.
   - `IN ?` → `= ANY($n)` with `[]uint64`. In `usersOfTracks` the same `$1` is used in both UNION branches and the limit is `$2`.
   - Scanning: `pgx.CollectRows` + `RowToStructByName`, `db.CollectAll`/`db.CollectOne`; scalars `QueryRow().Scan` or `pgx.RowTo`. Every row struct gets `db:"..."` tags (`trackRow`, `listRow`, `trackStatsRow`, `trackUserRow`, a named `blockerRow`; `linkRow`/`refRow` in `library.go` get tags too, GORM ignores them).
   - `DeleteUnreferenced`: wrap the collect error with `deleteError(...)` too (a pgx FK violation surfaces at `rows.Err()`), so `ErrTrackHasHistory`/`ErrInUse` mapping and the service retry loop keep working. Returns `[]uint64{}` instead of nil when nothing is deleted; verify callers.
   - `PlannedHours`/`MissedHours`: keep the nil checks and `.UTC()` normalisation.
   - `Users`: check the error before building results (current order is wrong).
   - Keep every EXPLAIN comment verbatim (`tracks_list.go` List and Stats, `tracks_usage.go`, `tracks_sql.go`). Reword the `array_to_string` comment away (decision 2).
3. **Wiring (`cmd/backend/serve.go`).** `librepo.NewTrackQueries(database.Pool)` passed to the track service.
4. **Docs.** Package doc of `internal/repository/library` and CLAUDE.md layout line `library/ (GORM; one store per aggregate)` → mention `TrackQueries` (pgx: list, stats, usage, delete of unreferenced, planned/missed hours), like `clocks.CheckStore`.

## 5. Tests

- Existing integration tests go through public methods and should pass unchanged apart from construction: `tracks_sort_`, `tracks_stats_`, `tracks_review_`, `tracks_cue_`, `tracks_in_use_integration_test.go`, `library_integration_test.go` in `internal/repository/library`, and `internal/httpapi/tracks_cue_integration_test.go`, `library_integration_test.go`.
- Add integration tests:
  - `DeleteUnreferenced` and `Blockers` with an empty id list.
  - `List` with the `q` filter plus other filters, cursor and offset together (proves `$n` numbering).
  - `SetCue` (Tracks) and `PlannedHours` (TrackQueries) bound to one `tx`, if `TestCueStateSetCueAndPlannedHours` does not already cover it.
  - `quality_issues` round-trip (several issues, none).
- Architecture test: pgx is allowed in `internal/repository`, no allowList entry needed.

## 6. Commits and done when

Commits: (1) domain interface split + fakes + wiring with `TrackQueries` still delegating nothing new (compiles, tests pass) — or fold into (2) if a separate step does not compile cleanly; (2) move list/stats/get; (3) move usage/blockers/delete; (4) move planned/missed hours; (5) docs and plan status. Each commit: `make fmt`, `go build ./...`, `go vet ./...`, `make test`; after (2)–(4) `make test-integration`; at the end `make lint`.

Done when all checks pass, no `gorm.Raw` remains for the methods in §3, and API responses are unchanged (integration tests).

## 7. Deviations

(Filled in during implementation.)
