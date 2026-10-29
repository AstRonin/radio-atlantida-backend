# Phase 15 Plan — Log Search (Loki)

Status: Implemented (2026-10-15); the §6 checks with a running backend (stopping liquidsoap, media playing) are still to be done
Superseded in part by logs-loki-plan.md (2026-10-29): the backend pushes its log to Loki, Alloy collects only the playout journal, Loki and Alloy are core services.
Based on: `tech-design.md` (§7.1 playout journal, §9, §10.1, §10.3, §10.4, decision 30), `docs/phase-14-plan.md` (Grafana, the `monitoring` profile), deploy plan (`radio-atlantida-deploy/docs/deploy-init-plan.md`: D1, D2, D4 step 10a, D5, decision 8.12)
Last updated: 2026-10-15

## 1. Goal

The Grafana of Phase 14 also searches the logs: the operator filters the
backend's entries by level, component, and text, sees how many errors and
warnings each component logged over time, and reads the playout journal
(track starts and ends, Icecast output changes) next to them. No alerts,
no change to the Go code of the backend (§10.4).

Phase 14 must be done first: this phase adds two services to its
`monitoring` profile, a second data source, and a second dashboard.

## 2. Decisions (2026-10-15)

1. **Source: the files on the host.** Alloy reads `LOG_ROOT/app-*.log`
   and `PLAYOUT_JOURNAL_ROOT/journal-*.jsonl`, both mounted read-only. Not
   container stdout: each entry is collected once, and the files survive
   container recreation (§10.4).
2. **The playout journal is collected too.** It puts what aired next to
   what the backend logged.
3. **Retention 10 days**, like the files: Loki `retention_period: 240h`
   through the compactor, and `reject_old_samples_max_age: 240h`, so a
   first start loads the files still on disk.
4. **No alerts.** Grafana alerting stays unconfigured (§10.4).
5. **Loki: one process, filesystem storage.** `-target=all`, schema
   `v13` (TSDB index, filesystem object store), `auth_enabled: false`,
   `common.replication_factor: 1`, in-memory ring. Volume `loki-data`.
   Not published: only Grafana and Alloy reach it, on the compose network.
6. **Alloy pipeline.** For each source: `local.file_match` (glob,
   `sync_period: 10s`, so a new daily file is found) →
   `loki.source.file` with `file_watch` polling (`min_poll_frequency`,
   `max_poll_frequency` about 1 s), because file change notifications do
   not cross the WSL2 layer reliably → `loki.process`: `stage.json`,
   `stage.timestamp` from `time` (`RFC3339Nano`; the journal writes
   milliseconds, the backend nanoseconds, both parse), `stage.labels`,
   static `job` → `loki.write` to `http://loki:3100`. Positions in the
   volume `alloy-data` (`--storage.path`), so a restart resumes where it
   stopped. New files are read from the start.
7. **Labels.** Backend: `job="backend"`, `level` (`DEBUG`, `INFO`,
   `WARN`, `ERROR`), `component` (the 11 values of §10.1). Journal:
   `job="playout-journal"`, `event` (`start`, `end`, `output`), `source`.
   Everything else (`request_id`, `err`, `item_id`, `track_id`, `uri`,
   `filename`) stays in the line and is filtered with `| json` in
   queries. A field is never made a label because it is useful to search
   by; only small closed sets are labels.
   *Implementation (2026-10-15):* both sources also carry `file`, the
   daily file's name (Alloy's own `filename` label, the full path, is
   dropped). Loki refuses a line more than `max_chunk_age / 2` (1 h) older
   than the newest line of its stream ("too far behind"), so without it
   yesterday's file, read after today's on a first start, is lost;
   `pipeline_test.go` shows this. Loki's `discover_service_name` and
   `discover_log_levels` are off, so no other label or metadata is added.
8. **Data source and dashboard.** Loki data source `uid: radio-logs`,
   provisioned next to `radio-db`. Dashboard "Logs" (§3), same provider
   (`allowUiUpdates: false`). Unlike the database dashboard, it uses
   Grafana variables and the `$__interval` / `$__range` macros: LogQL
   needs a time range.
9. **One switch.** Loki and Alloy belong to the `monitoring` profile in
   dev and to `MONITORING_ENABLED` in production, with Grafana. No
   separate switch for logs.
10. **Dev only: Alloy's UI** on `127.0.0.1:12345` to inspect the pipeline
    (components, positions, errors). Production does not publish it.
11. **Images.** `grafana/loki` and `grafana/alloy`, exact tags chosen at
    implementation (current stable, at least two weeks old), recorded in
    CLAUDE.md; the deploy repository uses the same tags. Health checks:
    Loki `/ready` (3100), Alloy `/-/ready` (12345), with a tool the image
    contains (checked on the chosen tags).
    *Implementation (2026-10-15):* `grafana/loki:3.7.8` and
    `grafana/alloy:v1.20.1`. The Loki image is distroless (only
    `/usr/bin/loki`: no shell, wget, or curl; 3.4 and 3.5 have only `sh`)
    and `loki` has no health subcommand, so `loki` has **no health check**
    (decided with the owner); `alloy` and `grafana` depend on it
    `service_started`. Alloy has bash but no wget/curl: its check sends
    `GET /-/ready` over `/dev/tcp`.
12. **Not collected:** stdout of the containers (`liquidsoap`, `icecast`,
    `db`, …). That needs the Docker socket; a later step (§10.4).

## 3. Dashboard "Logs"

Default range: last 6 hours. Variables: `level` (multi, from the label
values), `component` (multi, from the label values), `search` (text box,
empty = no filter).

| Panel | Type | Query (outline) |
|-------|------|-----------------|
| Errors and warnings by component | time series, stacked | `sum by (component) (count_over_time({job="backend", level=~"ERROR\|WARN"} [$__interval]))` |
| Errors in range | stat | `sum(count_over_time({job="backend", level="ERROR"} [$__range]))` |
| Backend log | logs | `{job="backend", level=~"$level", component=~"$component"} \|= "$search"`; newest first; the line shows `msg`, details show all fields |
| Icecast output | state timeline | `{job="playout-journal", event="output"} \| json \| line_format "{{.connected}}"` |
| Tracks started per hour | time series (bars) | `sum by (source) (count_over_time({job="playout-journal", event="start"} [1h]))` |
| Playout journal | logs | `{job="playout-journal"} \|= "$search"` |

## 4. Files

| File | Change |
|------|--------|
| `dev/loki/loki.yaml` | New: decisions 3, 5 |
| `dev/alloy/config.alloy` | New: decisions 6, 7 |
| `dev/alloy/pipeline_test.go` | New: integration test (§5, task 3) |
| `dev/grafana/provisioning/datasources/loki.yaml` | New: data source `radio-logs` |
| `dev/grafana/dashboards/logs.json` | New: the dashboard of §3 |
| `docker-compose.dev.yml` | Services `loki`, `alloy` (profile `monitoring`), volumes `loki-data`, `alloy-data`; `grafana` depends on `loki` started (decision 11) |
| `Makefile` | `monitoring-up` / `monitoring-down` include `loki` and `alloy`; `monitoring-reset` also removes `loki-data` and `alloy-data` |
| `.github/workflows/ci.yml` | `loki -verify-config` and `alloy fmt` in the pinned images |
| `CLAUDE.md` | Layout (`dev/loki/`, `dev/alloy/`), pinned images, dev stack note (Grafana at 3000, Alloy UI at 12345) |

## 5. Tasks

1. **Loki configuration** per decisions 3 and 5. Keep the defaults
   elsewhere; write a comment for every value that differs from the
   default and why. *Found in the test:* with `-target=all` the querier
   asks the ingester only about the last ~2.5 h
   (`ingester.query_store_max_look_back_period` defaults to
   `max_chunk_age` + 30 min), so lines loaded with an older time stayed
   invisible until their chunk was flushed; it is set to `-1`.
2. **Alloy configuration** per decisions 6 and 7, one pipeline per
   source. Comments explain the polling and the label choice.
3. **Integration test** (`//go:build integration`, testcontainers-go
   generic containers, the pinned images): start Loki with
   `dev/loki/loki.yaml` and Alloy with `dev/alloy/config.alloy` on one
   network, with two temporary folders mounted as `/logs` and `/journal`.
   Then, table-driven:
   - a backend line (`ERROR`, `component: outage`, `request_id`) in
     today's `app-*.log` → a Loki query `{job="backend",
     component="outage", level="ERROR"}` returns it with the entry's
     `time` as its timestamp; `request_id` is not a label (label API);
   - a journal `start` line → `{job="playout-journal", event="start"}`;
   - a line in yesterday's file → accepted with yesterday's time
     (old samples within 10 days);
   - a new file for the next day created after start → collected within
     the `sync_period` plus polling (wait with a deadline, no fixed
     sleep);
   - Alloy restarted → no line is collected twice (count before and
     after).
   Queries go through Loki's HTTP API (`/loki/api/v1/query_range`,
   `/loki/api/v1/labels`) with `t.Context()` deadlines; `testify/require`.
4. **Data source and dashboard.** Build "Logs" in the Grafana UI against
   the dev stack, export it, remove instance fields, set the data source
   uid to `radio-logs`. Extend the Phase 14 test that checks data source
   uids so it covers `logs.json` too (its LogQL is not run there).
5. **Compose.** `loki`: pinned image, `-config.file=/etc/loki/loki.yaml`,
   volume `loki-data`, health check, no ports. `alloy`: pinned image,
   `run --storage.path=/var/lib/alloy/data --server.http.listen-addr=0.0.0.0:12345 /etc/alloy/config.alloy`,
   `${LOG_ROOT}:/logs:ro`, `${PLAYOUT_JOURNAL_ROOT}:/journal:ro`, volume
   `alloy-data`, port `127.0.0.1:12345:12345`, health check, depends on
   `loki` healthy. `grafana` gains `depends_on: loki` healthy. All in
   profile `monitoring`. *Implemented without a Loki health check, so both
   depend on `loki` started (decision 11).*
6. **Makefile, CI, CLAUDE.md** as in §4.
7. **Manual check on the dev stack** (§6).

## 6. Done when

- `make test-integration` passes with the pipeline test; `make lint` is
  clean; CI validates the Loki and Alloy configuration.
- On the dev stack: `make stack-up && make monitoring-up && make run` →
  the "Logs" dashboard shows the backend's entries; filtering by level,
  component, and text works; stopping `liquidsoap` produces
  `liquidsoap` and `outage` warnings visible in "Errors and warnings by
  component", and the journal shows the `output` change.
- With media playing, "Tracks started per hour" and "Playout journal"
  show the starts and ends.
- `make monitoring-down` stops Grafana, Loki, and Alloy, and nothing else.
- `loki` publishes no port; `alloy` publishes only `127.0.0.1:12345`.
- After `make monitoring-reset && make monitoring-up`, the files still on
  disk are loaded again.

## 7. Deploy repository (after this phase)

Not part of this repository's PR (deploy plan 8.12): copy `dev/loki/`,
`dev/alloy/` (without the test), `dev/grafana/provisioning/datasources/loki.yaml`,
and `dev/grafana/dashboards/logs.json` into `config/`; add `loki` and
`alloy` with `LOKI_IMAGE` and `ALLOY_IMAGE`, no published ports; extend
step 10a and the CI checks. **Acceptance on the Windows machine:** new
lines, the daily file switch at 00:00 UTC, and the deletion of files
older than 10 days are seen by Alloy through the WSL2 bind mounts (the
risk of decision 6 that macOS cannot show).

## 8. Known limits

- Container stdout is not searchable in Grafana (decision 12); Docker
  Desktop's own log view or `docker compose logs` covers it.
- An entry dropped by the backend's non-blocking logger (§10.1) is
  missing from the file, and so from Loki.
- Lines older than 10 days are refused, so files kept longer by a larger
  `retention.log_days` are not loaded completely.
- Loki and Alloy add about 150–400 MB of memory in the WSL2 VM, on top of
  Grafana; `MONITORING_ENABLED=false` frees all of it.
