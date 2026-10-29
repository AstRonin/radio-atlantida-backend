# Logs in Loki: plan

Status: Approved in principle by the owner (2026-10-28); to be copied into the backend repository after the final check
Based on: `tech-design.md` (§7.1, §10.1, §10.4, decision 30), `docs/phase-15-plan.md` (Loki and Alloy, files as the source), `docs/phase-14-plan.md` (Grafana, `monitoring` profile), `radio-atlantida-frontend/docs/f11-logs-plan.md` (logs page), `radio-atlantida-frontend/docs/f13-grafana-link-plan.md`
Companion: `frontend/docs/logs-loki-plan.md`
Last updated: 2026-10-28

## 1. Goal

The backend keeps no application log files and does no log rotation. The backend sends its log lines straight to Loki over HTTP, the same way on a laptop (`make run`) and in a container. Loki is the only store of the backend's log. The manager reads it on the Logs page of the panel (through a new backend endpoint that queries Loki) and in Grafana.

Alloy stays, but only for the playout journal, which Liquidsoap writes to a file (decision 2).

## 2. Current state (verified read-only, 2026-10-28)

- `internal/logging`: the slog handler writes to stdout and to `app-YYYY-MM-DD.log` through `DailyRotator`. The rotator deletes files older than `retention.log_days` (admin setting, default 10). The async writer drops entries when its buffer (4096) is full (§10.1).
- The Logs page reads the files through `GET /api/logs/files` and `GET /api/logs/files/{name}` (`internal/logging/files.go`, `handlers/logs.go`). It has no other source.
- Alloy reads `LOG_ROOT/app-*.log` and `PLAYOUT_JOURNAL_ROOT/journal-*.jsonl` into Loki (`dev/alloy/config.alloy`). Container stdout is not collected (phase 15, decision 12).
- Loki (`loki:3.7.8`, one process, filesystem, `retention_period: 240h`) and Alloy are in the `monitoring` profile together with Grafana (`MONITORING_ENABLED` in production).
- A local run (`make run`) is found by Alloy only because it writes to the file under `LOG_ROOT`, which Alloy mounts. Nothing else reaches Loki from the host.
- The playout journal (`journal-YYYY-MM-DD.jsonl`) is written by Liquidsoap, not by the backend. The backend replays it for reconciliation (`internal/outage/journal.go`, §7.1). It is not an application log.

## 3. Decisions

1. **The backend pushes its log to Loki directly.** A slog handler keeps the JSON lines on stdout and also sends the same entries to Loki's push API (`POST /loki/api/v1/push`, JSON, standard library only). The same code path runs on the host and in the container. No Docker API, no Docker socket, no Docker-side collector for the log.
2. **The playout journal stays a file, and Alloy stays for it.** Liquidsoap writes it and the backend replays it. Moving it needs a change to the Liquidsoap script and the replay code, which is a separate phase. `PLAYOUT_JOURNAL_ROOT` and the journal's Alloy pipeline stay.
3. **Labels.** `job="backend"`, `level`, `component`. `component` is always set: an entry without a component gets `component="none"`, so the dashboard's `component=~"$component"` matches every line (this also fixes the dashboard issue found on 2026-10-28, where lines without a component were hidden). The `file` label is gone with the daily files. Other fields stay in the line and are filtered with `| json`.
4. **Loki and Alloy are not behind the `monitoring` profile.** The Logs page of the panel needs Loki, and Alloy is needed for the journal. Grafana stays in `monitoring`. This changes phase 15's decision 9 ("one switch") and §10.4: with `MONITORING_ENABLED=false` the panel keeps its logs and only Grafana is off.
5. **Retention is deploy configuration.** Loki `retention_period: 240h` (10 days, as today) in `dev/loki/loki.yaml` and the deploy copy. Loki reads its configuration at start, so it cannot be an admin setting. The admin setting `retention.log_days` is removed (§4.4).
6. **No limit on the search window in the backend or the frontend.** A range longer than the retention returns no lines for the part that is gone. Loki's own query limits still apply (its `max_query_length`); when Loki refuses a query for that reason, the backend returns 422 with the code `range_too_long` (§4.2).
7. **The panel reads Loki through the backend.** `GET /api/logs` (§4.2). The browser never talks to Loki. Loki publishes no port in production; in development it is published on `127.0.0.1:3100` only, so `make run` on the host can push to it.
8. **The total count is a separate query.** The page gets its entries from one request, and `total` comes from a second request (`count_over_time` over the same range and filters). If the count is slow or fails, `total` is left out of the response, and the entries are still shown.
9. **Transition in three steps, each one working on its own** (§8).

## 4. Backend changes

### 4.1 Logging and the push

- `internal/logging`: the handler writes to stdout as today and also hands each entry to a Loki sink. Keep the async writer, the masking of `password|secret|token|credential`, and the component loggers.
- The Loki sink is a small type in `internal/logging`:
  - a bounded buffer (a named constant, with a comment why); when it is full, the entry is dropped and a counter of dropped entries goes to the stdout log once per minute, so the loss is visible without blocking the logger (CLAUDE.md: logging never blocks);
  - a single goroutine owned by the sink (`errgroup` in `serve`), which sends a batch every second or when the batch reaches a size limit, with a per-request deadline (`context.WithTimeout`, a few seconds) and `http.NewRequestWithContext`;
  - on failure the batch is retried a few times with backoff and then dropped with a counted loss; the backend never waits for Loki;
  - on shutdown the sink flushes what is buffered, with a timeout, through `context.WithoutCancel` (CLAUDE.md, the cleanup rule).
- Labels are sent as in §3.3. Lines are sent with the entry's `time` as the timestamp (nanoseconds).
- `internal/config`: remove `LOG_ROOT`. Add `LOKI_URL`. An empty value turns the push off (tests and a backend run without the dev stack). The compose file and `.env.example` set it to `http://loki:3100` in containers and `http://127.0.0.1:3100` on the host.
- `cmd/backend/serve.go`: remove the file reader wiring; wire the Loki sink (§4.1) and the log search service (§4.2).
- Remove the daily rotator, its tests and the file reader.

### 4.2 Log search endpoint

Replaces `GET /api/logs/files` (`list-log-files`) and `GET /api/logs/files/{name}` (`read-log-file`) with one operation, `GET /api/logs` (`list-logs`).

Query parameters:

| Name | Type | Rule |
|------|------|------|
| `from`, `to` | date-time, required | `from < to`. No other limit in the backend (§3.6) |
| `level` | enum `DEBUG`, `INFO`, `WARN`, `ERROR` | minimum level; the backend expands it to a regex on `level` |
| `component` | enum (the 10 components of §10.1; `none` is not a filter value) | equality on the label |
| `search` | string, 1 to 200 characters | text match on the decoded `msg` field, case-insensitive substring: `\| json \| msg=~"(?is).*…*."`, the text escaped with `regexp.QuoteMeta` and then for LogQL (§4.2 rules) |
| `request_id` | string, pattern of the request ID | `\| json \| request_id="…"` |
| `limit` | int, default 100, 1 to 500 | below Loki's default maximum of 5000 lines per query |
| `cursor` | string, opaque | the time of the last returned entry, with the direction backward; from the previous page's `next_cursor` |

Response `LogEntryPage`: `items` (the same `LogEntry` as today: `time`, `level`, `message`, `component`, `request_id`, `error`, `attrs`), `next_cursor` (absent on the last page) and `total` (optional, §3.8).

Errors: 422 with field codes in `errcodes`, named `query.<parameter>` as the other endpoints do (`query.from`, `query.to`, `query.search`, `query.request_id`, `query.limit`, `query.cursor`, `query.level`, `query.component`); 422 `range_too_long` when Loki refuses the query for its own length limit; 503 `logs_unavailable` when Loki does not answer within the deadline. New codes are added to `errcodes` in the same change, and `problemFor` maps them.

Rules for the query:

- LogQL is built only from validated enums and from the pattern-checked request ID. User text never goes into a label matcher.
- `search` matches the decoded `msg` field, not the raw JSON line: a line filter on the raw line would match almost every entry (JSON keys are quoted) and would need the escaped form of a quote or backslash. Loki anchors label-filter regexes, so the pattern is `(?is).*<regexp.QuoteMeta(search)>.*`: case-insensitive, the dot also crosses line breaks, and the match is a substring. The pattern is then escaped for the LogQL string (`strconv.Quote` escapes backslash, double quote and control characters).
- An empty `search` means no filter (decision 8 in §10).
- Every call has its own deadline (`context.WithTimeout`, 5 s) and is made with `http.NewRequestWithContext`.
- Entries are read with `direction=backward` and `end` set from the cursor, so pages go back in time and no page is skipped or repeated when new lines arrive.
- The count (`total`) is a second call with its own, shorter deadline; it is left out when it does not finish.

Layout: a domain package `internal/logsearch` (the query model, validation, the LogQL builder, the cursor) with a small interface for Loki that the package defines itself (`QueryRange(ctx, params) ([]Stream, error)`, `Count(ctx, params) (int64, error)`). The Loki client is in the same package (plain HTTP, no SQL), so the architecture test does not change. The handler `internal/httpapi/handlers/logs.go` only decodes, validates and calls the service.

Tests: unit tests for validation, the LogQL builder (escaping, enum expansion), the cursor and the push batching; integration tests against the pinned Loki image (testcontainers, `dev/loki/loki.yaml`): lines pushed through the push API are returned by each filter, paging crosses a page boundary, the count matches, and a stopped Loki gives 503.

### 4.3 Docs and API

- `make openapi`; the frontend syncs `api/openapi.json` in the same change as its code (CLAUDE.md of the frontend).
- `tech-design.md` §10.1 (the file contract and the rotation rule are removed; the push replaces them), §10.4 (the backend pushes the log; Alloy collects only the journal; Loki and Alloy are core services), decision 30, §3 and §9 (compose services and host paths: `LOG_ROOT` is gone, `LOKI_URL` is added).
- Backend `CLAUDE.md`: layout (`internal/logsearch`), commands (`monitoring-up` no longer starts Loki and Alloy), environment (`LOG_ROOT` removed, `LOKI_URL` added), dev stack notes (the log files paragraph and the Alloy paragraph).
- `docs/phase-15-plan.md`: status line says the file source and the Alloy log pipeline are superseded by this plan.

### 4.4 Admin setting `retention.log_days`

Remove the key from `internal/settings` (registry, defaults, validation, the `SetRetentionDays` call from the logger). Check how the settings service treats a stored key that is no longer in the registry (ignored or rejected) before the key is removed, and write the answer in the change. The retention of the play log and operations is not affected.

## 5. Alloy, Loki and deploy

### 5.1 Alloy

- Remove the app-log pipeline (`local.file_match` on `LOG_ROOT`).
- Keep the journal pipeline unchanged (`PLAYOUT_JOURNAL_ROOT`, job `playout-journal`).
- Positions stay in `alloy-data`, as in phase 15.

### 5.2 Loki

- `dev/loki/loki.yaml` unchanged except for comments.
- In development the Loki port is published on `127.0.0.1:3100` (so `make run` can push). In production it is not published.
- The backend does not wait for Loki: it keeps writing stdout and retries the push (§4.1).

### 5.3 Docker's own copy of the log (optional)

In the container, Docker's log driver keeps stdout too. The backend service may set `logging: {driver: json-file, options: {max-size: 10m, max-file: 3}}` so that Docker's copy stays bounded. This is Docker's rotation, not the application's. It is optional: Loki is the store.

### 5.4 Compose and deploy repository

- Dev `docker-compose.dev.yml`: `loki` and `alloy` leave the `monitoring` profile; Loki publishes `127.0.0.1:3100`; `backend` gets `LOKI_URL=http://loki:3100` and loses the `LOG_ROOT` mount; the Grafana service keeps its profile and its `depends_on: loki` (started).
- `.env.example`: `LOKI_URL=http://127.0.0.1:3100` for `make run`, and `LOG_ROOT` removed.
- Deploy repository (`radio-atlantida-deploy`): the same services, `LOKI_IMAGE` and `ALLOY_IMAGE` with the pinned tags, no published ports, the backend's `LOG_ROOT` mount removed and `LOKI_URL` set, the config copies (`config/loki/`, `config/alloy/`), and deploy plan step 10a and the CI checks updated. `tech-design.md` is a shared document: the backend version is canonical and is pushed to deploy.

## 6. Frontend

Companion plan: `frontend/docs/logs-loki-plan.md`. In short: the file select becomes a time range; the popup link builds a time window around the alert; the 404 handling becomes the 503 handling; `next_cursor` replaces the offset; `total` is shown only when the backend sends it.

## 7. Dashboards

The "Logs" dashboard (`dev/grafana/dashboards/logs.json`) keeps its labels and its queries. The `component=~"$component"` filter now matches every line, because `component` is always set (§3.3). Its `file` label references, if any, are removed. The journal panels do not change. Dashboards are edited in the repo (provisioned, `allowUiUpdates: false`).

## 8. Order of work

1. **Push first, files still written.** The Loki sink is added and the backend sends each line to Loki with `job="backend-push"`, while the file and Alloy's file pipeline still write and read `job="backend"`. Check that the lines in `job="backend-push"` match the lines of `job="backend"` for one hour of traffic. Duplicates are avoided by the different job label.
2. **Read path.** `GET /api/logs` on Loki, `api/openapi.json` regenerated, the frontend logs page switched to it (companion plan). The file endpoints still exist and are removed in step 3.
3. **Removal.** The sink's job label becomes `backend`. Remove the file writer and rotator, the file reader and endpoints, `LOG_ROOT`, the `retention.log_days` setting, and the app-log pipeline of Alloy. Move Loki and Alloy out of the `monitoring` profile in dev and deploy. Update the docs (§4.3).

Each step is one change that passes all checks. No database migration is needed; the settings key removal is checked as in §4.4.

## 9. Done when

- `make run` on the Mac (no container) shows the backend's lines on the Logs page of the panel and in Grafana; filters by time range, level, component, text and request ID work, and paging continues through `next_cursor`.
- The stack in containers shows the same lines.
- `grep -rn "LOG_ROOT\|app-.*\.log\|DailyRotator\|retention.log_days"` finds nothing in the backend, the frontend, and the deploy repository (except this plan and the phase 15 history).
- With Loki stopped, the backend keeps running, the Logs page shows "logs unavailable", and the dropped-line counter is written to the log; after Loki starts again, new lines are pushed. Lines dropped while Loki was down are not restored (accepted, §3.1); they stay on stdout.
- The playout journal still loads and the outage reconciliation test passes unchanged.
- Entries older than 10 days are deleted by Loki's compactor (checked on the dev stack with a short retention in a test config).
- `make test`, `make test-integration`, `make lint`, `make openapi-lint` pass; the Alloy and Loki configuration checks in CI pass; the frontend checks pass.
- **Acceptance on the Windows machine** (the risk named in phase 15 §7): the stack starts with `MONITORING_ENABLED=false` and the Logs page works, the push reaches Loki from the container, and Alloy reads the journal through the WSL2 bind mount.

## 10. Decisions taken (2026-10-28)

1. The playout journal stays a file; Alloy stays for it.
2. Loki and Alloy are core services, outside the `monitoring` profile; Grafana stays optional.
3. The total count is a separate query, optional in the response.
4. The `retention.log_days` setting is removed; Loki's retention is deploy configuration (10 days).
5. Collection is a direct push from the backend, not the Docker API and not a Docker socket proxy. This removes the Docker access question of phase 15 §12.
6. No limit on the search window in the backend or the frontend; Loki's own query limit can still refuse very long ranges (422 `range_too_long`).
7. **Text search matches the decoded `msg` field**, case-insensitive, not the raw line (§4.2).
8. **Field error codes use the `query.<parameter>` form** of the other endpoints, for every parameter of `/api/logs`.
9. **Ten components**, as in §10.1 and the code. The earlier "11" was a mistake in this plan.
