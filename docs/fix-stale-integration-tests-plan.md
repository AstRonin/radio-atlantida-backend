# Plan — Fix Three Stale Integration Tests (radio-atlantida-backend)

Status: Approved by the operator (2026-11-10); documents of §3.4 updated, tests not started
Based on: `docs/tech-design.md` (import API), `docs/phase-15-plan.md`, `docs/phase-18-plan.md`
Last updated: 2026-11-10

## 1. Goal

`make test-integration` fails on three tests. None of them is a product bug: two tests were not updated after a deliberate change, and one depends on the current date. After this change the whole suite passes, and the documents describe the current behaviour.

## 2. Decisions

1. **Only tests change in the code.** No product code and no Loki or Alloy configuration changes.
2. **Logs Drilldown stays.** `discover_service_name: [job]` and `discover_log_levels: true` in `dev/loki/loki.yaml` are deliberate (`f8d515e`). The test accepts `service_name` and `detected_level`.
3. **The import summary keeps its analysis part.** `; analysis: ...` (`internal/ingest/import.go:170-171`) is deliberate and is now written down.
4. **The documents are changed in the docs repository** and pushed by the `docs` subagent. The implementing agent does not edit `docs/`.

## 3. Work

### 3.1 `dev/alloy/pipeline_test.go` (`TestPipeline`)

- Add `"service_name": "playout-journal", "detected_level": "unknown"` to the three `wantLabels` maps (lines 66–68, 76–78, 90–92).
- Change the label list at line 118 to `[]string{"event", "file", "job", "service_name", "source"}`. `detected_level` is structured metadata and is not returned by `/labels`.

### 3.2 `internal/httpapi/rotation_simulate_integration_test.go` (`TestRotationSimulate`)

Replace the fixed `from=2026-11-01&to=2026-12-01` at line 96 with a range that starts after the test's own `date`, so it holds no grid hour on any day:

```go
d, err := time.Parse(time.DateOnly, date)
require.NoError(t, err)
emptyFrom := d.AddDate(0, 0, 1)
emptyTo := emptyFrom.AddDate(0, 0, 30)
f.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("%s?from=%s&to=%s", path, emptyFrom.Format(time.DateOnly), emptyTo.Format(time.DateOnly)), nil, &sim)
```

The range is 31 days inclusive in every month. Keep the assertion and its message.

### 3.3 `internal/httpapi/ingest_integration_test.go` (`TestImportFolder`)

| Line | Expected summary |
|------|------------------|
| 105 | `folder jingles: imported 2; analysis: analysed 2` |
| 125 | `folder jingles: imported 1, in_library 2; analysis: analysed 1` |
| 131 | `folder music: imported 3, skipped 1; analysis: analysed 3` |
| 161 | `whole media folder: imported 1, in_library 6, duplicates 1, skipped 1; analysis: analysed 1` |

This test returns early without ffmpeg, so run it where ffmpeg is installed, or in the backend container. If ffmpeg is not available, say so: the assertions were then not exercised.

### 3.4 Documents (docs repository)

- `tech-design.md`, `POST /api/import`: after "the summary starts with the folder", add that the import counts are followed by `; analysis: ...`, the counts of the analysis of the new tracks (`internal/ingest/import.go:169-171`).
- `backend/docs/phase-18-plan.md`, the summary paragraph of decision 7: the same addition.
- `backend/docs/phase-15-plan.md`, decision 7: replace the sentence saying `discover_service_name` and `discover_log_levels` are off. New text: `discover_service_name` is `[job]`, so `service_name` is one-to-one with `job` and adds no streams, and `discover_log_levels` is on, which adds `detected_level` as structured metadata, not an index label. Both are for the Logs Drilldown app.

Push `tech-design.md` to backend, frontend, and deploy, and `phase-15-plan.md` and `phase-18-plan.md` to backend.

## 4. Tests

The change is to the tests themselves. Each of the three must pass, with `-race -count=1`, and fail if its expected value is put back.

## 5. Done when

- `make test`, `make test-integration`, `make lint` pass, with no failing test left in any package.
- `git diff` touches only the three test files.
- The documents of §3.4 are updated and pushed.
