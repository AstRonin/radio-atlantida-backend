# CLAUDE.md — radio-atlantida-backend

## Project context

Radio Atlantida is a radio automation system. This repository is its Go backend.

- **Library**: tracks, artists, and playlists. A playlist is a pool, not a queue: track order is irrelevant, rotation picks the track. A track can be in several playlists. Every playlist has a type (`music`, `jingle`, `news`, `segment` are system types; the manager can add more). A type is only a label.
- **Clocks**: 60-minute templates of elements. An element draws from one or more playlists (union, any types) and has a target duration and tolerance. Placement: `flow`, `start`, `end`, or `fixed` minute (±1 min). Fixed elements split the hour into segments that are fitted separately. Audio is never stretched.
- **Grid**: which clock plays in which hour, up to 30 days ahead, in station local time (`Asia/Yerevan` by default).
- **Rotation**: a rolling 24–48 h track plan that honours separation rules (e.g. a track at most once per 72 h) and play rules (time windows, minimum interval, max per hour — these replace dayparting and are never relaxed). Short clocks get filler (blocking dialog on save, pools, reminders); overflow keeps the crossing track and fades it out at the segment end. Hour boundaries are hard.
- **Playout**: the backend pushes the next 2–3 items to Liquidsoap (`request.push`), which streams to Icecast and reports track start/end by HTTP callback and in a local playout journal.
- **Live voice**: WebRTC from the browser through a Go bridge into a Liquidsoap harbor input, mixed over music.
- **Outages**: detected automatically (Icecast mount, callbacks heartbeat, control link); items that did not air are marked *missed*, kept until the manager reschedules or dismisses them; after backend downtime the journal is reconciled.
- **Users**: one manager in an Angular panel. **No authentication** by design — the API listens on localhost or a private network only.
- **Runs on**: Docker Desktop on a Windows 10/11 PC (Linux containers); development on macOS. Media files live on the host under `MEDIA_ROOT`, never in images.

## Required reading at the start of every session

1. `docs/base.md` — the product requirements (the goal; keep later features in mind so early code does not block them).
2. `docs/tech-design.md` — the technical design. **It is the authority** on how things are built.
3. The current phase plan — `docs/backend-init-plan.md` for Phases 1–5.

**If the code, the design, and the requirements disagree, stop and ask.**

## Commands

| Command | What it does |
|---------|--------------|
| `make stack-up` | Start `db`, `icecast`, `liquidsoap` and wait until healthy (creates `.env` from `.env.example` if missing) |
| `make stack-down` / `make stack-logs` | Stop / follow the dev stack |
| `make media` | Generate ~10 test MP3s into `.dev/media` (ffmpeg in a container) |
| `make run` | Run the backend on the host against the dev stack |
| `make build` | Build `bin/backend` |
| `make test` | Unit tests with `-race` |
| `make test-integration` | Unit + integration tests (`-tags integration`, testcontainers-go, needs Docker) |
| `make lint` | `go vet` + `golangci-lint` |
| `make fmt` | gofumpt + goimports |
| `make vuln` | govulncheck |
| `make migrate` | `backend migrate up` (Phase 2) |
| `make openapi` | Write `api/openapi.json` (Phase 3) |
| `make db-up` / `make db-down` | Only PostgreSQL |
| `docker compose -f docker-compose.dev.yml --profile app up --build` | Stack plus the backend container |

Binary: `backend serve` | `backend migrate up` (no `down`, ever) | `backend openapi`.

## Layout

```
cmd/backend/          subcommand dispatch (main.go) and serve wiring
internal/
  config/             env config (caarlos0/env + godotenv); the only place that reads env
  logging/            slog JSON, components, secret masking, async writer, daily UTC rotator
  db/                 pgx pool (UTC session, timeouts); GORM + tx helper in Phase 2
  migrations/         embedded *.up.sql + runner (Phase 2)
  httpapi/            chi router, middleware, health; dto/ and handlers/ from Phase 3
  ws/ operations/ settings/ library/ playrules/ separation/ media/ jobs/   (Phases 3–5)
  clocks/ grid/ rotation/ filler/ playout/ liquidsoap/ outage/ live/       (later phases)
api/                  generated openapi.json (not committed, not hand-edited)
docker/Dockerfile     multi-stage; runtime Debian slim + ffmpeg, non-root
docker-compose.dev.yml  dev/test stack only (production compose is in radio-atlantida-deploy)
dev/liquidsoap/radio.liq  minimal dev playout script (keep in step with deploy's production script)
dev/scripts/gen-media.sh  test audio generator
.dev/                 generated media, playout journal, logs (git-ignored)
docs/                 requirements, design, phase plans
```

## Ground rules

- Module `github.com/AstRonin/radio-atlantida-backend`; Go version pinned in `go.mod` (and `GO_VERSION` in the Dockerfile, CI reads `go.mod`).
- All instants are `time.Time` in UTC; DB columns `timestamptz`; PostgreSQL session timezone UTC. API timestamps ISO 8601 UTC ending in `Z`.
- Database structs are never API payloads. Every endpoint has its own request/response DTOs. Validate DTOs before any write; return field errors per field.
- Multi-table writes use explicit transactions.
- GORM for admin CRUD; pgx for hot paths (play log, rotation, separation, reconciliation). Never GORM auto-migrate.
- Logging never blocks. Secrets are never logged (the handler masks keys matching `password|secret|token|credential`).
- No authentication anywhere. Listen on localhost or a private network.
- Tests: unit tests next to the code; integration tests (`//go:build integration`) against real PostgreSQL via testcontainers-go. No DB mocks for repository tests.

### Go

- Packages by domain (`library`, `playrules`, `rotation`), not by layer. No `util`/`common`.
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
- Domain code never calls `time.Now()`: inject a `Clock`. Randomness: `math/rand/v2` with an injected seeded source (rotation must be deterministic per seed).
- HTTP: explicit server timeouts (streaming routes clear their write deadline via `http.ResponseController`); request bodies size-limited; close response bodies; thin handlers (decode, validate, call service, encode).
- Format with gofumpt + goimports; `golangci-lint` and `go vet` clean — any `//nolint` needs a specific linter and an explanation. Exported identifiers have doc comments; comments explain why.
- `slog` with structured attributes, always with a `component` (`logging.For(log, logging.ComponentX)`; one component per logger — derive from the root logger, never re-tag). Error key: `err`.
- Tests: table-driven where there are several cases, `t.Parallel()` where safe, `testify/require`.

### PostgreSQL and SQL

- Parameterized queries only. Dynamic identifiers (sort column) from a fixed whitelist. No `SELECT *`.
- Avoid N+1 (join or `WHERE id = ANY($1)`; deliberate `Preload`/`Joins` in GORM). Upserts with `INSERT ... ON CONFLICT`.
- Keyset pagination for long lists (play history, logs); `OFFSET` only for short lists.
- `EXPLAIN ANALYZE` every hot-path query; keep the result in a test or comment.
- The database enforces invariants: `NOT NULL` by default, FKs, `UNIQUE`, `CHECK`.
- Constraint/index names: `pk_<table>`, `fk_<table>_<column>`, `uq_<table>_<columns>`, `ck_<table>_<rule>`, `ix_<table>_<columns>`. Map violations to domain errors by SQLSTATE (`23505`, `23503`, `23514`) and constraint name.
- `snake_case`, plural tables. `bigint GENERATED ALWAYS AS IDENTITY` keys, `timestamptz`, `text` (not `varchar(n)` unless a real rule), `jsonb` only for optional settings, `boolean NOT NULL DEFAULT ...`. Enum-like columns are `text` + `CHECK`, not PG enums.
- Index every FK column used in joins or in-use checks. No speculative indexes.
- Short transactions; no network calls inside. `READ COMMITTED`; `SELECT ... FOR UPDATE` for read-then-change; `FOR UPDATE SKIP LOCKED` for job claims. Singleton jobs take an advisory lock. Large deletes in batches. Every query has a context deadline (connections also set `statement_timeout`, `idle_in_transaction_session_timeout`).
- Migrations: forward-only `.up.sql`, one logical change per file, never edit an applied one. Expand and contract. `CREATE INDEX CONCURRENTLY` in its own migration. Migrations seed only system rows; test data lives in fixtures.
- GORM: `SkipDefaultTransaction: true`, `TranslateError: true`, no auto-migrate, `Omit(clause.Associations)` unless intended. pgx: `pgx.CollectRows`/`RowToStructByName`; `pgx.Batch`/`CopyFrom` for bulk.

## Dev stack notes

- Pinned images: `postgres:17.11`, `libretime/icecast:2.4.4-alpine`, `savonet/liquidsoap:v2.4.6`, `linuxserver/ffmpeg:9.0-cli-ls84` (media script), `golang:1.27.2-trixie` / `debian:trixie-20261005-slim` (Dockerfile).
- All ports are published on `127.0.0.1` only. Stream: `http://127.0.0.1:8000/radio`. Liquidsoap telnet `127.0.0.1:1234` (`request.push /media/<file>`, `var.set music_gain = 0.2`), harbor `127.0.0.1:8005` (mount `live`).
- Liquidsoap writes the journal to `.dev/journal/journal-YYYY-MM-DD.jsonl` (one JSON line per track `start`/`end`, UTC) and POSTs the same line to `BACKEND_CALLBACK_URL` (endpoint arrives in the Liquidsoap phase; 404 until then, harmless).
- Connection settings (Icecast/Liquidsoap hosts and ports) become admin settings in Phase 4 with code defaults equal to the compose service names and ports. For `make run`, add `127.0.0.1 icecast liquidsoap` to `/etc/hosts`.
