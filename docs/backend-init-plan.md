# Backend Initialization Plan (radio-atlantida-backend)

Status: Draft for Claude Code
Based on: `base.md` (requirements), `tech-design.md` (technical design)
Last updated: 2026-10-13

## 0. How to use this plan

- **Context documents.** Before any work, Claude Code reads three files, which are copied into the repository under `docs/`:
  - `docs/base.md` — the product requirements. It explains what the station needs and why. Use it to understand the goal of every feature, even when the current phase does not implement it, so that early code (schema, packages, interfaces) does not block later features.
  - `docs/tech-design.md` — the technical design. It is the authority on how things are built.
  - `docs/backend-init-plan.md` — this plan. It says what to build now.
- This plan is the task list for Claude Code. Work one phase at a time. Each phase ends with a green build, passing tests, and a commit (one PR per phase).
- The technical design (`tech-design.md`) is the authority. If this plan and the design disagree, stop and ask.
- Section 10 lists decisions and open points. Items marked **ASK** must be confirmed by the developers before the phase that needs them. Items marked **DEFAULT** are applied unless the developers say otherwise.
- Scope of initialization: Phases 1 to 6. They produce a running service with the full schema, the API contract pipeline, the repository layer, settings, and the library CRUD. The rotation engine, Liquidsoap, outages, and live voice are later phases (Section 9), planned in their own documents.

## 1. Ground rules for the code

- Go module: `github.com/AstRonin/radio-atlantida-backend`. Latest stable Go; pin the version in `go.mod` and in CI.
- One binary, `backend`, with subcommands:
  - `backend serve` — HTTP API, WebSocket hub, background jobs.
  - `backend migrate up` — apply migrations and exit. No `down` command exists.
  - `backend openapi` — print the OpenAPI specification (JSON) to stdout, for the frontend build.
- All instants are `time.Time` in UTC. Database columns are `timestamptz`. The PostgreSQL session timezone is UTC (set on every connection).
- API timestamps are ISO 8601 UTC ending with `Z`.
- Database structs are never used as API payloads. Every endpoint has its own request and response DTOs.
- Validation happens on DTOs before any write. Field errors are returned per field.
- Multi-table writes use explicit transactions.
- GORM for admin CRUD, pgx for hot paths (play log, rotation, separation, reconciliation). GORM auto-migrate is never used.
- All SQL lives in the repository layer (`internal/repository/<domain>`, tech-design §5.1). Domain code, handlers, and jobs contain no SQL. Migration files and test fixtures are the only exceptions.
- Logging never blocks. Secrets are never logged.
- No authentication anywhere (by design). The server listens on localhost or a private network only.
- Tests: unit tests next to the code; integration tests against a real PostgreSQL via testcontainers-go. No mocks of the database for repository tests.

### 1.1 Go best practices (rules for the AI)

**Structure and dependencies**

- Packages are organized by domain (`library`, `playrules`, `rotation`), not by layer (`models`, `services`, `utils`). No `util` or `common` packages. The repository layer (`internal/repository/<domain>`) is the one deliberate exception; its packages are still named after the domains.
- Interfaces are small and defined where they are used (by the consumer), not next to the implementation. Accept interfaces, return concrete types.
- No global mutable state and no `init()` with side effects. Dependencies are passed through constructors (`NewService(db, log, clock)`). `main` wires everything.
- Configuration is read once at startup. `os.Getenv` is not called anywhere else.
- Every new third-party dependency needs a reason in the PR description. Prefer the standard library.
- No premature abstraction: no generic repositories, no interfaces with one implementation unless a test needs a fake.

**Errors**

- Errors are never ignored. Wrap with context: `fmt.Errorf("load playlist %d: %w", id, err)`.
- Domain errors are typed or sentinel values (`ErrNotFound`, `ErrInUse`, `ErrValidation`) checked with `errors.Is` / `errors.As`. They are mapped to HTTP responses in one place (the error middleware), not in every handler.
- Log an error once, at the boundary where it is handled. Do not log and return the same error.
- `panic` only for programmer errors at startup. Expected failures return errors.

**Context, concurrency, time**

- `context.Context` is the first parameter of every function that does I/O. Respect cancellation. Never store a context in a struct.
- Every goroutine has an owner and a stop path (context cancellation or a closed channel). Use `errgroup` for groups. No goroutine leaks.
- Shared state is protected by a mutex or owned by one goroutine. Document which.
- Tests run with `-race`.
- Domain code never calls `time.Now()` directly. It uses an injected `Clock` interface, so rotation, retention, and outage logic are testable and deterministic.
- Randomness uses `math/rand/v2` with an injected, seeded source (the rotation engine must be deterministic for a given seed, §7 of the design).

**HTTP**

- `http.Server` has explicit `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout` (except streaming routes), and `IdleTimeout`.
- Request bodies are size-limited. Response bodies of outgoing requests are always closed.
- Handlers are thin: decode DTO, validate, call the domain service, encode DTO.

**Style and tooling**

- Code is formatted with `gofumpt` and `goimports`. `golangci-lint` and `go vet` pass with no exceptions added without a comment.
- `govulncheck` runs in CI.
- Exported identifiers have doc comments. Comments explain why, not what.
- Logging uses `slog` with structured attributes, not formatted strings: `log.Error("push failed", "track_id", id, "err", err)`.
- Tests are table-driven where there are several cases. `t.Parallel()` where safe. Assertions with `testify/require`.

### 1.2 PostgreSQL and SQL best practices (rules for the AI)

**Queries**

- All SQL is written in the repository layer (`internal/repository/<domain>`, tech-design §5.1), and nowhere else.
- Always use parameterized queries (`$1`, `$2`). Never build SQL by concatenating values. Dynamic identifiers (sort column) only from a fixed whitelist.
- No `SELECT *`. List the columns explicitly.
- Avoid N+1 queries: load related data with a join or one `WHERE id = ANY($1)` query. In GORM, use `Preload` or `Joins` deliberately.
- Use `INSERT ... ON CONFLICT` for upserts, not "select then insert".
- Use keyset pagination (`WHERE started_at < $1 ORDER BY started_at DESC LIMIT n`) for long lists such as play history and logs. `OFFSET` only for short lists.
- Check the plan (`EXPLAIN ANALYZE`) of every hot-path query (rotation candidates, separation checks, play log writes) and keep the result in a test or a comment.

**Schema**

- The database enforces invariants: `NOT NULL` by default, foreign keys, `UNIQUE`, `CHECK`. Application validation exists to give a friendly error, not to replace constraints.
- Constraints and indexes have explicit names: `pk_<table>`, `fk_<table>_<column>`, `uq_<table>_<columns>`, `ck_<table>_<rule>`, `ix_<table>_<columns>`. The backend maps constraint violations to domain errors by SQLSTATE (`23505` unique, `23503` foreign key, `23514` check) and constraint name.
- Names are `snake_case`; tables are plural.
- Types: `bigint` identity keys, `timestamptz` for instants, `text` instead of `varchar(n)` unless the limit is a real rule, `jsonb` only for optional settings, `boolean NOT NULL DEFAULT ...` for flags.
- Every foreign key column that is used in joins or in "is it in use" checks has an index.
- No speculative indexes. Each index serves a query named in the design or in the code.

**Transactions and locking**

- Transactions are short. No network calls (Liquidsoap, Icecast, HTTP) inside a transaction.
- Default isolation is `READ COMMITTED`. Use `SELECT ... FOR UPDATE` when a row is read and then changed, and `FOR UPDATE SKIP LOCKED` when workers claim jobs.
- Singleton background jobs (plan extension, retention, reconciliation) take a PostgreSQL advisory lock, so two runs never overlap.
- Large deletes (retention) run in batches (`DELETE ... WHERE id IN (SELECT id ... LIMIT 5000)`) to avoid long locks.
- Connections set `statement_timeout` and `idle_in_transaction_session_timeout`. Every query has a context with a deadline.

**Migrations**

- One logical change per migration file. An applied migration is never edited; fix it with a new migration.
- Expand and contract (§9.2 of the design): add first, switch code, remove in a later release.
- Indexes on large existing tables are created with `CREATE INDEX CONCURRENTLY` in a migration of their own (outside a transaction).
- Migrations seed only system rows (system playlist types, default separation rules). Test data lives in test fixtures.

**GORM and pgx**

- GORM: `SkipDefaultTransaction: true` (transactions are explicit), `TranslateError: true`, no auto-migrate, no implicit associations saving (`Omit(clause.Associations)` unless intended).
- pgx: use `pgx.CollectRows` / `pgx.RowToStructByName` for scanning; batch inserts with `pgx.Batch` or `CopyFrom` for bulk data (track plan, reconciliation).

## 2. Repository layout

```
radio-atlantida-backend/
  cmd/backend/main.go            # subcommand dispatch: serve | migrate | openapi
  internal/
    config/                      # env config (caarlos0/env + godotenv)
    logging/                     # slog JSON, async writer, daily rotator (removed 2026-10-29, see logs-loki-plan.md), secret masking
    db/                          # pgx pool, GORM handle, tx helper, UTC session, SQLSTATE mapping (no SQL statements)
    migrations/                  # embedded *.up.sql + runner (golang-migrate, iofs)
    httpapi/                     # chi router, middleware, error format, OpenAPI wiring
      dto/                       # request/response DTOs per resource
      handlers/                  # one file per resource
    ws/                          # WebSocket hub, event types (part of OpenAPI)
    operations/                  # async operation tracking + terminal events
    settings/                    # typed setting definitions, defaults, validation
    library/                     # playlist types, playlists, artists, tracks
    playrules/                   # play rules + targets
    separation/                  # separation rules
    media/                       # MEDIA_ROOT path resolution, file streaming (preview)
    jobs/                        # in-process scheduler; holds no business rules
    retention/                   # retention rules: play log, operations (jobs calls it)
    repository/                  # the only place with SQL (tech-design §5.1); one package per domain, named as the domain
      operations/
      retention/
      jobs/                      # advisory locks of the scheduler
      settings/                  # Phase 5
      library/                   # Phase 6
      playrules/                 # Phase 6
      separation/                # Phase 6
    # later phases (each gets its repository package when it is built):
    clocks/ grid/ rotation/ filler/ playout/ liquidsoap/ outage/ live/
  api/                           # generated openapi.json (CI artifact, not hand-edited)
  testdata/
  docker/Dockerfile
  docker-compose.dev.yml         # full dev/test stack: db, icecast, liquidsoap, backend (§3, task 9)
  dev/
    liquidsoap/radio.liq         # minimal dev playout script (startup values come from compose env)
    scripts/gen-media.sh         # generates test audio files with ffmpeg
  docs/
    base.md                      # requirements (context for the AI)
    tech-design.md               # technical design (authority)
    backend-init-plan.md         # this plan
  .dev/                          # generated media, playout journal, logs (git-ignored)
  .env.example
  Makefile
  .golangci.yml
  .github/workflows/ci.yml
  .github/workflows/release.yml
  CLAUDE.md
  README.md
```

`docker-compose.dev.yml` is for development and testing only. The production compose file stays in `radio-atlantida-deploy` (§1.1 of the design); see 10.5.

## 3. Phase 1 — Skeleton

**Goal:** a binary that starts, logs, answers a health check, and shuts down cleanly.

Tasks:

1. `go mod init`, `.gitignore`, `.editorconfig` (LF line endings; development is on macOS, runtime on Windows — see §9.1 of the design), `.gitattributes` with `* text=auto eol=lf`.
2. `internal/config`: config struct with `caarlos0/env/v11` tags; load `.env` with `godotenv` without overriding variables that are already set.
   - Required: `DATABASE_URL`, `MEDIA_ROOT`, `LOG_ROOT` (`LOG_ROOT` and the log files were removed on 2026-10-29, see `logs-loki-plan.md`).
   - Secrets (required later, optional in Phase 1): `ICECAST_SOURCE_PASSWORD`, `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD`.
   - `HTTP_ADDR` (default `127.0.0.1:8090`) — see 10.4.
   - Fail fast with a clear message listing every missing variable.
3. `internal/logging`:
   - `log/slog` JSON handler. Fields: `time` (RFC 3339), `level`, `msg`, `component`, error fields; `request_id` for API logs.
   - Allowed `component` values as typed constants: `api`, `db`, `rotation`, `filler`, `liquidsoap`, `icecast`, `webrtc`, `retention`, `outage`, `reconcile`.
   - Masking handler: any attribute whose key matches `password|secret|token|credential` (case-insensitive) is replaced with `***`.
   - Async writer: buffered channel; when full, drop and count; log one WARN with the drop count after the buffer drains. A write failure never panics.
   - Daily rotator `io.Writer`: file `LOG_ROOT/app-YYYY-MM-DD.log` (UTC date), switches at UTC midnight, deletes files older than 10 days on rotation. Also writes to stdout. *Status 2026-10-29: removed; the handler writes stdout and pushes to Loki, see `logs-loki-plan.md` §4.1.*
4. `internal/httpapi`: chi router with middleware: request ID, recoverer (logs panic with `component=api`), request logging, timeout for normal endpoints (not for streaming or WebSocket).
5. Health endpoints: `GET /healthz` (process alive), `GET /readyz` (database reachable). Used by the Docker health check.
6. Graceful shutdown on SIGINT/SIGTERM: stop accepting requests, drain, close the DB pool, flush logs.
7. `docker/Dockerfile`: multi-stage. Build stage `golang:<ver>`; runtime stage Debian slim with `ffmpeg` and `ca-certificates` installed (ffmpeg is needed for import, analysis, transcoding). Non-root user. `ENTRYPOINT ["backend"]`, `CMD ["serve"]`. `HEALTHCHECK` on `/healthz`.
8. `Makefile`: `run`, `build`, `test`, `test-integration`, `lint`, `migrate`, `openapi`, `db-up`, `db-down`.
9. `docker-compose.dev.yml`: every external service the backend works with, so the application can be run and tested locally. All image versions are pinned (no `latest`). All published ports are bound to `127.0.0.1`.

   | Service | Image | Purpose | Ports (host) | Mounts |
   |---------|-------|---------|--------------|--------|
   | `db` | `postgres:17` (pinned minor) | Database | `127.0.0.1:5432` | named volume `pgdata` |
   | `icecast` | `libretime/icecast:2.4.4-alpine` (see 10.5) | Stream server; the backend checks its mount (§7.1 of the design) | `127.0.0.1:8000` | none (configured by environment variables) |
   | `liquidsoap` | `savonet/liquidsoap` (pinned 2.x tag) | Playout: request queue, fades, fallback, harbor input, callbacks, playout journal | `127.0.0.1:1234` (telnet control), `127.0.0.1:8005` (harbor) | `${MEDIA_ROOT}` → `/media` (ro), `./.dev/journal` → `/journal` (rw), `dev/liquidsoap/radio.liq` (ro) |
   | `backend` | built from `docker/Dockerfile` | The application in a container (profile `app`) | `127.0.0.1:8090` | `${MEDIA_ROOT}` (ro), `${LOG_ROOT}` (rw; removed 2026-10-29, see `logs-loki-plan.md` §5.4), `./.dev/journal` (ro) |

   Details:
   - `db`: user, password, and database from `.env` (dev values in `.env.example`). Health check `pg_isready`.
   - **Startup configuration lives in the compose file** (10.5): everything a service needs to start is set in `docker-compose.dev.yml` through `environment:` (values from `.env`). No separate service config files except the Liquidsoap script.
   - `icecast`: configured only through the image's environment variables: `ICECAST_SOURCE_PASSWORD`, `ICECAST_ADMIN_USERNAME`, `ICECAST_ADMIN_PASSWORD`, `ICECAST_RELAY_PASSWORD`, `ICECAST_HOSTNAME`, `ICECAST_MAX_CLIENTS`, `ICECAST_MAX_SOURCES`. Health check on `/status-json.xsl`. The mount `/radio` is created when Liquidsoap connects as a source.
   - `liquidsoap`: the dev script `radio.liq` reads its startup values from environment variables set in compose (`ICECAST_HOST=icecast`, `ICECAST_PORT=8000`, `ICECAST_MOUNT=/radio`, `ICECAST_SOURCE_PASSWORD`, `TELNET_PORT=1234`, `HARBOR_PORT=8005`, `JOURNAL_DIR=/journal`, `BACKEND_CALLBACK_URL`). The script is minimal but uses the same mechanisms as production (§8 of the design):
     - a request queue fed through the telnet control interface (`request.push`);
     - a safety source (a sine tone or silence) through `fallback`, so the stream never stops;
     - a harbor input for live voice and an interactive `music_gain` variable;
     - `output.icecast` to the `icecast` service, mount `/radio`;
     - on track start and end: append a UTC line to the journal in `/journal`, and send an HTTP callback to `BACKEND_CALLBACK_URL` (default `http://host.docker.internal:8090/api/playout/callback`, which reaches a backend running on the host with `make run`; `http://backend:8090/...` when the `app` profile is used). A failed callback is ignored by Liquidsoap and never stops playout. The callback endpoint itself is implemented in the Liquidsoap phase; until then the backend returns 404 and nothing breaks.
     - Health check: the telnet port answers.
   - The control and harbor ports are published to `127.0.0.1` only so that a backend running on the host can reach them in development. In production they stay on the internal network (§8.9 of the design).
   - `backend` runs only with `docker compose --profile app up`. By default developers run the stack without it and start the backend with `make run`. The `migrate` service (`backend migrate up`, `service_completed_successfully` before `backend`) is added in Phase 2.
   - Test media: `dev/scripts/gen-media.sh` (run by `make media`) generates about 10 short MP3 files with ffmpeg (sine tones of different durations: 20 s, 45 s, 3 min, 4 min) into `./.dev/media`, using ffmpeg from a container so no local install is needed. `.env.example` sets `MEDIA_ROOT=./.dev/media`, `LOG_ROOT=./.dev/logs` (`LOG_ROOT` removed 2026-10-29, see `logs-loki-plan.md` §5.4).
   - Make targets: `make stack-up` (db, icecast, liquidsoap), `make stack-down`, `make stack-logs`, `make media`.
   - Integration tests of repositories still use testcontainers-go (isolated database per test package). The compose stack is for manual testing and for the end-to-end tests of later phases (Liquidsoap, outages).
10. `.golangci.yml` with a sensible default set (errcheck, govet, staticcheck, gosec, revive, gofumpt).
11. `CLAUDE.md`, so every later session works with the same context and conventions:
    - **Project context**: a short summary (10 to 15 lines) of `docs/base.md`: what the system is (radio automation: library, playlists, 60-minute clocks, grid, rotation with separation and play rules, filler, Liquidsoap playout to Icecast, live voice, outage detection and missed items), who uses it (one manager, no authentication), and where it runs (Docker Desktop on Windows, development on macOS).
    - **Required reading** at the start of every session: `docs/base.md` (goal), `docs/tech-design.md` (authority), the current phase plan.
    - The ground rules of Section 1, including 1.1 (Go) and 1.2 (PostgreSQL and SQL).
    - The layout of Section 2 and the commands from the Makefile.
    - "If the code, the design, and the requirements disagree, stop and ask."

**Done when:** `make stack-up` starts `db`, `icecast`, and `liquidsoap` and all three are healthy; the Icecast mount `/radio` plays the safety source in a player; `make media` creates the test files; `make run` starts the service against the dev database; `/healthz` and `/readyz` answer 200; a log file appears in `LOG_ROOT` (removed 2026-10-29: the log goes to stdout and to Loki, see `logs-loki-plan.md`); Ctrl+C exits cleanly; `make lint test` passes; `docker compose --profile app up` runs the backend container and its health check passes.

## 4. Phase 2 — Database and migrations

**Goal:** the full schema from §4 of the design, applied by `backend migrate up`.

Tasks:

1. `internal/db`: pgx pool (`pgxpool`), GORM opened on the same `*sql.DB` via `pgx/stdlib`, `timezone=UTC` set for every connection (`AfterConnect` / runtime params). Helper `WithTx(ctx, fn)` for pgx and GORM.
2. `internal/migrations`: golang-migrate with the `iofs` source over `embed.FS`. Only `.up.sql` files. `backend migrate up` applies, logs each version, exits non-zero on failure (including a "dirty" state, with a message pointing to `update.bat restore`).
3. Migration `0001_init.up.sql` — create, in dependency order:
   - `playlist_types` + seed the four system types (`music`, `jingle`, `news`, `segment`, `is_system = true`).
   - `artists`, `playlists`, `tracks`, `track_playlists`.
   - `play_rules`, `track_play_rules`, `playlist_play_rules`, `separation_rules` (+ seed one track rule and one artist rule with `min_interval_hours = 72`; the real values from the reference software are entered later).
   - `clocks`, `clock_elements`, `clock_element_playlists`, `clock_filler_pool`.
   - `grid_slots`.
   - `broadcast_outages`, `scheduled_items`, `filler_requests`, `play_log`.
   - `settings`, `operations`.
   - All indexes from §4 of the design, including the partial unique indexes (`filler_requests` pending, `broadcast_outages` open).
4. Schema conventions (see 10.3):
   - Primary keys: `bigint GENERATED ALWAYS AS IDENTITY`.
   - Enum-like columns: `text` + `CHECK (... IN (...))`, not PostgreSQL enum types (easier expand/contract).
   - Every instant: `timestamptz NOT NULL DEFAULT now()` where applicable.
   - Foreign keys explicit, with `ON DELETE` chosen per relation (`RESTRICT` for anything the API must refuse to delete: playlist types in use, playlists used by clock elements, play rules in use; `CASCADE` for pure link tables).
   - CHECK constraints for invariants that do not need data: `clocks.length_seconds = 3600`, `fixed_minute BETWEEN 0 AND 59`, `fixed_minute IS NOT NULL` when `placement = 'fixed'`, play rule "at least one constraint set", `window_from`/`window_to` both set or both null, weekdays in 1..7.
5. Integration test: start PostgreSQL in testcontainers, run all migrations, assert the system types and separation rules exist, run migrations a second time (no-op).

**Done when:** `make migrate` creates the schema on an empty database; the integration test passes; `\d` of every table matches §4 of the design.

## 5. Phase 3 — API contract, errors, WebSocket, operations

**Goal:** the plumbing every feature endpoint will use.

Tasks:

1. OpenAPI generation (library per 10.1). Every endpoint is registered with typed request/response DTOs, so the spec is produced from code. `backend openapi` prints the spec. CI stores `api/openapi.json` as a release artifact for `radio-atlantida-frontend`.
2. Error format (one shape for all errors), RFC 9457 problem details:
   ```json
   { "type": "...", "title": "Validation failed", "status": 422, "code": "validation_failed",
     "detail": "...", "errors": [ { "field": "elements[2].fixed_minute", "code": "range", "message": "..." } ],
     "request_id": "..." }
   ```
   Business refusals (for example, "type is in use") use the same shape with a list of the blocking entities in an `extensions`-style field (for example, `used_by: [{ "kind": "playlist", "id": 3, "name": "Pop" }]`).
   - Every problem response carries a stable `code` (for example `playlist_type_in_use`, `system_type_not_deletable`, `play_rule_in_use`). Every field error carries a stable `code` too (for example `required`, `range`, `format`). The codes are constants in one Go package (`internal/httpapi/errcodes`), listed in the generated OpenAPI document as an enum, so the frontend can translate messages by code (see the frontend plan, 1.3). Codes are never renamed once released; new codes are added. `title` and `detail` are English and are for logs and developers only.
3. `internal/ws`: hub with `coder/websocket` at `GET /api/ws`. Broadcast to all connected panels. Envelope:
   ```json
   { "type": "track_plan.built", "time": "2026-10-13T06:44:00Z", "operation_id": 12, "data": { } }
   ```
   Each event type is a named schema in the OpenAPI document (added to components explicitly). Slow clients are dropped, never block the hub.
4. `internal/operations`: `Start(ctx, kind) (id)`, `Complete(id, summary)`, `Fail(id, component, err)`. Writes the `operations` row and emits exactly one terminal event (`<kind>.completed` / `<kind>.failed`). Optional `Progress`. Helper to run a function as an operation in a goroutine with panic recovery.
   - `GET /api/operations/{id}` and `GET /api/operations?since=` so a reconnecting panel can catch up.
5. Errors at ERROR level are also pushed to the panel as an `error.alert` event (§10.1 of the design).
6. Logs API: `GET /api/logs/files`, `GET /api/logs/files/{name}?level=&component=&limit=&offset=`. File name must match `^app-\d{4}-\d{2}-\d{2}\.log$`; read only from `LOG_ROOT`. *Status 2026-10-29: replaced by `GET /api/logs` on Loki, see `logs-loki-plan.md` §4.2.*
7. `internal/jobs`: a small in-process scheduler (ticker-based, UTC). First jobs: log file cleanup (already in the rotator; removed 2026-10-29) and `operations` retention (10 days). Play log retention is added here too (`DELETE ... WHERE status IN ('aired','truncated') AND started_at < now() - interval '10 days'`, plus resolved missed rows by `resolved_at`), because the table already exists.
8. Live status endpoint `GET /api/status/live` (deploy plan, 8.5): returns `{ "on_air": bool|null, "open_outage_id": int|null, "current_clock": string|null, "time": "<UTC>" }`. Until the Liquidsoap and outage phases (Section 9, items 9 and 10) exist, `on_air`, `open_outage_id` and `current_clock` are `null`. `null` means unknown, never off-air; callers must treat it as on air. Documented in the OpenAPI spec and covered by one test.

**Done when:** `backend openapi` outputs a valid spec (checked by a validator in CI); a test operation emits its event over WebSocket and is readable via REST; validation errors come back per field; `GET /api/status/live` answers with the unknown-state values.

## 6. Phase 4 — Repository layer

**Goal:** all SQL is in one layer, `internal/repository/<domain>`, with one package per domain (tech-design §5.1). The queries written in Phases 1 to 3 move into it.

Tasks:

1. **Layer skeleton.** Create `internal/repository/operations` and `internal/repository/retention`. Package names equal the domain package names (`operations`, `retention`). Where both are imported in one file, the repository import is aliased `operationsrepo` or `retentionrepo`, so both names stay visible. One file per aggregate.
2. **Domain interfaces.** `internal/operations` and `internal/retention` declare the small interfaces they need (for example `operations.Store`, `retention.Store`), next to where they are used. The repositories implement them and return domain types. No generic or base repository.
3. **Transaction handle.** Extend `internal/db` so a transaction is passed explicitly: `db.WithTx(ctx, func(ctx context.Context, tx db.Tx) error)`, where `db.Tx` serves both pgx and GORM. Repositories get a bound copy through `WithTx(tx)`. A repository never begins or commits a transaction itself.
4. **Error mapping.** `internal/db` translates SQLSTATE `23505`, `23503`, and `23514` together with the constraint name into a `db.ConstraintError`. Each repository maps it to its own domain sentinel error. Any other error is wrapped with context, never mapped.
5. **Move the existing queries.**
   - `operations` (Phase 3): insert of the running row, terminal status update, select by ID, and list since a time → `internal/repository/operations`.
   - Retention (Phase 3, in `internal/jobs`): delete of `operations` older than 10 days, and delete of `play_log` rows with status `aired` or `truncated` older than 10 days → `internal/repository/retention`. The rule lives in `internal/retention`; `internal/jobs` only schedules it.
   - Advisory lock of the scheduler (Phase 3, in `internal/jobs`): `pg_try_advisory_lock` and `pg_advisory_unlock` → `internal/repository/jobs`, behind the `jobs.Locker` interface. The repository package is named after the domain package `internal/jobs`.
   - Not moved, because they are not query statements: the connection probe behind `/readyz` (stays in `internal/db`), the migration runner and its SQL files (`internal/migrations`), and log file cleanup (file I/O in `internal/logging`; removed 2026-10-29, the rotator is gone).
6. **Architecture test.** A test in `internal/architecture` scans all non-test Go files. It fails if SQL statements (`SELECT`, `INSERT INTO`, `UPDATE … SET`, `DELETE FROM`, `ON CONFLICT`) or imports of `gorm.io/gorm` and `github.com/jackc/pgx` appear outside `internal/repository/`, `internal/db/`, `internal/dbtest/`, and `internal/migrations/`. The failure message names the file and line. A deliberate exception needs a comment and an entry in the test's allow list.
7. **Repository tests.** Integration tests for each repository against PostgreSQL (testcontainers, through `internal/dbtest`). The existing operations and retention tests keep their behaviour; only the location of the SQL changes. Tests may use SQL for fixtures and assertions.
8. **CLAUDE.md.** Add the layer rule (tech-design §5.1), the rule "a new domain package gets a repository package with the same name", and the updated layout of Section 2.

**Done when:** the architecture test passes, and it fails when a SQL statement is added temporarily to `internal/operations` (checked once, then reverted); every repository has integration tests; `make lint test` passes.

## 7. Phase 5 — Settings

**Goal:** all runtime settings in the database, edited through the API (§6.3 of the design).

Tasks:

1. `internal/settings`: a registry of typed definitions. Persistence goes through `internal/repository/settings`; the registry itself holds no SQL: key, Go type, default, validator, `apply` mode (`immediate` or `reconnect`), and a `secret` flag (secrets are never stored here; the API only reports whether the env secret is set).
2. Initial keys (grouped as in §6.3): station (timezone `Asia/Yerevan`, name), stream output (Icecast host, port, mount, codec, bitrate, sample rate, channels, loudness target, metadata, HLS flag), Liquidsoap (music gain, `fade_out_ms` 2500 in 2000..3000, harbor port, control host/port — see 10.4), rotation (horizons, lookahead 2..3, tolerance defaults, default filler mode), filler (reminder deadline, fallback `random`), outage detection (heartbeat 10 s, Icecast check 5 s, failures 2), retention (play log 10, logs 10; the logs key was removed 2026-10-29, see `logs-loki-plan.md` §4.4), emergency playlist id.
   - **Code defaults equal the dev compose values** (10.4, item 2). The connection settings to Icecast and Liquidsoap are stored in the database and edited in the admin panel; until the manager saves a value, the code default is used. Defaults: Icecast host `icecast`, port `8000`, mount `/radio`, codec MP3, bitrate 128 kbps, sample rate 44100, channels 2; Liquidsoap control host `liquidsoap`, port `1234`, harbor port `8005`. The host names are the compose service names, so the defaults work for the `app` profile and for the production stack in `radio-atlantida-deploy`, which uses the same service names.
   - For a backend started on the host with `make run`, the README tells the developer to add one line to the hosts file: `127.0.0.1 icecast liquidsoap` (the ports are published to `127.0.0.1`). The alternative is to change the two hosts in the admin panel.
   - The defaults are not seeded by a migration. Migrations seed only system rows (1.2), and `GET /api/settings` must be able to report `is_default` (§6.3 of the design: "Defaults are defined in code and used until a value is saved").
   - The values in `docker-compose.dev.yml` and the code defaults must match. A unit test compares the default keys with a small table of expected dev values, and a comment in the compose file points to `internal/settings`.
3. Timezone validated against the IANA database (`time.LoadLocation`); the runtime image must contain tzdata (or import `time/tzdata`).
4. Endpoints: `GET /api/settings` (all keys with value, default, `is_default`, apply mode), `PUT /api/settings/{key}`, `GET /api/settings/secrets` (only `{ name, is_set }`).
5. A change that affects air is logged at INFO with a UTC timestamp. Subscribers in code can react to a change (later used by Liquidsoap and the outage detector).

**Done when:** reading an unsaved key returns its default; an invalid value returns a field error; secrets never appear in any response or log.

## 8. Phase 6 — Library CRUD and track preview

**Goal:** the manager can build the library: playlist types, playlists, artists, tracks, play rules, separation rules. Endpoints follow §6.4 of the design. Persistence goes through `internal/repository/library`, `playrules`, and `separation` (Phase 4).

Tasks:

1. Playlist types: `GET/POST/PUT/DELETE /api/playlist-types`. List includes a usage count. `code` is a unique slug and immutable after creation. Delete refused for system types and for types used by any playlist or track (error lists them).
2. Playlists: `GET /api/playlists?type_id=` (with type and track count), `POST`, `PUT`, `DELETE` (refused if used by a clock element; error names the clocks), `PUT /api/playlists/{id}/tracks` (set membership; order is not stored).
3. Artists: basic CRUD (needed for tracks and artist separation).
4. Tracks: CRUD for metadata and `file_path` (relative to `MEDIA_ROOT`; reject absolute paths and `..`), `type_id`, playlist membership, play rules (`PUT /api/tracks/{id}/play-rules`). Import and ffmpeg analysis are a later phase; for now, metadata is entered or registered by path, and `duration_ms`/format may be filled by an `ffprobe` call on registration if the file exists.
5. Play rules: `GET /api/play-rules` (with usage counts), `POST`, `PUT`, `DELETE` (refused if in use; lists tracks and playlists), `PUT /api/play-rules/{id}/targets`, `PUT /api/playlists/{id}/play-rules`. Validation: at least one constraint; window both-or-none; window may cross midnight; weekdays 1..7.
6. Separation rules: list and update (`min_interval_hours`, notes).
7. Track preview: `GET /api/tracks/{id}/audio`. Resolve `file_path` under `MEDIA_ROOT` by track ID (client never sends a path), serve with `http.ServeContent` (Range support), content type from the stored format, 404 + log `component=api` if the file is missing. No timeout middleware on this route.
8. Integration tests for every refusal rule (type in use, playlist in use, rule in use, system type) and for the preview Range response.

**Done when:** the full library flow works through the API and the generated OpenAPI spec covers every endpoint.

## 9. Later phases (outline only, separate plans)

7. Clocks and grid: clock CRUD with elements and `playlist_ids`, placement (`flow/start/end/fixed`), segments, replacement of fixed elements, two-step save with dry check and filler choice (§6.0, §7.2), grid slots up to 30 days.
8. Rotation engine: candidate union, separation and play rule checks against `play_log` + `scheduled_items`, tolerance fit, shortfall filler, overflow truncation, emergency steps; deterministic with a seed; rolling 24–48 h plan job; filler requests and reminders.
9. Liquidsoap integration: control client (`request.push`, 2–3 ahead), callbacks endpoint, hour-boundary switching, music gain, emergency mode.
10. Outages and missed items: detector (Icecast mount check, callbacks heartbeat, control link), marking, playout journal reconciliation, played-tracks list, reschedule and dismiss.
11. Import pipeline: ffmpeg/ffprobe analysis, quality thresholds, transcoding to the broadcast standard.
12. Live voice: WebRTC bridge to the Liquidsoap harbor input.
13. CI/CD: `release.yml` builds and pushes `ghcr.io/astronin/radio-atlantida-backend:<tag>` on `v*` tags and publishes `openapi.json`. (A minimal `ci.yml` with lint, unit and integration tests is created in Phase 1.)

## 10. Decisions and open points

### 10.1 OpenAPI library — ASK

The design leaves the choice open. Recommendation: **huma v2** (`github.com/danielgtaylor/huma/v2`) with the chi adapter (`humachi`). It generates OpenAPI 3.1 from Go types, validates requests from struct tags, and returns RFC 9457 errors out of the box, which matches §6, §6.2 and §6.4 of the design. Alternatives: swaggo (comment-based, OpenAPI 2, weaker typing), ogen (spec-first, the opposite of "generated from Go code"). WebSocket event schemas are registered in the huma schema registry manually.

### 10.2 Greenfield schema — DEFAULT

The v7-7 and v7-8 migration notes in §9.2 of the design (copying from `categories` and `daypart_rules`) describe a migration from an earlier version. This project starts from an empty database, so `0001_init` creates the final model directly. No `categories` or `daypart_rules` tables are created.

### 10.3 Schema conventions — DEFAULT

`bigint` identity keys, `text` + `CHECK` instead of PostgreSQL enums, `timestamptz` everywhere, PostgreSQL 17.

### 10.4 Items missing from the design — ASK

1. **HTTP listen address** is not listed among environment variables. Proposal: `HTTP_ADDR` in env, default `127.0.0.1:8090` (in Docker: `0.0.0.0:8090` on the internal network).
2. **Liquidsoap control host/port and callback URL** — **DECIDED.** Connection settings (Icecast host, port, mount, stream format; Liquidsoap control host and port, harbor port) are admin settings in the database, with code defaults equal to the dev compose values (Phase 5, task 2). Credentials stay in env. The callback URL is a startup setting of Liquidsoap and is set in the compose file (`BACKEND_CALLBACK_URL`).
3. **Clock default filler mode.** §7 and §7.2 refer to "the clock's default mode", but `clocks` has no such column. Proposal: add `clocks.default_filler_mode` (nullable; falls back to the global setting).
4. **`play_log.source`** is "auto or live" in §4, but §8.6 logs `source = emergency`. Proposal: `auto`, `live`, `emergency`.
5. **Playout journal access.** Reconciliation (§7.1) reads the Liquidsoap journal, but §9 mounts the journal only into `liquidsoap`. Proposal: mount the same host directory read-only into `backend`; path via env `PLAYOUT_JOURNAL_ROOT` (another host path, like `MEDIA_ROOT`).
6. **Clock save: sync or async** — **DECIDED** (2026-10-14, `docs/phase-7-plan.md` §2): the dry check and the save are synchronous; the async rebuild of planned hours arrives with rotation (Phase 8). Its operation ID (`clock_build.completed/failed`) is then added to the save response.
7. **Missed marking scans `aired` items in `scheduled_items`**, but aired items are moved to `play_log` (§4.1). Proposal: marking scans `planned` and `queued` only; aired items are already confirmed.
8. **§9.2 step 8** says "does not start the new `api`"; the service is named `backend`. Fix in the design.

### 10.5 Dev stack and service configuration — DECIDED

1. **Two kinds of configuration.**
   - *Startup configuration* of a service (passwords, ports, host name, limits) lives in the compose file of each repository (`environment:` with values from `.env`). Icecast needs no config file. Liquidsoap needs its script, which reads the startup values from the environment.
   - *Connection configuration* (how the backend reaches Icecast and Liquidsoap, and the stream format) lives in the database and is edited in the admin panel, so the manager can change it without touching files. Code defaults match the dev compose values (Phase 5).
2. **Liquidsoap script.** The backend repository keeps a minimal dev script in `dev/liquidsoap/radio.liq`. The deploy repository keeps the production script in `config/`. When the interface the backend relies on changes (telnet commands, callback payload, journal format), both scripts are updated in the same release.
3. **Icecast image:** `libretime/icecast:2.4.4-alpine` (small Alpine image, stable Icecast 2.4.4, configured by environment variables, also published as `ghcr.io/libretime/icecast`). The same tag is used in `radio-atlantida-deploy`. The image maintainers rebuild tags when base packages change, so for reproducible builds the tag may be pinned with its sha256 digest.
4. **Liquidsoap version:** pin one 2.x tag of `savonet/liquidsoap` for both repositories; the dev script is written for that version. Claude Code picks the latest stable 2.x tag and reports it.

### 10.6 Stable error codes — DECIDED

1. Problem responses and field errors carry stable machine-readable codes (Phase 3, task 2). The frontend translates messages by code, so the UI can be shown in any language (frontend plan, 1.3). The English `title` and `detail` are a fallback for logs and for developers.
2. Codes live in one package, `internal/httpapi/errcodes`, and are published in the OpenAPI document as enums. A new refusal needs a new code, added in the same change as the rule that raises it.
3. Renaming or removing a released code is a breaking change for the frontend and needs a coordinated release.

### 10.7 Repository layer — DECIDED

1. All SQL lives in `internal/repository/<domain>` (tech-design §5.1). Each repository package is named after its domain package and is renamed together with it.
2. Domain packages declare the interfaces they need; repositories implement them. No generic or base repository.
3. An architecture test enforces the rule (Phase 4, task 6).

## 11. Prompt for the first Claude Code session

Before the session, copy `base.md`, `tech-design.md`, and `backend-init-plan.md` into `docs/` of the empty repository.

> You are starting the backend of a radio automation system.
>
> 1. Read `docs/base.md` first. It describes what the station needs: the overall goal of the product. Keep it in mind for every decision, so that what you build now does not block later features.
> 2. Read `docs/tech-design.md`. It is the authority on how the system is built.
> 3. Read `docs/backend-init-plan.md`. It is the task list.
>
> Implement Phase 1 of `docs/backend-init-plan.md`. Follow Section 1 (ground rules, including 1.1 Go and 1.2 PostgreSQL/SQL best practices) and Section 2 (layout). Build the full dev stack of task 9 (`db`, `icecast`, `liquidsoap`, `backend` profile) with pinned image versions, and the test media script. Write `CLAUDE.md` as described in task 11, including the project context summary of `docs/base.md`.
>
> Apply the DEFAULT and DECIDED items of Section 10. For ASK items, use the proposals unless told otherwise, and list in your final message which ones you applied and which image tags you pinned. If the requirements, the design, and the plan disagree, stop and ask. Do not start Phase 2. Finish with `make stack-up` healthy, `make lint test` passing, and a short summary of what was created.
