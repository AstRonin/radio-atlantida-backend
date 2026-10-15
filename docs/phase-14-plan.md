# Phase 14 Plan — Database Monitoring (Grafana)

Status: Done (2026-10-15)
Based on: `tech-design.md` (§9, §9.2, §10, §10.3, decision 29), deploy plan (`radio-atlantida-deploy/docs/deploy-init-plan.md`: D1, D2 item 8, D4 steps 1, 9, 10a, D5, decision 8.12)
Last updated: 2026-10-15

## 1. Goal

The operator opens Grafana on the station machine, logs in, and sees what
PostgreSQL is doing now: connections by application, running queries and
their duration, blocked sessions, the oldest open transaction, database
and table sizes, dead tuples and autovacuum, cache hit ratio. No history,
no Prometheus, no change to the Go code of the backend (§10.3).

This phase builds and tests it in the dev stack of this repository. The
deploy repository then copies `dev/grafana/` into `config/grafana/` and
adds the production services (deploy plan D2, D4).

## 2. Decisions (2026-10-15)

1. **Live view only.** Grafana's built-in PostgreSQL data source reads
   the statistics views directly. Counters of `pg_stat_database` are shown
   as totals "since the statistics reset" (`stats_reset`), never as rates:
   without stored samples a rate cannot be computed.
2. **Login required.** One admin user, password `GRAFANA_ADMIN_PASSWORD`.
   Anonymous access and sign-up are off. Grafana applies the password only
   when its state volume is created; `make monitoring-reset` (dev) or
   `grafana cli admin reset-admin-password` (production runbook) changes
   it later.
3. **Read-only role `grafana_ro`.** `LOGIN`, `pg_monitor`,
   `CONNECTION LIMIT 3`, and role settings `statement_timeout = 5s`,
   `idle_in_transaction_session_timeout = 10s`,
   `default_transaction_read_only = on`. No grant on application tables.
   Not a migration: migrations seed only system rows and must not carry a
   password, and the role exists only where monitoring runs (§10.3).
4. **`role.sql` is plain SQL.** The password comes from the session
   setting `radio.grafana_password` (custom GUC), set by the caller:
   `PGOPTIONS='-c radio.grafana_password=…'` for `psql` in compose, a
   runtime parameter in the integration test. No `psql` meta-commands
   (`\gexec`, `:'var'`), so the test runs the same file through pgx. The
   script is idempotent: create the role if missing (`DO` block checking
   `pg_roles`), then always `ALTER ROLE … PASSWORD` (through `format('%L')`)
   and the settings, and `GRANT pg_monitor`. With the default
   `log_statement = none` the password statement is not logged; the
   PostgreSQL defaults of the stack are not changed.
5. **One-shot `grafana-role` service.** The `postgres` image (same tag as
   `db`) runs `psql -v ON_ERROR_STOP=1 -q -f /role.sql` as
   `POSTGRES_USER` (a superuser in the official image) and exits. `grafana`
   starts only after it succeeds. Changing `GRAFANA_DB_PASSWORD` and
   restarting the monitoring services is enough.
6. **Provisioning as files.** The data source (`uid: radio-db`, type
   `grafana-postgresql-datasource`, `maxOpenConns: 2`) and one dashboard
   ("Database", refresh 10 s) are provisioned read-only. The password and
   the database name reach the data source file through Grafana's
   environment interpolation. The dashboard provider has
   `allowUiUpdates: false`: a change is exported as JSON and committed.
7. **Dashboard queries are plain SQL.** No Grafana macros or template
   variables (`$__timeFilter`, `$db`); the database is
   `current_database()`. The integration test can then run every query as
   `grafana_ro`. Sessions are grouped by `application_name`
   (`radio-atlantida-backend`, `radio-atlantida-backend-migrate`, set in
   `internal/db`).
8. **Dev stack: profile `monitoring`.** `make stack-up` does not start
   Grafana; `make monitoring-up` does. Port `127.0.0.1:${GRAFANA_PORT:-3000}`.
   Dev passwords have defaults in the compose file
   (`${GRAFANA_ADMIN_PASSWORD:-dev-grafana-pass}`), like
   `LIQUIDSOAP_CONTROL_USER`, so an existing `.env` keeps working; they are
   also listed in `.env.example`. Production requires them (deploy plan
   D4, step 1).
9. **Offline machine.** `GF_ANALYTICS_REPORTING_ENABLED`,
   `GF_ANALYTICS_CHECK_FOR_UPDATES`, `GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES`,
   `GF_NEWS_NEWS_FEED_ENABLED` are `false`. No plugins are installed.
10. **Image.** Grafana OSS, an exact tag chosen at implementation: the
    current stable release, at least two weeks old. It is recorded in
    CLAUDE.md with the other pinned images; the deploy repository uses the
    same tag. The health check uses a tool the image contains (`wget` or
    `curl`, checked on that tag) against `/api/health`.
    *Chosen:* `grafana/grafana:13.2.3` (released 2026-09-29; has `wget` and
    `curl`). The OSS edition is published as `grafana/grafana`;
    `grafana/grafana-oss` received no release after 13.0.2 (June 2026).
11. **`pg_stat_statements` is out of scope.** It needs
    `shared_preload_libraries` and a restart of `db` (§10.3).

## 3. Dashboard "Database"

Each panel is one query. Thresholds are hints for the operator, not alerts.

| Panel | Type | Query (outline) |
|-------|------|-----------------|
| Server | stat | `version()`, `now() - pg_postmaster_start_time()` (uptime) |
| Connections | stat | client backends in `pg_stat_activity` against `current_setting('max_connections')` |
| Sessions by application | table | `application_name` (empty → `(none)`), `state`, `count(*)` for `datname = current_database()` |
| Running queries | table | `pid`, `application_name`, `state`, `wait_event_type`, `wait_event`, `now() - query_start`, `left(query, 300)`; `state <> 'idle'`, not the panel's own session; longest first |
| Oldest open transaction | stat | `max(now() - xact_start)`; warning above 30 s (the server `statement_timeout`) |
| Idle in transaction | stat | count of `state = 'idle in transaction'` |
| Blocked sessions | table | `pid`, `pg_blocking_pids(pid)`, waiting time, `left(query, 200)` where `cardinality(pg_blocking_pids(pid)) > 0` |
| Database size | stat | `pg_database_size(current_database())` |
| Largest tables | table | `pg_stat_user_tables`: `relname`, `pg_total_relation_size(relid)`, `n_live_tup`, `n_dead_tup`, `last_autovacuum`, `last_autoanalyze`; top 15 by size |
| Since statistics reset | stat row | `pg_stat_database` of the current database: cache hit ratio, `xact_commit`, `xact_rollback`, `deadlocks`, `temp_files`, `temp_bytes`, `stats_reset` |

## 4. Files

| File | Change |
|------|--------|
| `dev/grafana/role.sql` | New: idempotent role script (decision 4) |
| `dev/grafana/provisioning/datasources/postgres.yaml` | New: data source `radio-db` |
| `dev/grafana/provisioning/dashboards/dashboards.yaml` | New: file provider, `allowUiUpdates: false` |
| `dev/grafana/dashboards/database.json` | New: the dashboard of §3 |
| `dev/grafana/grafana_test.go` | New: integration test (§5) |
| `docker-compose.dev.yml` | Services `grafana-role` and `grafana` (profile `monitoring`), volume `grafana-data`; header comment |
| `.env.example` | `GRAFANA_PORT`, `GRAFANA_ADMIN_PASSWORD`, `GRAFANA_DB_PASSWORD` (dev values) |
| `Makefile` | `monitoring-up`, `monitoring-down`, `monitoring-reset`; `stack-down` and `stack-logs` include the `monitoring` profile |
| `.github/workflows/ci.yml` | `docker compose -f docker-compose.dev.yml --profile app --profile monitoring config -q` |
| `CLAUDE.md` | Commands, layout (`dev/grafana/`), pinned images, dev stack note |
| `internal/architecture` | Only if the architecture test flags the SQL of `dev/grafana`: an `allowList` entry with the reason (test of an infrastructure file) |

## 5. Tasks

1. **Role script.** `dev/grafana/role.sql` per decision 4.
2. **Integration test** (`//go:build integration`, package `grafana_test`):
   - start PostgreSQL with `dbtest.StartPostgres` (superuser DSN); create
     a table that stands for the application tables;
   - run `role.sql` twice with `radio.grafana_password` as a runtime
     parameter (idempotency), the second time with another password; log
     in as `grafana_ro` with the second password;
   - as `grafana_ro`: reading the stand-in table fails with
     `insufficient_privilege` (`42501`); a write fails; `SHOW
     statement_timeout` is `5s`; another session's query text in
     `pg_stat_activity` is visible (not `<insufficient privilege>`);
   - parse `dashboards/database.json` and run every `rawSql` of every
     panel target as `grafana_ro`: each one succeeds; every target uses
     the data source uid of `postgres.yaml`;
   - table-driven where several cases, `testify/require`, `t.Context()`.
3. **Provisioning and dashboard.** The files of §4. Build the dashboard
   in the Grafana UI against the dev stack, export it, remove instance
   fields (`id`, `version`), set the data source uid to `radio-db`.
4. **Compose.** `grafana-role`: `postgres:17.11`, `PGHOST=db`,
   `PGUSER`/`PGPASSWORD`/`PGDATABASE` from the `POSTGRES_*` values,
   `PGOPTIONS=-c radio.grafana_password=${GRAFANA_DB_PASSWORD:-dev-grafana-db-pass}`,
   `./dev/grafana/role.sql:/role.sql:ro`, `restart: "no"`, depends on
   `db` healthy. `grafana`: the pinned image, `127.0.0.1:${GRAFANA_PORT:-3000}:3000`,
   the `GF_*` variables of the deploy plan D2 item 8, `GRAFANA_DB_PASSWORD`
   and `POSTGRES_DB` for the data source, volume `grafana-data`, the two
   provisioning folders read-only, health check, depends on `grafana-role`
   completed. Both in profile `monitoring`.
5. **Makefile.** `monitoring-up`: `$(COMPOSE) --profile monitoring up -d --wait grafana-role grafana`
   (needs `db`, which `depends_on` starts). `monitoring-down`: stop
   `grafana`. `monitoring-reset`: remove the `grafana-data` volume after
   stopping `grafana`, so the admin password from `.env` applies again.
6. **CI.** Render check of the dev compose file with all profiles. The
   integration test runs in the existing integration job.
7. **CLAUDE.md** as in §4.
8. **Manual check on the dev stack** (§7).

## 6. Deploy repository (after this phase)

Not part of this repository's PR; listed so the hand-off is complete
(deploy plan 8.12): copy `dev/grafana/` to `config/grafana/` (without the
test), add `grafana-role` and `grafana` with the production variables
(`:-` interpolation, required by the preflight), step 10a of the update,
the first-run passwords, the CI checks, and the runbook section.

## 7. Done when

- `make test-integration` passes with the new test; `make lint` is clean.
- On the dev stack: `make stack-up && make monitoring-up` → Grafana at
  `http://127.0.0.1:3000` asks for a login; `admin` with
  `GRAFANA_ADMIN_PASSWORD` opens the "Database" dashboard; every panel
  shows data, and with `make run` the sessions of
  `radio-atlantida-backend` appear.
- A long query started by hand in `psql` (`SELECT pg_sleep(60)`) appears
  under "Running queries"; a lock held in one session and waited for in
  another appears under "Blocked sessions".
- Changing `GRAFANA_DB_PASSWORD` and running `make monitoring-up` again
  keeps the dashboard working.
- `make monitoring-down` stops Grafana and nothing else; `make stack-up`
  alone never starts it.
- Port 3000 is published on `127.0.0.1` only (`docker compose port`).

## 8. Known limits

- Snapshot only: what happened between two refreshes, or last night, is
  not visible. History needs `postgres_exporter` and Prometheus (§10.3).
- Secrets reach the containers as environment variables, visible in
  `docker inspect`, like every other secret of the stack.
- Grafana takes about 100–200 MB of memory in the WSL2 VM of Docker
  Desktop; `MONITORING_ENABLED=false` in production frees it.
- Logs are not part of this phase: log search in the same Grafana is
  Phase 15 (`docs/phase-15-plan.md`, §10.4).
- A dashboard edited in the UI is not saved (`allowUiUpdates: false`);
  this is deliberate, the JSON in git is the source.
