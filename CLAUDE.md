# CLAUDE.md — radio-atlantida-backend

## Project context

Radio Atlantida is a radio automation system. This repository is its Go backend.

- **Library**: tracks, artists, and playlists. A playlist is a pool, not a queue: track order is irrelevant, rotation picks the track. A track can be in several playlists. Every playlist has a type (`music`, `jingle`, `news`, `segment` are system types; the manager can add more). A type is only a label.
- **Clocks**: 60-minute templates made of playlists and timed inserts. Every playlist has a placement rule (`fill`, `spread` N, `start`, `end`, `start_end`, `middle`), stored on the playlist and overridable per clock; a clock needs at least one fill playlist, and fill playlists may have shares (`fill_weight`, all or none). A timed insert plays a track, or a track of a playlist, at `mm:ss` with a start tolerance (±1 min by default); a one-time insert does the same in one planned hour. Tracks of any length are used; audio is never stretched.
- **Grid**: which clock plays in which hour, up to 30 days ahead, in station local time (`Asia/Yerevan` by default).
- **Rotation**: a rolling 24–48 h track plan that honours play rules (time windows, track and artist intervals, max per hour — these replace dayparting and separation, are never relaxed; "Music rotation" is the 72 h rule). The inserts are anchors, rule tracks take their slots, and the fill playlists fill the rest; the last track before a hard point is the best fit, else it is cut at the target and faded out. There is no filler. Hour boundaries are hard.
- **Playout**: the backend pushes the next 2–3 items to Liquidsoap (`request.push`), which streams to Icecast and reports track start/end by HTTP callback and in a local playout journal.
- **Live voice**: WebRTC from the browser through a Go bridge into a Liquidsoap harbor input, mixed over music.
- **Outages**: detected automatically (Icecast mount, callbacks heartbeat, control link while items are due); items that did not air are marked *missed*, kept until the manager reschedules or dismisses them; after backend downtime the journal is reconciled.
- **Users**: one manager in an Angular panel. **No authentication** by design — the API listens on localhost or a private network only.
- **Runs on**: Docker Desktop on a Windows 10/11 PC (Linux containers); development on macOS. Media files live on the host under `MEDIA_ROOT`, never in images.

## Required reading at the start of every session

1. `docs/base.md` — the product requirements (the goal; keep later features in mind so early code does not block them).
2. `docs/tech-design.md` — the technical design. **It is the authority** on how things are built.
3. The current phase plan — `docs/backend-init-plan.md` for Phases 1–6, `docs/phase-7-plan.md` for Phase 7 (clocks and grid), `docs/phase-8-plan.md` for Phase 8 (rotation engine), `docs/phase-9-plan.md` for Phase 9 (Liquidsoap integration), `docs/phase-10-plan.md` for Phase 10 (outages and missed items), `docs/phase-11-plan.md` for Phase 11 (import, quality analysis, transcoding), `docs/phase-12-plan.md` for Phase 12 (live voice), `docs/phase-13-plan.md` for Phase 13 (release pipeline), `docs/phase-14-plan.md` for Phase 14 (database monitoring with Grafana), `docs/phase-15-plan.md` for Phase 15 (log search with Loki), `docs/phase-16-plan.md` for Phase 16 (broadcast status), `docs/phase-17-plan.md` for Phase 17 (air window), `docs/phase-18-plan.md` for Phase 18 (import in place), `docs/phase-19-plan.md` for Phase 19 (bulk track delete), `docs/phase-20-plan.md` for Phase 20 (track list pages and sort), `docs/phase-21-plan.md` for Phase 21 (on-air titles, reviewed mark, bulk rename), `docs/phase-22-plan.md` for Phase 22 (track cue points, waveform, usage marks), `docs/phase-23-plan.md` for Phase 23 (playlist placement rules, clock inserts, no filler, separation in play rules).

**If the code, the design, and the requirements disagree, stop and ask.**

## Commands

| Command | What it does |
|---------|--------------|
| `make stack-up` | Start `db`, `icecast`, `liquidsoap` and wait until healthy (creates `.env` from `.env.example` if missing) |
| `make stack-down` / `make stack-logs` | Stop / follow the dev stack (monitoring included) |
| `make monitoring-up` | Start Grafana (profile `monitoring`: live database view, log search) at `http://127.0.0.1:3000` with Loki and Alloy; starts `db` if needed |
| `make monitoring-down` / `make monitoring-reset` | Stop Grafana, Loki, and Alloy only / remove their volumes so `GRAFANA_ADMIN_PASSWORD` applies again and the log files are loaded again |
| `make media` | Generate ~10 test MP3s into `.dev/media` (ffmpeg in a container) |
| `make import-samples` | Generate files with known quality issues into the folder `.dev/media/import-samples` (ffmpeg in a container); import that folder |
| `make live-check` | Go live through a running backend with a test tone over WebRTC, no browser (`dev/livecheck`) |
| `make run` | Run the backend on the host against the dev stack |
| `make build` | Build `bin/backend` (version from `git describe`) |
| `make image` | Build the runtime image `radio-atlantida-backend:dev` locally with that version |
| `make test` | Unit tests with `-race` |
| `make test-integration` | Unit + integration tests (`-tags integration`, testcontainers-go, needs Docker) |
| `make lint` | `go vet` + `golangci-lint` |
| `make fmt` | gofumpt + goimports |
| `make vuln` | govulncheck |
| `make migrate` | `backend migrate up` against `DATABASE_URL` |
| `make openapi` | Write `api/openapi.json` |
| `make openapi-lint` | `make openapi` + Redocly lint (`redocly.yaml`; needs Node.js, also runs in CI) |
| `make db-up` / `make db-down` | Only PostgreSQL |
| `docker compose -f docker-compose.dev.yml --profile app up --build` | Stack plus the backend container |

Binary: `backend serve` | `backend migrate up` (no `down`, ever) | `backend openapi` | `backend version`.

Release: push a tag `vX.Y.Z` from `main`. `.github/workflows/release.yml` runs the whole CI, pushes `ghcr.io/astronin/radio-atlantida-backend:X.Y.Z` (never overwrites, no `latest`) and creates the GitHub release with that image's `openapi.json`.

## Layout

```
cmd/backend/          subcommand dispatch (main.go) and serve wiring
internal/
  config/             env config (caarlos0/env + godotenv); the only place that reads env
  settings/           admin settings: typed registry (defaults, limits, apply mode, secrets), Service, Value/OnChange, Guard (cross-checks before a save)
  logging/            slog JSON, components, secret masking, async writer, daily UTC rotator
  db/                 pgx pool (UTC session, timeouts), GORM on the same pool, DB.WithTx (one Tx for pgx and GORM), ConstraintError, CollectOne/CollectAll
  dbtest/             integration-test helpers: PostgreSQL container, migrated DB (build tag integration)
  architecture/       architecture test: SQL and driver imports only in the allowed packages; handlers and jobs never import repositories
  migrations/         embedded NNNN_name.up.sql + runner (golang-migrate)
  httpapi/            chi router, middleware, health, huma API (api.go: OpenAPI, event schemas)
    errcodes/         stable problem and field error codes (never renamed; published as enums)
    dto/              request/response bodies, Problem (RFC 9457)
    handlers/         huma operations; problemFor maps domain errors to HTTP in one place
  ws/                 WebSocket hub at /api/ws and the event types
  operations/         async operations: operations row + one terminal <kind>.completed/.failed event
  jobs/               scheduler (advisory-locked singleton runs, each an operation) and job schedules; no business rules
  retention/          retention rules (play log, operations); jobs schedules it
  workerpool/         bounded worker pool (Pool: fixed workers + bounded queue) and ForEach for batches
  validation/         validation.Error: per-field refusals of domain services (rule → field code in problemFor)
  library/            playlist types, playlists (placement rule in clocks), artists, tracks; track preview; *InUseError names what blocks a delete; title rule, reviewed mark, bulk rename; cue points and fades (SetCue, Cue.Check), usage marks (next planned, on air)
  playrules/          play rules (window, weekdays, track and artist intervals capped at the play log retention, max per hour) and their track/playlist targets
  clocks/             clock templates: playlists with placement rules (EffectiveRule), timed inserts, Check (field validation), one-step save, dry check warnings
  grid/               grid slots (station local weekday/hour/dates), assign/unassign with trim/split/delete, Schedule, Hours, ClockHours
  rotation/           track plan: Build (pure, seeded: insert anchors and chains, rule slots, weighted fill, best fit and crossing, play rules, emergency), Service.Sync (missing/stale hours, plan_hours), one-time inserts (AddInsert, RemoveInsert, Reschedule; hour_inserts survive rebuilds), Sample (the hour of a clock save), planner queue for clock_build, hour_insert, and reschedule
  liquidsoap/         telnet control client (push, queue, var.set, ping), link state and connection generation; Annotate (quoted request annotations)
  playout/            pusher (hour queues by parity, lookahead, pre-push, resync), callbacks → play log (Replay for journal lines), now playing, music gain, emergency picks
  broadcast/          Tracker: derived broadcast state (connected, reconnecting, unreachable, unknown) from output reports, outages, mount checks, heartbeat; since stored on change (broadcast_state), broadcast.state events
  airwindow/          air window: last due items (aired, truncated, missed), the item on air, next plan items; pure Assemble, counts from settings (group air); read only
  outage/             Detector (Icecast mount, callback heartbeat, control link; open/close, marking missed, sweep of unaired items, startup reconcile), Journal replay, played-tracks list, missed items, outages
  media/              MEDIA_ROOT: path rules, os.OpenInRoot (no escape, symlinks included), walk, folder tree, checksum, commit; ffprobe; ffmpeg (loudnorm measure, encode, waveform peaks)
  ingest/             import of a folder in place (folders and files with counts and statuses, title and artist from tags or the file name, originals of .optimal copies skipped), analysis against the broadcast standard (quality issues), transcodes (Fix loudness, Optimize; replace or .optimal copy); one media operation at a time on its worker; cached track waveforms (beside the operations, two decodes at a time)
  repository/         the only place with SQL (§5.1): one package per domain, same name as the domain
    operations/ retention/ jobs/ settings/         (jobs: the advisory locks; settings: GORM)
    library/                                       (GORM; one store per aggregate)
    playrules/                                     (pgx: time and smallint[] columns, array-parameter link sets)
    clocks/ grid/                                  (GORM CRUD; clocks.CheckStore is pgx for the dry check reads)
    rotation/                                      (pgx: library snapshot, history window, plan hours, CopyFrom of items, hour inserts)
    playout/                                       (pgx: items to push, queued marks, move to play_log, ends, emergency tracks)
    broadcast/                                     (pgx: the one broadcast_state row)
    airwindow/                                     (pgx: last play log rows by due time, next plan items)
    outage/                                        (pgx: outages, heartbeat, mark missed, drop stale, play history keyset, dismiss)
    ingest/                                        (pgx: imported tracks, track paths under a folder, analysis columns with the cue reset, artist find-or-create, busy check, waveforms)
  live/               live voice bridge: WebRTC (pion) on one UDP port, one session at a time (takeover), Opus relayed unchanged as Ogg/Opus to the Liquidsoap harbor (Icecast SOURCE); states and live.state events; no SQL
api/                  generated openapi.json (not committed, not hand-edited)
docker/Dockerfile     multi-stage; runtime Debian slim + ffmpeg, non-root
docker-compose.dev.yml  dev/test stack only (production compose is in radio-atlantida-deploy)
dev/liquidsoap/radio.liq  minimal dev playout script (keep in step with deploy's production script)
dev/scripts/gen-media.sh  test audio generator
dev/scripts/gen-import-samples.sh  files with known quality issues in import-samples (make import-samples)
dev/scripts/gen-live.sh   Ogg/Opus test tone for make live-check
dev/grafana/          monitoring (copied to deploy's config/grafana/): role.sql (role grafana_ro), provisioning (data sources radio-db and radio-logs, dashboard provider), dashboards/*.json (Database, Logs); grafana_test.go runs role.sql and every SQL dashboard query against PostgreSQL, and checks every panel's data source
dev/loki/loki.yaml    Loki for log search (copied to deploy's config/loki/): one process, filesystem storage, 10 days
dev/alloy/            Alloy (copied to deploy's config/alloy/, without the test): config.alloy reads LOG_ROOT and PLAYOUT_JOURNAL_ROOT into Loki; pipeline_test.go runs both on the pinned images
dev/livecheck/        dev tool: a WebRTC client that goes live through the backend (make live-check)
.dev/                 generated media, playout journal, logs (git-ignored)
docs/                 requirements, design, phase plans
```

## Ground rules

- Module `github.com/AstRonin/radio-atlantida-backend`; Go version pinned in `go.mod` (and `GO_VERSION` in the Dockerfile, CI reads `go.mod`).
- All instants are `time.Time` in UTC; DB columns `timestamptz`; PostgreSQL session timezone UTC. API timestamps ISO 8601 UTC ending in `Z`.
- Database structs are never API payloads. Every endpoint has its own request/response DTOs. Validate DTOs before any write; return field errors per field.
- Multi-table writes use explicit transactions.
- GORM for admin CRUD; pgx for hot paths (play log, rotation, play rule checks, reconciliation). Never GORM auto-migrate.
- **Repository layer (§5.1).** All SQL lives in `internal/repository/<domain>`; domain code, handlers, jobs, and `cmd` contain none and import neither `gorm.io/...` nor `github.com/jackc/pgx...`. `internal/architecture` enforces it (only `internal/repository`, `internal/db`, `internal/dbtest`, `internal/migrations` are exempt; an exception needs an `allowList` entry with a reason and a comment at the code).
- Logging never blocks. Secrets are never logged (the handler masks keys matching `password|secret|token|credential`).
- No authentication anywhere. Listen on localhost or a private network.
- Tests: unit tests next to the code; integration tests (`//go:build integration`) against real PostgreSQL via testcontainers-go. No DB mocks for repository tests.
- API: every endpoint is a huma operation with DTO types (the OpenAPI document is generated from them). Errors are `dto.Problem` with a code from `errcodes`; a new refusal adds its code there in the same change. Domain errors map to HTTP only in `handlers.problemFor`. New WebSocket events are named schemas registered in `httpapi/api.go`; a new async operation kind is added to `operations.Kinds()`.

### Go

- Packages by domain (`library`, `playrules`, `rotation`), not by layer. No `util`/`common`/`helpers` packages: a package is named after what it provides, and a helper lives in the package whose job it serves (row collection in `db`, bounded concurrency in `workerpool`). A small helper used in two places is copied rather than shared ("a little copying is better than a little dependency"). Prefer the standard library and built-ins (`new(expr)` instead of a `ptr` helper, `slices`, `maps`).
- Small interfaces defined by the consumer. Accept interfaces, return concrete types. No interface with one implementation unless a test needs a fake. No generic repositories.
- No global mutable state, no `init()` with side effects. Dependencies via constructors; `main` wires everything. `os.Getenv` only in `internal/config`.
- Every new third-party dependency needs a reason in the PR description. Prefer the standard library.
- Errors: never ignored; wrap with context (`fmt.Errorf("load playlist %d: %w", id, err)`). Domain errors are sentinels/types (`ErrNotFound`, `ErrInUse`, `ErrValidation`) checked with `errors.Is/As` and mapped to HTTP in one place. Log an error once, where it is handled. `panic` only for programmer errors at startup.
- `context.Context` first in every I/O function; respect cancellation; never store it in a struct. Which context to use:
  - **HTTP handlers**: `r.Context()`, passed to every DB and I/O call. It is cancelled when the client disconnects, by the 30 s route timeout, or when a graceful shutdown runs past its drain deadline (`BaseContext` in `httpapi.NewServer`). Add a shorter `context.WithTimeout` for calls that should fail faster (e.g. `/readyz` pings with 2 s).
  - **Database**: every query gets a context with a deadline. Background work wraps each query or transaction in its own `context.WithTimeout`. The server-side `statement_timeout` (30 s) is only a backstop.
  - **Async operations started by a request** (§6.5): never use `r.Context()`, because it is cancelled as soon as the `accepted` response is sent. Run them on the service context (from `serve`, cancelled on shutdown) and copy over request values (request ID) explicitly.
  - **Scheduled jobs**: each run gets a new context derived from the service context, with a per-job timeout (`context.WithTimeout(serviceCtx, job.timeout)`), cancelled when the run ends. Never reuse one context across runs. Never use `context.Background()`, because shutdown must be able to stop a run.
  - **Outgoing calls** (Icecast, Liquidsoap telnet/HTTP): `http.NewRequestWithContext`, `net.Dialer.DialContext`, and a per-call deadline.
  - `context.Background()` only in `main` and in tests (use `t.Context()`). `context.WithoutCancel` only for cleanup that must outlive a cancelled parent (shutdown drain, final writes), always with a timeout.
- Every goroutine has an owner and a stop path; use `errgroup`. Shared state: mutex or single owner, documented. Tests run with `-race`.
- **Bounded concurrency (`internal/workerpool`).** Never start one goroutine per request, item, file, or event: work whose count depends on input goes through a worker pool.
  - Tasks that arrive over time (async operations, per-request background work): a `workerpool.Pool` created in `main`, passed through the constructor, closed on shutdown after the producers stop (`Close` runs the queued tasks and waits). Size workers and queue with a named constant and a comment why.
  - A batch known up front (scan a directory, probe files, import tracks): `workerpool.ForEach(ctx, n, items, fn)`; the first error cancels the rest.
  - `Submit` waits for a queue slot until its context ends; on an error the producer reports the task as failed (e.g. `operations.Tracker.Run` fails the operation), it never drops it silently. A task recovers its own panics when it runs foreign code.
  - Not for fixed, long-lived goroutines (server, scheduler loops, a single drain/writer goroutine such as `logging.AsyncWriter`, a WebSocket client's write loop): those stay in `errgroup` or have one documented owner. A goroutine that only waits for cancellation is `context.AfterFunc`, not a hand-written `select` on a local `done` channel.
- Domain code never calls `time.Now()`: inject a `Clock`. Randomness: `math/rand/v2` with an injected seeded source (rotation must be deterministic per seed).
- HTTP: explicit server timeouts (streaming routes clear their write deadline via `http.ResponseController`); request bodies size-limited; close response bodies; thin handlers (decode, validate, call service, encode).
- Format with gofumpt + goimports; `golangci-lint` and `go vet` clean — any `//nolint` needs a specific linter and an explanation. Exported identifiers have doc comments; comments explain why.
- `slog` with structured attributes, always with a `component` (`logging.For(log, logging.ComponentX)`; one component per logger — derive from the root logger, never re-tag). Error key: `err`.
- Tests: table-driven where there are several cases, `t.Parallel()` where safe, `testify/require`.

### Repository layer

- A new domain package gets a repository package with the same name (`internal/library` → `internal/repository/library`), renamed together with it. Import it aliased `<domain>repo` (`operationsrepo`) where both are used.
- One file per aggregate (`playlists.go`, `tracks.go`). It holds the SQL, the row structs for scanning, and the mapping to domain types. No business rules, DTOs, network calls, or transaction boundaries.
- The domain declares the small interface it uses (`operations.Store`, `retention.Store`, `jobs.Locker`); the repository implements it and returns domain types and sentinels. No generic or base repository.
- Handlers (`internal/httpapi`) and jobs (`internal/jobs`) call domain services, never repositories, not even for a plain read: they import no `internal/repository/...` package. Only `cmd` wires repositories into services; tests are exempt. `internal/architecture` enforces it.
- Constructor `New(q db.Querier)` (or `New(d *db.DB)` for GORM), and `WithTx(tx db.Tx)` returning a bound copy. When the domain interface includes `WithTx`, the repository returns the domain interface type (Go has no covariant returns), e.g. `func (s *Tracks) WithTx(tx db.Tx) library.TrackStore`. The domain service opens the transaction with `d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {...})` and passes `tx` to every repository it touches; a repository never begins or commits.
- Errors: `pgx.ErrNoRows` → the domain's `ErrNotFound`; `db.AsConstraintError(err)` gives kind (`23505`/`23503`/`23514`/`23P01`) and constraint name → the domain's sentinel. Anything else is wrapped with context. A domain never sees `pgconn.PgError` or a GORM error.
- SQL keywords are upper case (`SELECT`, `INSERT INTO`, …): the architecture test matches them that way, so English messages are not mistaken for SQL.
- Tests: integration tests per repository (testcontainers via `internal/dbtest`); SQL is fine in tests for fixtures and assertions. Domain services get unit tests with fake stores.

### PostgreSQL and SQL

- Parameterized queries only. Dynamic identifiers (sort column) from a fixed whitelist. No `SELECT *`.
- Avoid N+1 (join or `WHERE id = ANY($1)`; deliberate `Preload`/`Joins` in GORM). Upserts with `INSERT ... ON CONFLICT`.
- Keyset pagination for long lists (play history, logs); `OFFSET` only for short lists.
- `EXPLAIN ANALYZE` every hot-path query; keep the result in a test or comment.
- The database enforces invariants: `NOT NULL` by default, FKs, `UNIQUE`, `CHECK`.
- Constraint/index names: `pk_<table>`, `fk_<table>_<column>`, `uq_<table>_<columns>`, `ck_<table>_<rule>`, `ix_<table>_<columns>`, `ex_<table>_<columns>` (exclusion). Map violations to domain errors by SQLSTATE (`23505`, `23503`, `23514`, `23P01` exclusion) and constraint name.
- `snake_case`, plural tables. `bigint GENERATED ALWAYS AS IDENTITY` keys, `timestamptz`, `text` (not `varchar(n)` unless a real rule), `jsonb` only for optional settings, `boolean NOT NULL DEFAULT ...`. Enum-like columns are `text` + `CHECK`, not PG enums.
- Index every FK column used in joins or in-use checks. No speculative indexes.
- Short transactions; no network calls inside. `READ COMMITTED`; `SELECT ... FOR UPDATE` for read-then-change; `FOR UPDATE SKIP LOCKED` for job claims. Singleton jobs take an advisory lock. Large deletes in batches. Every query has a context deadline (connections also set `statement_timeout`, `idle_in_transaction_session_timeout`).
- Migrations: forward-only `.up.sql`, one logical change per file, never edit an applied one. Expand and contract. `CREATE INDEX CONCURRENTLY` in its own migration. Migrations seed only system rows; test data lives in fixtures.
- **A schema change is applied, not only written.** When a change adds a migration (new or changed columns, tables, constraints, indexes), apply it to the dev database before running or checking anything: `make migrate` (or `docker compose -f docker-compose.dev.yml --profile app up --build`, whose `migrate` service runs first), then `make test-integration`. Every phase plan that changes the schema says so in its work, order of work, and "Done when".
- GORM: `SkipDefaultTransaction: true`, `TranslateError: true`, no auto-migrate, `Omit(clause.Associations)` unless intended. pgx: `pgx.CollectRows`/`RowToStructByName`; `pgx.Batch`/`CopyFrom` for bulk.

## Dev stack notes

- Pinned images: `postgres:17.11`, `libretime/icecast:2.4.4-alpine`, `savonet/liquidsoap:v2.4.6`, `linuxserver/ffmpeg:9.0-cli-ls84` (media script), `grafana/grafana:13.2.3` (the OSS edition; `grafana/grafana-oss` is no longer published), `grafana/loki:3.7.8`, `grafana/alloy:v1.20.1`, `golang:1.27.2-trixie` / `debian:trixie-20261005-slim` (Dockerfile).
- All ports are published on `127.0.0.1` only. Stream: `http://127.0.0.1:8000/radio`. Liquidsoap telnet `127.0.0.1:1234` (`hour_0.push /media/<file>`, `var.set music_gain = 0.2`), harbor `127.0.0.1:8005` (mount `live`).
- Liquidsoap has two hour queues, `hour_0` and `hour_1` (`hour_<p>.push <uri>`, `hour_<p>.queue`); a time switch plays the queue of the current station hour, using `hour_shift` set by the backend. Requests carry `liq_cue_in`/`liq_cue_out` and `liq_fade_in`/`liq_fade_out`; in Liquidsoap 2.4 `fade.out` must be `track_sensitive=true` (the default fades once and silences the source for good), and the fade in is the script's own `cue_fade_in` (see `docs/phase-22-plan.md` §10). Behind it, `request.dynamic` asks `BACKEND_EMERGENCY_URL` for emergency tracks, then a safety tone.
- Liquidsoap writes the journal to `$PLAYOUT_JOURNAL_ROOT/journal-YYYY-MM-DD.jsonl` (`.dev/journal` in dev; one JSON line per track `start`/`end` and per Icecast `output` change, UTC; files older than 10 days deleted by the script) and POSTs the same line to `BACKEND_CALLBACK_URL` (`/api/playout/callback`). It also POSTs a `heartbeat` every 5 s (not journaled); without it the outage detector opens a `liquidsoap_callback` outage.
- Live voice: the browser's WebRTC reaches the backend on one UDP port, `LIVE_UDP_ADDR` (default `127.0.0.1:8189`; the backend container listens on `0.0.0.0:8189`, published on `127.0.0.1:8189/udp`), and the answer offers `LIVE_PUBLIC_IPS` (the container sets `127.0.0.1`). The bridge sends to the harbor at `liquidsoap.control_host:liquidsoap.harbor_port`, mount `live`. The script ducks automation to `duck_gain` (setting `liquidsoap.duck_gain`) while the harbor has a source; its buffer is 0.3 s. Browsers allow the microphone only on `localhost` or HTTPS.
- `PLAYOUT_JOURNAL_ROOT` is a required env var (host path, read-only for the backend), like `MEDIA_ROOT` and `LOG_ROOT`.
- The backend mounts `MEDIA_ROOT` read-write (transcodes write files; an import only reads and registers files where they lie); Liquidsoap read-only. Import, analysis, and transcoding need ffmpeg: the backend image has it; on a host without it (`make run` on macOS) they are refused with `media_tools_unavailable`, so use `docker compose -f docker-compose.dev.yml --profile app up --build` to try them. ffmpeg integration tests skip without it (CI installs it).
- Grafana (profile `monitoring`; `make stack-up` never starts it): login `admin` / `GRAFANA_ADMIN_PASSWORD` (applied only when the `grafana-data` volume is created). It reads the database as `grafana_ro` (`pg_monitor`, read-only, `statement_timeout` 5 s, connection limit 3), which the one-shot `grafana-role` service creates or updates from `dev/grafana/role.sql` with `GRAFANA_DB_PASSWORD` on every `make monitoring-up`. Dev passwords have defaults in the compose file. Provisioned dashboards cannot be saved from the UI: export the JSON into `dev/grafana/dashboards/`, keep the queries plain SQL (no Grafana macros, the test runs them), and copy the change to the deploy repository.
- Log search (profile `monitoring`, with Grafana): Alloy reads `LOG_ROOT/app-*.log` and `PLAYOUT_JOURNAL_ROOT/journal-*.jsonl` (read-only, by polling: file notifications do not cross WSL2) into Loki, 10 days like the files; positions in the volume `alloy-data`. Labels: `job`, `file` (the daily file; Loki refuses lines more than 1 h older than their stream's newest), backend `level` and `component`, journal `event` and `source`; every other field stays in the line (`| json`). Loki publishes no port (Grafana data source `radio-logs` at `http://loki:3100`) and has no health check (distroless image, no HTTP client); Alloy's UI is on `127.0.0.1:12345` (dev only). `config.alloy` is formatted by `alloy fmt` (tabs; CI diffs it). Container stdout is not collected.
- Connection settings (Icecast/Liquidsoap hosts and ports, stream format) are admin settings (`internal/settings`) with code defaults equal to the compose service names and ports (`TestDefaultsMatchDevStack`). For `make run`, add `127.0.0.1 icecast liquidsoap` to `/etc/hosts`.
