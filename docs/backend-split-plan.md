# Plan — Split Large Files (radio-atlantida-backend)

Status: Done (2026-11-13). Phases A–D implemented: A `e64f8b1` (A2 skipped), B `278dad6` (CLAUDE.md rule) + `0972b4f`, C `2a3edcd`, D `cfe036c`. §7 item 1 decided: option (a).
Based on: `go-reviewer` architecture review (2026-11-13), `docs/tech-design.md` (layers, GORM/pgx split)
Last updated: 2026-11-13

## 1. Goal

Four files have grown large and mix several responsibilities:

| File | Lines |
|---|---|
| `cmd/backend/serve.go` | 792 |
| `internal/repository/library/tracks.go` | 771 |
| `internal/rotation/service.go` | 808 |
| `internal/rotation/build.go` | 969 |

Split them by responsibility so each file has one topic, without changing behaviour. Layer boundaries are mostly respected already; two layer violations found by the review are separate follow-up tasks (§5).

## 2. Rules

1. **Pure moves only** in phases A–D. Move declarations into new files of the **same package**. Do not rename functions, types or constants, do not change signatures, logic or comments (comments move with their code).
2. **No new packages** in phases A–D. All `builder` methods share one mutable state, and `cmd/backend` wiring stays in package `main`.
3. **One file per commit.** After each step: `gofmt -l` clean, `go build ./... && go vet ./... && go test ./...`; for `internal/repository/library` also the integration tests (`go test -race -tags integration ./internal/repository/library/...`).
4. **Check name collisions** before creating a file: `ls` the package directory. If a target name already exists (for example `settings.go`, `ports.go` in `internal/rotation`), merge into it only if it holds the same topic, otherwise pick another name and record it in §6.
5. Line numbers below come from the review and may drift. Group by function name, not by line.
6. Do not touch `docs/` other than updating the status line of this plan. Test files are not changed or moved.
7. Phase B starts with a separate commit that amends the backend `CLAUDE.md` rule "One file per aggregate" (Repository layer) to: "One aggregate per file group: `<aggregate>.go` holds the constructor and writes; a large aggregate may add `<aggregate>_<topic>.go` files in the same package." (§7).

## 3. Target layout

### Phase A — `cmd/backend/serve.go` (first, lowest risk, halves the file)

| New file | Contents |
|---|---|
| `serve.go` | `serve`, `newPlayout`, `forwardAlerts`, `wakeOnStreamChange` |
| `wiring_settings.go` | settings adapters: `playoutSettings`, `streamOutput`, `retentionPeriods`, `intervalLimit`, `gridSettings`, `clockSettings`, `rotationSettings`, `airWindowSettings`, `outageSettings`, `ingestSettings`, `harborTarget`, `controlAddr`, `emergencySetting` |
| `settings_guards.go` | `guardRetention`, `checkEncoderLimits`, `encoderLimitCheck`, `streamFormatKeys` |

Optional A2 (separate commit, only a move into helpers, no behaviour change): split the ~200-line `serve` into `buildServices(...)` and `runServices(...)`. Skip it if it needs more than passing already-built values as arguments.

Schema startup checks (`readCompat`, `checkSchema`, `guardSchema`, `compatRetry`) already live outside `serve.go`; find where and leave them, unless they sit in an unrelated file, in which case move them to `schema_guard.go` and record it in §6.

### Phase B — `internal/repository/library/tracks.go`

Names match the existing test files (`tracks_cue_*`, `tracks_review_*`, `tracks_in_use_*`, `tracks_stats_*`, `tracks_sort_*`).

| New file | Contents |
|---|---|
| `tracks.go` | `Tracks`, `NewTracks`, `WithTx`, `trackEditColumns`, `trackWrite`, `Create`, `Update`, `SetPlaylists`, `Delete` |
| `tracks_list.go` | `trackRow`, `trackOrder`, `trackWhere`, `notIf`, `whereClause`, `List`, `count`, `trackStatsRow`, `Stats`, `Get`, `withLinks`, `issues` |
| `tracks_usage.go` | `trackUserRow`, `Users`, `DeleteUnreferenced`, `Blockers` |
| `tracks_review.go` | `SetReviewed`, `Titles`, `titleRow`, `SetTitles` |
| `tracks_cue.go` | `CueState`, `SetCue`, `PlannedHours`, `MissedHours` |

Shared SQL constants (`selectTracks`, `fromTracks`, `usersOfTracks`) stay in `tracks.go`, or go to `tracks_sql.go` if `tracks_list.go` stays over ~350 lines.

### Phase C — `internal/rotation/service.go`

| New file | Contents |
|---|---|
| `service.go` | `Service`, `NewService`, `sync`, `RunJob`, `startedHour`, `startedOffset`, `clock` |
| `ports.go` | `Store`, `ClockSource`, `GridSource`, `Operations`, `Publisher`, `TxRunner` |
| `plan_types.go` | `InsertError`, `PlanHour`, `PlannedPlaylist`, `PlannedItem`, `PlanItemRef`, `PlannedHour`, `Range`, `SyncResult` |
| `settings.go` | `PlanSettings`, `frozen`, `SettingsFunc`, `planHorizon` |
| `service_hour.go` | `hourJob`, `hourOutcome`, `build`, `buildTx`, `settleInserts`, `report`, `clear` |
| `service_plan.go` | `Plan`, `checkRange`, `validationQuery`, `locationName` |

Check `rotation.go`, `snapshot.go`, `inputs.go`, `planner.go` first: if plan types already live in one of them, put the types from `plan_types.go` there instead.

### Phase D — `internal/rotation/build.go` (last, optional)

Pure algorithm, no I/O, well covered (`build_test.go`, `simulate_test.go`, `fullpass_test.go`). Do it only after A–C pass.

| New file | Contents |
|---|---|
| `build.go` | `Build`, `refusedClaim`, `buildHour`, `newBuilder`, constants |
| `build_state.go` | `kind`, `hard`, `anchor`, `soft`, `blockItem`, `fillSource`, `builder`, `slotClaims` |
| `build_claims.go` | `index`, `indexClaims`, `claimSlot`, `spreadCount`, `avoidClaimed` |
| `build_anchors.go` | `opening`, `anchors`, `templateTrack`, `hourTrack`, `hourItem`, `refuse`, `endBlock`, `dropBlock`, `ruleMissing`, `placeAnchor`, `placeStartInsert`, `placeStartRule` |
| `build_soft.go` | `softAnchors`, `nearestSlot`, `nextSoft`, `popSoft`, `placeSoft` |
| `build_fill.go` | `fillTo`, `drawFill`, `bestFit`, `closest`, `place` |
| `build_helpers.go` | `burnDraw`, `at`, `allowedOf`, `fillAllowed`, `clip`, `abs`, `clock`, `clockTime`, `warn`, `warnRule` |

## 4. Acceptance

- Each phase: build, vet, unit tests and (phase B) integration tests pass; `golangci-lint run` reports nothing new.
- `git diff --stat` per commit shows lines moved, not changed: total added ≈ total removed (plus package headers and imports).
- No exported API change: `go doc` of each package lists the same symbols before and after.

## 5. Follow-up tasks (not in this plan, separate plans)

1. **Guards into the domain.** `guardRetention` and `encoderLimitCheck` are business rules (design §4.1, §6.3) living in `cmd`. Move them into their domain packages with domain tests; `main` only calls them.
2. **Complex SQL from `gorm.Raw` to pgx** (project rule: GORM for simple CRUD, pgx for complex queries). Candidates in `tracks*.go`: `List` (CTE, `count(*) OVER ()`, dynamic WHERE/ORDER), `Stats`, `Blockers` and `usersOfTracks` (UNION), `DeleteUnreferenced` (`DELETE ... RETURNING`), `PlannedHours`, `MissedHours`. Simple `Raw` in `SetReviewed` and `Delete` stays. Changes `WithTx` and tests.
3. **`buildTx` (~100 lines)** in `service_hour.go` may be split into sub-functions; that is a refactor, not a move.

## 6. Deviations

1. **A2 skipped.** `serve` interleaves construction with `defer`s (`logs.Close`, `database.Close`, `cancelRequests`) and early error returns; splitting it into `buildServices`/`runServices` needs a new struct or many return values and moved defers, more than passing already-built values.
2. **Phase A, schema checks.** `checkSchema` and `guardSchema` live in `migrate.go` (same topic: schema/migrations), so they stay; no `schema_guard.go`. The `compatRetry` constant stays in `serve.go`, its only user.
3. **Phase A, unlisted declarations.** `operationsRetentionDays` and the `EmergencyPlaylistID` method went to `wiring_settings.go` with `retentionPeriods` and `emergencySetting`. `shutdownTimeout`, `operationWorkers`, `operationQueue` stay in `serve.go`.
4. **Phase B, `tracks_sql.go`.** `tracks_list.go` came out at 363 lines (over ~350), so the shared SQL constants `selectTracks`, `trackSelect`, `planUsage`, `fromTracks`, `usersOfTracks` went to `tracks_sql.go`; `tracks.go` keeps only the constructor and writes, as the amended `CLAUDE.md` rule says. `trackSortKeys` and the `condTrack*` constants went to `tracks_list.go` (their users `trackOrder`, `trackWhere`, `Stats`); `TableName` stays with `trackWrite` in `tracks.go`.
5. **Phase C, names.** `ports.go`, `settings.go`, `plan_types.go`, `service_hour.go`, `service_plan.go` did not exist in `internal/rotation`; created as planned. `rotation.go`, `snapshot.go`, `inputs.go`, `planner.go` hold none of the plan types, so `plan_types.go` was created. Methods and constants not listed moved with their type: `InsertError.Error`, `InsertError.FailureReason`, `PlanHour.current`, `planItemPlanned`, `Range.contains`, `SyncResult.String` to `plan_types.go`. The error variables (`ErrHourStarted`, ...) and the timeout constants stay in `service.go`.
6. **Phase D.** The `kind*` constants went to `build_state.go` with `kind`; the size constants (`minuteMS`, `middleTargetMS`, `middleWindowMS`, `minAirMS`) stay in `build.go`.

## 7. Open questions

1. **"One file per aggregate" (backend `CLAUDE.md`, Repository layer).** Phase B splits the `tracks` aggregate into `tracks*.go` files, which the rule as written does not allow. **Decided 2026-11-13: option (a)**, amend the rule (§2 item 7); option (b), skipping Phase B, was rejected.
