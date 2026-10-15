# Radio Automation System: Technical Design

Status: Draft (v7-17)
Last updated: 2026-10-15

## 1. Overview

The system manages a radio station's programming and broadcasts it online. It covers:

- a content library (tracks, jingles, news, segments) organized into playlists, where every playlist has a type that the manager can extend;
- hourly clock templates (60 minutes) and a weekly grid that assigns clocks to time slots, planned up to 30 days ahead;
- a rolling track plan (24 to 48 hours) that respects separation rules and play rules;
- play rules (time windows, minimum intervals, plays per hour) that the manager creates on a separate page and applies to tracks or playlists, mostly for jingles;
- a play button for tracks in the panel, so the manager can listen without leaving the admin;
- clock fitting by track selection within tolerances, never by changing audio;
- filler handling when a clock is short, including a blocking dialog on save;
- clocks that start and end on exact hour boundaries; a track that overruns the boundary fades out over 2 to 3 seconds and is cut;
- live voice from a web browser mixed over the music;
- detection of broadcast outages, marking of items that did not air ("missed"), and rescheduling of missed content into later clocks;
- a play log of what actually aired, kept for 10 days (missed items are kept until resolved).

The system is split into four parts: a web management panel, a Go backend that holds the business logic, a PostgreSQL database, and a Liquidsoap playout engine that produces the audio stream, delivered through Icecast.

The panel is operated by a single manager. There is no authentication, no login, no passwords, and no roles.

### 1.1 Source Repositories

The system is developed in three Git repositories. Each repository has one purpose, and no component is copied between them.

| Repository | Contents | Used for |
|------------|----------|----------|
| `radio-atlantida-backend` | Go backend: REST API, WebSocket hub, clock/grid/rotation engine, filler and reminder service, outage detector, WebRTC live voice bridge, Liquidsoap control client, track preview audio endpoint (§6.6), play log writer, reconciliation job, retention job, versioned migrations (`.up.sql`), OpenAPI specification generation (§6.4) | Builds the `radio-atlantida-backend` image (§9.2) |
| `radio-atlantida-frontend` | Angular management panel, static help texts (§6.1), validation (§6.2), settings pages (§6.3), played-tracks list with missed items, track player (§6.6), TypeScript client generated from the backend OpenAPI specification (§6.4) | Builds the `radio-atlantida-frontend` image (§9.2) |
| `radio-atlantida-deploy` | `docker-compose.yml`, `.env.example`, `update.bat`, `update.ps1`, `config/` (Liquidsoap script and Icecast configuration for the test stack), `README.md` | Runs the stack on the production Windows host. Contains no application code (§9.2) |

Rules for using the code:

- The repositories are hosted on GitHub under the account `AstRonin` (for example, https://github.com/AstRonin/radio-atlantida-backend). Container images are published to GitHub Container Registry under the lowercase owner name, `ghcr.io/astronin/`, which is the form the registry requires.
- Each repository has its own GitHub Actions workflow that builds and pushes its image on tagged releases (§9.2).
- The backend and the frontend are released together under one version tag. The OpenAPI specification produced by the backend at that tag is the input for the frontend client build, so a frontend build always matches the backend it runs against.
- The deploy repository refers to application images by tag only (`IMAGE_TAG`). It never builds application code.
- Database migrations live in `radio-atlantida-backend` and are shipped inside the `radio-atlantida-backend` image. The deploy repository does not contain migration files.
- Liquidsoap scripts and Icecast configuration that are part of the test stack are kept in `radio-atlantida-deploy/config/`. Production stream settings are stored in the database through the admin panel (§6.3, §8.8) and are not kept in any repository.

## 2. Architecture

```
 Browser (Angular panel)              Browser (live microphone)
        |  HTTPS / JSON + WebSocket          |  WebRTC
        v                                    v
 +-----------------------------------------------+
 |              Go backend                       |
 |  - REST API (JSON) + OpenAPI specification    |
 |  - WebSocket hub (UI events)                  |
 |  - Clock/grid/rotation engine                 |
 |  - Filler and reminder service                |
 |  - Outage detector and missed-item service    |
 |  - WebRTC live voice bridge                   |
 |  - Liquidsoap control client                  |
 |  - Play log writer, reconciliation, retention |
 +-----------------------------------------------+
        |                 |                 ^
        | SQL (pgx/GORM)  | control         | callbacks (HTTP)
        v                 v                 |
 +-------------+   +----------------------------+
 | PostgreSQL  |   | Liquidsoap (playout/mix)   |
 +-------------+   |  + local playout journal   |
                   +----------------------------+
                          |
                          v
                 Icecast (stream server)
```

Responsibilities:

| Component   | Responsibility |
|-------------|----------------|
| Angular     | Management UI: library with track preview player, playlists, playlist types, clocks (including each clock's default filler mode), grid, play rules, filler dialogs, played-tracks list (aired, truncated, missed), live control, monitoring. Uses a TypeScript client generated from the backend OpenAPI specification |
| Go          | Decides what plays next, enforces rules, fills short clocks, detects outages, marks missed items, reconciles the play log, relays live voice, publishes the API contract |
| PostgreSQL  | Source of truth for library metadata, rules, grid, track plan, filler pools, play history, missed items, outage periods, settings |
| Repository layer (inside Go) | The only code that contains SQL: one package per domain under `internal/repository` (§5.1). Business logic reaches the database only through it |
| Liquidsoap  | Plays audio in real time, mixes live voice with music, applies fades, handles fallback, keeps a local playout journal |
| Icecast     | Distributes the audio stream to listeners and reports its own connection state |
| Docker      | Packages and runs all services except the media files |

## 3. Technology Stack

| Area              | Choice                                    | Notes |
|-------------------|-------------------------------------------|-------|
| Frontend          | Angular                                   | Consumes a TypeScript client generated from the backend contract (§6.4). Repository: `radio-atlantida-frontend` |
| Backend           | Go                                        | REST API, WebSocket events, scheduling, rotation, outage handling, API contract. Repository: `radio-atlantida-backend` |
| API contract      | OpenAPI specification generated from Go   | Single source of truth for REST and WebSocket payloads (§6.4) |
| UI events         | WebSocket                                 | Now playing, progress, live state, alerts, filler requests, outage and missed-item events |
| Live voice        | WebRTC into a Go bridge                   | Feeds a Liquidsoap input |
| Database          | PostgreSQL                                | Relational core, JSONB for optional element settings |
| DB access         | GORM for admin CRUD; pgx for hot paths    | See §5 and §5.1 |
| Playout           | Liquidsoap                                | Linux container |
| Stream server     | Icecast                                   | Test instance in Docker; production server configured in the admin panel |
| Audio tooling     | ffmpeg                                    | Transcoding and quality analysis on import |
| Containers        | Docker and Docker Compose                 | Linux containers, one compose file. Deployment repository: `radio-atlantida-deploy` |

### 3.1 Backend Libraries

| Purpose                     | Library                        | Notes |
|-----------------------------|--------------------------------|-------|
| HTTP router and middleware  | `github.com/go-chi/chi/v5`     | Built on `net/http`. Middleware for panic recovery, request ID, and request logging (§10.1) |
| WebSocket                   | `github.com/coder/websocket`   | UI events, error alerts, filler requests, missed-item events (§6) |
| Environment variables       | `github.com/caarlos0/env/v11`  | Config structs with `env` tags; supports required fields and defaults |
| `.env` file loading         | `github.com/joho/godotenv`     | Loads `.env` at startup if present. Variables already set in the environment are not overridden |
| PostgreSQL driver           | `github.com/jackc/pgx/v5`      | Hot paths: play log, rotation queries, separation checks, reconciliation (§5) |
| ORM                         | `gorm.io/gorm`                 | Admin CRUD (§5) |
| Migrations                  | `golang-migrate`               | Versioned schema changes (§5). Migration files are kept in `radio-atlantida-backend` |
| Logging                     | `log/slog` (standard library)  | JSON handler, daily file rotation (§10.1) |
| OpenAPI generation          | To be chosen at implementation | Generates the specification from Go code (§6.4) |

**Configuration split**

- **Environment variables** (`caarlos0/env`): the database connection (URL and credentials), secrets (`ICECAST_SOURCE_PASSWORD`, `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD`), and the host paths `MEDIA_ROOT`, `LOG_ROOT`, and `PLAYOUT_JOURNAL_ROOT`. The host paths are set once in the Compose `.env` file and never change. Like the API listen address (`HTTP_ADDR`), the UDP address of the live voice bridge (`LIVE_UDP_ADDR`) and the addresses on which the browser reaches it (`LIVE_PUBLIC_IPS`) are environment variables, because they belong to the compose port mapping (v7-15, `docs/phase-12-plan.md` §2). `PLAYOUT_JOURNAL_ROOT` is the host directory to which Liquidsoap writes its playout journal (§7.1); the backend mounts it read-only. All other backend settings are managed in the admin panel (see §6.3).
- **Admin settings (PostgreSQL)**: the Icecast output profile, station timezone, fade length, outage detection thresholds, and other backend settings are edited in the Angular admin panel and stored in the database (see §6.3).

**Precedence (highest first)**

1. Variables set in the shell or in `docker compose` (`environment:` or `--env-file`).
2. Values from the `.env` file, loaded by `godotenv` only where no variable is already set.

Code defaults are used only until the first value of an admin setting is saved.

## 4. Data Model (PostgreSQL)

Entities are stored in separate tables and linked by IDs. Foreign keys are enforced by the database.

**Terminology.** The word "playlist" is used everywhere: in the database, the API, the code, and the interface. A playlist is a pool of tracks, not a queue. The order of tracks inside a playlist is not significant. The rotation engine chooses the track (§7). The old name "category" is not used.

- **playlist_types**: id, code (unique slug, for example `music`), name (display name), is_system (bool, default false), created_at. The list of types is dynamic: the manager adds new types in the panel and assigns them to playlists.
  - Four system types are created by the first migration: `music`, `jingle`, `news`, `segment` (`is_system = true`). A system type can be renamed (display name) but not deleted. Its `code` never changes, because the engine and the help texts may refer to it.
  - A type that is used by any playlist or track cannot be deleted. The API refuses the request and names the playlists that use it.
  - The type does not change how rotation works. It is a label for the manager (filters and badges in the panel). It does not limit what a clock element can combine: one element may use playlists of different types, for example music and jingle.
- **playlists**: id, name, type_id (FK to `playlist_types`), created_at.
- **artists**: id, name. Used for artist separation rules.
- **tracks**: id, title, artist_id, duration_ms, file_path (relative to the media root, see §8.7), format, bitrate, sample_rate, quality_ok (bool), quality_notes, type_id (FK to `playlist_types`), file_checksum, created_at.
  - Analysis columns (v7-14, `docs/phase-11-plan.md` §2): codec, channels, loudness_lufs, true_peak_dbtp, loudness_range_lu (EBU R128), source_bitrate (the lossy file a transcoded file was made from), quality_issues (`text[]` of `format`, `sample_rate`, `channels`, `loudness`, `true_peak`, `low_bitrate`, `file_missing`), analyzed_at. An analysis sets `quality_ok` to "no issues"; a manual value lasts until the next analysis. `file_checksum` is the SHA-256 of the file, indexed for the duplicate check of an import.
- **track_playlists**: track_id, playlist_id (many-to-many). A track may belong to several playlists.
- **clocks**: id, name, length_seconds (fixed at 3600), status (`complete` or `incomplete`), default_filler_mode (nullable: `random`, `playlist`, or `artist`; null falls back to the global filler setting, §6.3), default_filler_playlist_id (nullable, FK to `playlists`; set only for `playlist`), default_filler_artist_id (nullable, FK to `artists`; set only for `artist`), updated_at. The default mode is chosen in the clock editor (§6.2). It applies when the filler pool is empty or a filler reminder expires (§7); `ask` and `pool` are not allowed here because they would fall back to themselves.
- **clock_elements**: id, clock_id, position, placement (enum: `flow` (default), `start`, `end`, `fixed`), fixed_minute (int 0 to 59, nullable; required when `placement = fixed`), fixed_tolerance_ms (int, default 60000; applies to the start of a fixed element), nominal_duration_ms, duration_tolerance_ms (int, default 30000), filler_mode (enum: `none`, `ask`, `random`, `playlist`, `artist`, `pool`), filler_playlist_id (nullable, FK to `playlists`; set only for `playlist` mode), filler_artist_id (nullable, FK to `artists`; set only for `artist` mode), created_at. Stored as rows; a JSONB column may hold optional per-element settings.
  - `filler_mode` is required on a clock that is saved short. A clock with `filler_mode = none` cannot be saved as complete if it is short.
  - The playlists of an element are stored in `clock_element_playlists`, not in this table.
  - The filler source has one typed column per kind, so the database refuses to delete a playlist or an artist that a clock or an element uses as filler (`ON DELETE RESTRICT`). The API exposes a single `filler_source_id` field and maps it to the column that matches the mode.
- **clock_element_playlists**: clock_element_id, playlist_id (composite primary key). The playlists from which an element takes its track.
  - An element has at least one row. The playlists may be of different types, so one element can mix, for example, music and jingle.
  - The engine selects a track from the union of all tracks of these playlists, without duplicates (see §7).
- **clock_filler_pool**: id, clock_id, track_id, position, used_at (timestamptz, nullable). A pool of specific tracks chosen for a clock. Used in position order; `used_at` is set when a track is taken for filler. Rows are not deleted when used, so the history stays visible.
- **filler_requests**: id, clock_id, hour_start_utc, reason (`short_clock`, `pool_empty`), created_at, deadline_at, mode, answer_track_id (nullable), status (`pending`, `answered`, `defaulted`, `expired`).
  - At most one `pending` request per `(clock_id, hour_start_utc)`, enforced by a partial unique index.
- **grid_slots**: id, weekday (1 = Monday to 7 = Sunday), hour, clock_id, valid_from, valid_to (dates, both inclusive; null `valid_to` is open-ended). Weekday and hour are in station local time (see §10.2). Covers up to 30 days ahead.
  - At most one clock per weekday and hour on any date: an exclusion constraint forbids two slots with the same weekday and hour and overlapping date ranges.
  - Saving a slot that overlaps an existing one replaces the existing slot for the overlapping dates, in one transaction, and the result names the replaced slot. A partly covered slot is trimmed or split to the dates outside the new one. A fully covered slot is deleted, whether or not it has aired; its history rows keep their `clock_id` and lose only `grid_slot_id` (`ON DELETE SET NULL`).
- **scheduled_items**: id, grid_slot_id, clock_id, track_id, planned_start_utc, planned_in_ms, planned_out_ms, truncated (bool, default false), source (`clock`, `filler_pool`, `filler_random`, `filler_playlist`, `filler_artist`, `filler_answer`, `reschedule`), status (`planned`, `queued`, `aired`, `missed`, `replaced`), outage_id (nullable, FK to `broadcast_outages`), missed_at (timestamptz, nullable), reschedule_of_id (nullable, FK to `play_log`: the missed item this item replaces; set only when `source = reschedule`), created_at. The rolling track plan, covering 24 to 48 hours ahead.
  - `reschedule_of_id` points to `play_log`, not to `scheduled_items`, because a missed item is moved to `play_log` and removed from `scheduled_items` (§4.1).
  - `missed` means the item was planned but did not reach the air because of a broadcast outage (see §7.1).
  - `clock_element_id` (nullable, FK to `clock_elements`, `ON DELETE SET NULL`): the element the item was planned for; null for filler. `filler_pool_id` (nullable, FK to `clock_filler_pool`, `ON DELETE SET NULL`): the pool row a `filler_pool` item took; a rebuild of the hour sets that row's `used_at` back to null (v7-11).
- **plan_hours**: hour_start_utc (primary key), clock_id (FK, `ON DELETE CASCADE`), clock_updated_at, grid_slot_id (nullable, `ON DELETE SET NULL`), seed, built_at, filler_request_id (nullable, `ON DELETE SET NULL`). One row per built hour of the track plan (v7-11, `docs/phase-8-plan.md` §2). An hour is stale when the grid now assigns another clock or none, or the clock's `updated_at` changed; the seed makes a rebuild after a reminder answer reproducible.
- **hour_inserts**: id, hour_start_utc, track_id (FK, `ON DELETE RESTRICT`), missed_id (nullable, FK to `play_log`, `ON DELETE SET NULL`), placement (`start`, `end`, `fixed`), fixed_minute, fixed_tolerance_ms, created_at. One-time elements of a single hour: a missed item rescheduled into it (§7.1, §7.2). Every build of the hour places them; the clock template is not changed (v7-13, `docs/phase-10-plan.md` §2).
- **backend_heartbeat**: one row, alive_at. Written by the outage detector every 30 seconds, so a restart can tell how long the backend was down (v7-13).
- **broadcast_outages**: id, started_at (timestamptz, UTC), ended_at (timestamptz, UTC, nullable while the outage is open), detected_by (`icecast`, `liquidsoap_callback`, `liquidsoap_control`, `backend`), component (`icecast`, `liquidsoap`, `backend`, `network`), reason (short text, nullable), reconciled_at (timestamptz, nullable). One row per interruption. An open outage has `ended_at` null.
- **play_rules**: id, name, window_from (time, nullable), window_to (time, nullable), weekdays (array of 1 to 7, nullable; null means every day), min_interval_minutes (int, nullable), max_per_hour (int, nullable), notes, created_at. A reusable, named rule (see §7.3). Times are station local time (§10.2).
  - Every constraint is optional, but at least one must be set. `window_from` and `window_to` are set together or not at all. A window may cross midnight (for example, 22:00 to 02:00); the weekday then refers to the day on which the window starts.
- **track_play_rules**: track_id, rule_id (composite primary key). Rules applied to a single track.
- **playlist_play_rules**: playlist_id, rule_id (composite primary key). Rules applied to every track of a playlist. The rule is checked for each track separately.
  - A rule that is applied to any track or playlist cannot be deleted. The API refuses the request and lists the tracks and playlists that use it.
  - These tables replace the old `daypart_rules` table. A time window is one kind of play rule.
- **separation_rules**: id, scope (track or artist), min_interval_hours (default 72), notes. The values are copied from the reference software and entered into this table. Rotation reads all separation values from here; none are hard-coded.
- **play_log**: id, track_id, artist_id, status (`aired`, `truncated`, `missed`), planned_start_utc, started_at (nullable for missed items), planned_out_ms, actual_duration_ms (nullable for missed items), truncated (bool, default false), source (`auto`, `live`, or `emergency`; see §8.6), clock_id, grid_slot_id, scheduled_item_id (nullable), outage_id (nullable, set for missed items), resolution (`open`, `rescheduled`, `dismissed`, nullable; used only for missed items), resolved_at (timestamptz, nullable), resolved_item_id (nullable, set when rescheduled). Records what aired and what did not.
  - `ended_at` (timestamptz, nullable): set when the end of the track is recorded; null while it plays and for missed items. The first recorded end wins, because Liquidsoap may report the end of a track twice (v7-12, `docs/phase-9-plan.md` §2). A unique partial index on `scheduled_item_id` makes a repeated start a no-op.
  - `scheduled_item_id` and `resolved_item_id` are trace ids of `scheduled_items` rows, without a foreign key: those rows are deleted once they air or are missed (§4.1), so a foreign key would point at nothing. Aired rows are kept for 10 days (see §4.1). Missed rows are kept until resolved.
- **settings**: key, value (JSONB), updated_at (timestamptz, UTC). Holds all backend settings edited in the admin panel, including the audio output profile (see §6.3 and §8.8), `fade_out_ms` (default 2500, allowed 2000 to 3000), and outage detection thresholds (see §6.3).
- **Deletion and history.** A track, artist, or clock that is referenced by `play_log` or `scheduled_items` cannot be deleted (`ON DELETE RESTRICT`). The API refuses the request and says why. This keeps the played-tracks list and open missed items complete. A grid slot can be deleted; the history rows keep their other references and lose only `grid_slot_id`.
- **operations**: id, kind, status (`running`, `completed`, `failed`), started_at, finished_at, error_message, result_summary (see §6.5).

Indexes:

- `play_log (track_id, started_at DESC)` for track separation checks. Only rows with status `aired` or `truncated` count.
- `play_log (artist_id, started_at DESC)` for artist separation checks. Same status filter.
- `play_log (status, planned_start_utc)` and `play_log (outage_id)` for the played-tracks list and missed-item handling.
- `playlists (type_id)` and `tracks (type_id)` for the check that a playlist type is unused before deletion.
- `track_playlists (playlist_id)` for candidate selection, and `track_playlists (track_id)`.
- `track_play_rules (track_id)`, `track_play_rules (rule_id)`, `playlist_play_rules (playlist_id)`, and `playlist_play_rules (rule_id)` for rule lookup during planning and for the in-use check.
- `clock_elements (clock_id, position)`.
- `clock_element_playlists (playlist_id)` for the check that a playlist is unused before deletion.
- `clock_filler_pool (clock_id, position)` and `clock_filler_pool (clock_id) WHERE used_at IS NULL`.
- `filler_requests (clock_id, hour_start_utc) WHERE status = 'pending'` (unique).
- `scheduled_items (planned_start_utc)` and `scheduled_items (track_id, planned_start_utc DESC)` for separation checks against planned items.
- `scheduled_items (status, outage_id)` for outage reconciliation.
- `broadcast_outages (started_at)` and a partial unique index `broadcast_outages (ended_at) NULLS NOT DISTINCT WHERE ended_at IS NULL` so there is at most one open outage. `NULLS NOT DISTINCT` is required, because in a plain unique index two NULLs never collide.
- `grid_slots (weekday, hour)`.

### 4.1 Play log retention

The play log keeps aired history only for the last 10 days. Missed items are an exception.

- A scheduled job (Go scheduler or `pg_cron`) runs nightly and executes `DELETE FROM play_log WHERE status IN ('aired', 'truncated') AND started_at < now() - interval '10 days'`.
- Rows with status `missed` and `resolution = 'open'` are never deleted by retention. They stay until the manager resolves them (see §7.1). Resolved missed rows follow the 10-day rule, counted from `resolved_at`.
- The table holds at most millions of rows for this library size. PostgreSQL handles that without partitioning, and autovacuum reclaims the space.
- The retention job is a scheduled job, not a trigger on insert.
- Rotation only needs the separation window (default 3 days), so a 10-day retention covers it with margin. If a separation rule ever exceeds 10 days, the retention period must be revisited.
- `scheduled_items` is not subject to this retention. Rows with status `aired` are moved to `play_log` and removed from `scheduled_items` after they air. Rows with status `missed` are moved to `play_log` in the same way, with `resolution = 'open'`, and removed from `scheduled_items`.

## 5. Database Access

- Every query is written in the repository layer (§5.1), never in domain code, handlers, or jobs.
- **GORM** is used for administrative CRUD: tracks, playlists, playlist types, clocks, clock elements, filler pools, grid, separation rules, play rules. It fits well because these are simple entities with associations. Nested inserts (for example, a track with its playlists, or a clock with its elements, their playlists, and the pool) run inside explicit transactions.
- **pgx** (optionally with **sqlc** for generated type-safe queries) is used for hot paths: play log writes, track plan generation, rotation candidate queries, separation and play rule checks against both `play_log` and `scheduled_items`, and outage reconciliation.
- Inserts that touch several tables are always done in an explicit transaction, so order and rollback are under control.
- Migrations are versioned and forward-only (golang-migrate, `.up.sql` files only; see §9.2). GORM auto-migrate is not used.

### 5.1 Repository layer

All database access goes through a repository layer. Business logic never contains SQL, and SQL never leaves the repository layer.

- **Location and naming.** Repositories live in `internal/repository/<domain>`. The package name equals the name of the domain package it serves (`library`, `playrules`, `separation`, `clocks`, `grid`, `rotation`, `filler`, `playout`, `outage`, `live`, `settings`, `operations`, `retention`). When a domain is renamed, its repository is renamed in the same change. The layer is organized by layer, but it uses the domain vocabulary only, so it adds no second set of names.
- **What a repository contains.** The SQL of its domain (GORM and pgx code, sqlc-generated code included), the row structs used for scanning, and the mapping from rows to domain types. One file per aggregate, for example `playlists.go` and `tracks.go`.
- **What a repository must not contain.** Business rules (eligibility, separation, fitting, tolerance decisions), HTTP or DTO types, network calls, and transaction boundaries. A repository never begins or commits a transaction.
- **Interfaces.** The domain package declares the interface it needs, next to where it is used (for example `library.PlaylistStore`). The repository implements it. Interfaces are small and contain only the methods that domain uses.
- **Transactions.** The domain service opens a transaction with `db.WithTx` and passes the transaction handle to each repository it touches through `WithTx(tx)`. A multi-table write therefore has one transaction that spans every repository involved (§5, explicit transactions).
- **Errors.** Repositories map SQLSTATE codes and constraint names to the domain's sentinel errors (`ErrNotFound`, `ErrInUse`, `ErrValidation`) and wrap every other error with context. A domain never sees a driver error (`pgconn.PgError`) or a GORM error.
- **Testing.** Each repository has integration tests against a real PostgreSQL (testcontainers). Tests may use SQL for fixtures and assertions.
- **Enforcement.** An architecture test fails if SQL statements, or imports of `gorm.io/gorm` and `github.com/jackc/pgx`, appear outside `internal/repository`, `internal/db`, `internal/dbtest`, and `internal/migrations`. Migrations are not queries, so their SQL files stay where they are.

## 6. API and Data Transfer

- REST over JSON for commands and data queries.
- WebSocket for UI events from the backend to the panel: now playing, track progress, live on/off, Liquidsoap state, Icecast connection state, outage start and end, missed-item updates, error alerts, filler requests, and the outcome of asynchronous operations (§6.5). The panel does not poll for these.
- Error alerts are shown in the panel as a popup window with the error text. Errors are also written to the log file (see §10.1).
- All timestamps in the API are ISO 8601 in UTC (ending with `Z`).
- Database structs are never sent to or accepted from the frontend directly.
- Separate DTOs (request and response structs with `json` tags) are defined for each endpoint.
- Validation happens on DTOs before any database write.
- The API is not authenticated. It must be bound to localhost or a private network and must not be exposed to the internet (see §10).

### 6.0 Clock save flow

Saving a clock is a two-step exchange, so the operator cannot miss a shortfall.

1. `POST /api/clocks` or `PUT /api/clocks/{id}` runs the dry check and returns:
   - `shortfall_ms` (0 if the clock is full);
   - `warnings` (for example, a playlist with too few tracks for the separation window, or an hour of the day in which no track passes the play rules, see §7.3);
   - `requires_filler_choice: true` when a segment is short and its holder element has no filler mode yet (a choice already stored with the clock is not asked again); `short_segments` lists those segments.

   The dry check and the save are synchronous. Shortfall is measured on the elements' nominal durations (decisions of 2026-10-14, `docs/phase-7-plan.md` §2). The response also carries `build_operation_id`: the `clock_build` operation that rebuilds the clock's planned hours (§6.5). Grid assignments return it too.

   The request body lists the elements, and each element carries `playlist_ids` (one or more). The clock itself may carry `default_filler_mode` with its `default_filler_playlist_id` or `default_filler_artist_id`.
2. If `requires_filler_choice` is true, the panel opens a blocking dialog. The operator chooses a mode and then calls one of:
   - `POST /api/clocks/{id}/save-with-filler`: body includes `segments`, one choice per short segment (`segment`, `filler_mode`, `filler_source_id` if needed), and optionally `pool_track_ids` (ordered list, for `pool` mode). Each choice is stored on the segment's holder element (§7.2). The clock is saved with `status = complete`.
   - `POST /api/clocks/{id}/save-incomplete`: the clock is saved with `status = incomplete`. The backend creates reminders (see §7).
3. Closing the dialog without a choice is treated the same as `save-incomplete`.

Validation errors from the dry check are returned per field, so the panel can highlight the element that causes them.

### 6.1 Admin Panel Help

Every admin page (library, playlists, playlist types, clocks, grid, play rules, separation rules, play log, track plan, filler reminders, missed items, logs, settings) has a help button in its header. The manager may forget what a page is for, so the help explains it on demand.

- **Help panel**: clicking the help button opens a side panel or popover with what the page is, what its entities mean, how to use them, typical steps, and common mistakes. For example, the Clocks page explains what a clock is, what its 60-minute structure means, how tolerances work, how an element can use several playlists, what filler modes do, and what the clock's default filler mode is: it is used when the filler pool is empty or a filler reminder expires, and if the clock has none, the global fallback applies. The Playlists page explains that a playlist is a pool and not a queue (the order of tracks inside it is not important, the system chooses the track), that a track can be in several playlists, and how playlists feed clock elements and rotation. The Playlist types page explains what a type is and how to add one. The Play rules page explains that a rule limits when a track may play, how the time window, the minimum interval, and the per-hour limit combine, how to apply a rule to tracks or playlists, and that how often a jingle appears is set by the clock, not by the rule. The Played tracks page explains what "missed" means and how to reschedule or dismiss a missed item.
- **Per-operation notes**: create, edit, and delete modes can each show a short note. For example, deleting a playlist that is used by clock elements is blocked, deleting a playlist type that is used by playlists or tracks is blocked, and deleting a play rule that is applied to tracks or playlists is blocked. The note says why.
- **Field hints**: individual form fields show short tooltips (for example, "nominal duration", "tolerance", "filler mode", "default filler mode", "min interval hours", "playlist type", "minimum interval", "plays per hour") and link to the full page help.
- **Content location**: help texts are static content in the Angular project (for example, i18n JSON files keyed by page ID such as `help.clocks`). They ship with each release and stay in sync with the UI, and they do not depend on the backend or the database. The files live in `radio-atlantida-frontend`.
- **Writing style**: plain language, one short worked example per page.

## 6.2 Form Validation

Validation happens in two layers. The frontend gives immediate feedback, and the backend is the authority.

**Frontend (Angular)**

- Forms use Angular Reactive Forms with validators (`required`, `min`, `max`, `pattern`) on each control.
- Fields use HTML5 input types so the browser restricts input where it can:
  - `type="number"` with `min`, `max`, and `step` for numeric values (nominal duration, tolerance, bitrate, sample rate, min interval hours, play rule interval in minutes, plays per hour, outage thresholds). Text cannot be typed into these fields.
  - `type="time"` for the play rule `window_from` and `window_to`, so the value is always `HH:MM`.
  - `type="date"` for `valid_from` and `valid_to` in grid slots.
  - Checkboxes or a multi-select for weekdays, instead of free text.
  - A select for the timezone, populated from IANA names, instead of typing.
  - A select for `filler_mode`, with options that match the backend enum.
  - In the clock editor, a select for the clock's default filler mode (`random`, `playlist`, `artist`, or none, meaning the global fallback). A playlist or artist picker is shown only for the matching mode. `ask` and `pool` are not offered, because they cannot be a fallback (§4).
  - A select for the playlist type, populated from `GET /api/playlist-types`, so a new type appears in the lists as soon as it is added.
  - A multi-select for the playlists of a clock element. It offers playlists of all types, so one element can combine, for example, music and jingle playlists.
  - A multi-select for play rules on the track form and the playlist form, and a multi-select of tracks and playlists on the play rule page.
  - A select for the target clock and element when rescheduling a missed item, populated from the grid, so only valid targets can be chosen.
  - `type="checkbox"` for boolean fields such as `quality_ok` overrides.
- Error messages appear next to the field, in plain language (for example, "Enter a number between 1 and 3600").
- The Save button stays disabled while the form has errors.
- The filler dialog (§6.0) cannot be dismissed by clicking outside it. It can only be closed by choosing a mode or by the explicit "save incomplete" action.

**Backend (Go)**

- Every request is validated again on the DTO before any database write (§6). Frontend checks can be bypassed, so the server never trusts them.
- The API returns validation errors per field, so the panel can show them next to the right input.
- Business rules that need data are checked in the backend, not only in the form. For example:
  - a playlist used by clock elements cannot be deleted;
  - a playlist type used by playlists or tracks cannot be deleted, and a system type cannot be deleted at all;
  - a clock element must have at least one playlist;
  - a play rule must set at least one constraint, and a rule that is in use cannot be deleted;
  - a pool track that breaks separation is refused;
  - a missed item is rescheduled into a clock that has no room.

### 6.3 Settings Administration

Everything the backend needs is managed by the manager in the Angular admin panel, except the database connection, secrets, and the host paths. The manager should not have to edit files or environment variables during normal use.

**Environment only (not editable in the UI)**

- Database connection (`DATABASE_URL` and credentials).
- Secrets: `ICECAST_SOURCE_PASSWORD`, `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD`.
- Host paths: `MEDIA_ROOT`, `LOG_ROOT`, and `PLAYOUT_JOURNAL_ROOT`. They are needed by Docker bind mounts before the backend starts, so they are set once in the Compose `.env` file and never change.

**Managed in the UI (settings pages)**

- **Station**: timezone (default `Asia/Yerevan`), station name.
- **Playlist types**: the list of types (system and custom). The manager can add a type, rename it, and delete a custom type that is not in use. System types can be renamed but not deleted (§4).
- **Stream output (Icecast)**: host, port, mount point, codec, bitrate, sample rate, channels, loudness target, stream metadata, HLS on or off.
- **Liquidsoap**: default music gain, the music level under live voice (`liquidsoap.duck_gain`, default 0.2; v7-15), fade lengths (including `fade_out_ms`, default 2500, allowed 2000 to 3000), harbor input port, control host and port, and the media directory as Liquidsoap sees it (`liquidsoap.media_root`, default `/media`).
- **Playout**: the emergency playlist (§8.6), and how long before its start an hour's first items are pushed to Liquidsoap (`playout.prepush_minutes`, default 5). From then on the hour is frozen: it is not rebuilt, and its reminders cannot be answered.
- **Rotation**: planning horizons (grid up to 30 days, track plan 24 to 48 hours), lookahead depth (2 to 3 tracks), default separation values, tolerance defaults, the shortest gap that gets filler (`rotation.min_filler_gap_ms`, default 10 s), and the limit of the emergency tolerance relaxation (`rotation.max_relaxed_tolerance_ms`, default 120 s). The global fallback filler mode, used when a clock has no default filler mode of its own (§4, §6.2).
- **Filler**: default deadline for filler reminders (default 2 hours before air), default fallback mode (`random`).
- **Outage detection**: heartbeat timeout for the Liquidsoap callback and control link (default 10 seconds), Icecast mount check interval (default 5 seconds), and the number of consecutive failed checks before an outage is opened (default 2).
- **Retention**: play log days (default 10), application log days (default 10). Missed items are not subject to the play log days setting while they are open (see §4.1).
- **Import** (v7-14): the inbox folder (`import.inbox_dir`, default `incoming`) and the library folder (`import.library_dir`, default `library`) under the media root; the broadcast standard of track files: target codec (`mp3`, `aac`, or `vorbis`; default `mp3`), bitrate (320 kbps), sample rate (44100 Hz), channels (2); the lowest acceptable bitrate of a lossy file (128 kbps), the loudness tolerance around `stream.loudness_target_lufs` (2 LU), and the highest true peak (−1 dBTP).
- Any future backend setting follows the same pattern.

Playlist types are data in the `playlist_types` table, not entries in the `settings` table. They are listed on the settings pages for convenience, and they are served by their own endpoints (§6.4). A clock's default filler mode is data of the clock (§4), so it is edited in the clock editor, not on the settings pages.

**Storage and validation**

- Settings live in a `settings` table in PostgreSQL (see §4). Each setting has a typed definition in code, so the backend validates the value before saving.
- Defaults are defined in code and used until a value is saved.

**Applying changes**

- Each setting states whether it takes effect immediately (for example, music gain or log level) or needs a reconnect (for example, Icecast host or mount point). Changing the stream server briefly interrupts the stream.
- Changes that affect air are logged with a UTC timestamp (§12).

**Secrets**

- Secrets are never returned by the API. The UI shows only whether a secret is set.
- Changing a secret means changing the environment and restarting the service. It is not done in the UI.

**Help and validation**

- Settings pages follow §6.1 (help) and §6.2 (validation).

### 6.4 API Contract and Client Generation

The backend is the single source of truth for the API. There is no separate frontend specification.

- The Go backend (`radio-atlantida-backend`) generates an **OpenAPI specification** from its code. It covers every REST endpoint with its DTOs and the WebSocket event payloads, which are defined as schemas in the same document.
- The Angular project (`radio-atlantida-frontend`) generates its **TypeScript client** from that specification at build time. The frontend never writes request or response types by hand.
- The specification is produced by the backend build, so a change to a DTO in Go shows up as a compile-time error in the Angular build until the frontend is updated.
- The specification and the generated client are versioned together with the release tag (§9.2), so frontend and backend always match.

**Endpoints added in v7-6 (outage and played-tracks handling)**

| Method and path | Purpose |
|-----------------|---------|
| `GET /api/play-history?from=&to=&status=` | Played-tracks list: aired, truncated, and missed items in one list, newest first. `status` filter is optional. Times in UTC |
| `GET /api/missed-items?resolution=open` | Missed items awaiting a decision (default: open items) |
| `POST /api/missed-items/{id}/reschedule` | Body: `target_hour_start_utc`, `placement` (`start`, `end`, or `fixed`), `fixed_minute` (required when `placement = fixed`), optional `fixed_tolerance_ms`. Creates a one-time `reschedule` item in that hour (§7.2); returns `accepted` with an operation ID (§6.5) |
| `POST /api/missed-items/{id}/dismiss` | Marks the missed item `resolution = dismissed`. Synchronous |
| `GET /api/outages?from=&to=` | Outage periods for the played-tracks view and diagnostics |

**Endpoints added in v7-7 (playlists and playlist types)**

| Method and path | Purpose |
|-----------------|---------|
| `GET /api/playlist-types` | List of playlist types (system and custom), with a usage count for each |
| `POST /api/playlist-types` | Body: `code` (unique slug), `name`. Creates a custom type |
| `PUT /api/playlist-types/{id}` | Renames a type (`name`). The `code` of an existing type does not change |
| `DELETE /api/playlist-types/{id}` | Deletes a custom type. Refused with a validation error if the type is a system type or is used by any playlist or track. The error lists the playlists that use it. Synchronous |
| `GET /api/playlists?type_id=` | List of playlists with their type and track count. The `type_id` filter is optional |
| `POST /api/playlists`, `PUT /api/playlists/{id}`, `DELETE /api/playlists/{id}` | Playlist CRUD. `type_id` is required. Delete is refused if the playlist is used by a clock element, and the error names the clocks |
| `PUT /api/playlists/{id}/tracks` | Sets the tracks of a playlist (list of track IDs) |

Clock endpoints (§6.0) take `playlist_ids` (an array) on each element instead of a single category, and accept the clock's `default_filler_mode` with its source.

**Endpoints added in v7-11 (track plan and filler reminders, `docs/phase-8-plan.md` §4)**

| Method and path | Purpose |
|-----------------|---------|
| `GET /api/track-plan?from=&to=` | Built hours (at most 7 days) with their items |
| `POST /api/track-plan/rebuild` | Body: `from`, `to`. Rebuilds the future hours of the range; returns `accepted` with an operation ID (`clock_build`) |
| `GET /api/filler-requests?status=` | Filler reminders (default `pending`) |
| `POST /api/filler-requests/{id}/answer` | Body: `track_id`. Fills the waiting segments of the hour; refused per field when the track breaks separation or a play rule. Synchronous |

**Endpoints added in v7-12 (playout, `docs/phase-9-plan.md` §4)**

| Method and path | Purpose |
|-----------------|---------|
| `POST /api/playout/callback` | Called by Liquidsoap: track `start` and `end`, and the Icecast output state (§8.3). A started plan item moves to `play_log`; repeated events change nothing |
| `GET /api/playout/emergency/next` | Called by Liquidsoap: the next track of the emergency playlist as a request (`text/plain`), or 204 (§8.2, §8.6) |
| `GET /api/playout/status` | Control link, Icecast output, now playing, items waiting in Liquidsoap's queues, music gain |
| `PUT /api/playout/music-gain` | Body: `gain` (0 to 1). Sets the live music gain (§8.5); refused with `liquidsoap_unavailable` when the control link is down |

**Implemented in v7-13 (`docs/phase-10-plan.md` §4).** The v7-6 endpoints above are implemented with these details: `GET /api/play-history` takes `from` and `to` (at most 31 days), an optional `status`, and is keyset paginated (`limit`, `cursor`, `next_cursor`); `GET /api/outages` takes at most 7 days; reschedule refuses the checks that need no build per field (422) and a resolved item with 409 `missed_item_resolved`, and its operation fails when the track breaks a rule at its place or does not fit whole. `POST /api/playout/callback` also takes a `heartbeat` event, sent by Liquidsoap every 5 seconds and not journaled.

**Endpoints added in v7-14 (import, analysis, transcoding, `docs/phase-11-plan.md` §4)**

| Method and path | Purpose |
|-----------------|---------|
| `GET /api/import/inbox` | Files waiting in the inbox folder (at most 1,000) |
| `POST /api/import` | Body: `type_id`, `playlist_ids`. Imports the inbox; returns `accepted` with an operation ID (`import`) |
| `POST /api/tracks/analyse` | Body: `track_ids` (absent: every track never analysed), `force`. Returns `accepted` (`analyse`) |
| `POST /api/tracks/transcode` | Body: `track_ids`, `fix` (`loudness` or `optimize`), `output` (`replace` or `copy`). Returns `accepted` (`transcode`) |

Track responses carry the analysis columns, `quality_issues`, and `fixes` (the transcodes that remove at least one issue); `GET /api/tracks` takes `quality` (`ok`, `issues`, `unanalysed`). Import, analysis, and transcode run one at a time: a second request is refused with 409 `media_operation_running`; without ffmpeg they are refused with 503 `media_tools_unavailable`.

**Endpoints added in v7-15 (live voice, `docs/phase-12-plan.md` §4)**

| Method and path | Purpose |
|-----------------|---------|
| `POST /api/live/session` | Body: `offer` (the browser's WebRTC SDP, candidates gathered), `takeover`. Returns the answer SDP and the session ID. One session at a time: 409 `live_session_active` unless `takeover`; 422 `live_offer_invalid`; 503 `live_unavailable` |
| `DELETE /api/live/session` | Ends the live session. Synchronous |
| `GET /api/live/status` | State of the live input (`off`, `connecting`, `on_air`, `reconnecting`), session, harbor connection, packets |

`GET /api/status/live` now answers `on_air` (a live session sends audio), `open_outage_id`, and `current_clock` (the clock the grid assigns to the current hour). The live state is sent as the `live.state` event.

**Endpoints added in v7-8 (play rules and track preview)**

| Method and path | Purpose |
|-----------------|---------|
| `GET /api/play-rules` | List of play rules with the number of tracks and playlists that use each |
| `POST /api/play-rules`, `PUT /api/play-rules/{id}`, `DELETE /api/play-rules/{id}` | Play rule CRUD. Body: `name`, `window_from`, `window_to`, `weekdays`, `min_interval_minutes`, `max_per_hour`, `notes`. Delete is refused if the rule is in use |
| `PUT /api/play-rules/{id}/targets` | Body: `track_ids` and `playlist_ids`. Sets the tracks and playlists the rule is applied to |
| `PUT /api/tracks/{id}/play-rules` | Body: `rule_ids`. Sets the rules of one track |
| `PUT /api/playlists/{id}/play-rules` | Body: `rule_ids`. Sets the rules of one playlist |
| `GET /api/tracks/{id}/audio` | Audio stream of the track for preview in the panel (§6.6). Supports HTTP Range |

### 6.5 Asynchronous Operations and WebSocket Events

Some backend operations take time and run asynchronously. Examples: building a track plan for an hour, extending the plan, importing and analysing a batch of files, transcoding, regenerating filler pools, purging old logs, reconciling playout after an outage, marking missed items, and rescheduling a missed item. The panel cannot wait for these in an HTTP request, so the backend reports their outcome over WebSocket.

**Rule: every asynchronous operation emits a WebSocket event when it finishes.**

- The HTTP request that starts the operation returns at once with an operation ID and status `accepted`. It does not wait for the work to complete.
- When the operation finishes, the backend sends exactly one terminal event over the WebSocket hub: either a success event or a failure event. The event carries the operation ID.
- Long operations may also send progress events (for example, "12 of 40 files analysed"). Progress is optional. The terminal event is mandatory.
- Event types are named `<operation>.completed` for success and `<operation>.failed` for failure (so the plan job ends with `track_plan.completed`, not `track_plan.built`), for example `track_plan.completed`, `track_plan.failed`, `clock_build.completed`, `clock_build.failed`, `import.completed`, `import.failed`, `missed_items.completed`, `missed_items.failed`, `reschedule.completed`, `reschedule.failed`, `playout_reconcile.completed`, `playout_reconcile.failed`, `analyse.completed`, `transcode.completed` (and their `.failed` and `.progress` events).
- A failure event contains a short error message, the component that failed (§10.1), and the UTC time. The same error is written to the log file (§10.1) and shown in the panel as an error popup (§6, §10.1).
- A success event contains the operation ID, a short result summary, and the UTC time, so the panel can refresh the affected view.
- The event schemas are part of the OpenAPI contract (§6.4). Each event type is a named schema, so the TypeScript client has typed handlers for every event.

**Outage events (not operations, sent when the state changes)**

- `broadcast_outage.started`: outage opened, with `started_at`, `component`, and `reason`.
- `broadcast_outage.ended`: outage closed, with `started_at`, `ended_at`, and the number of items marked missed.
- `icecast.state`: connection state of the Icecast output (connected, disconnected, reconnecting).
- `live.state`: state of the live voice input (`off`, `connecting`, `on_air`, `reconnecting`), with the reason when a session ends (v7-15).

**Operation tracking**

- Each asynchronous operation has a row in an `operations` table: id, kind, status (`running`, `completed`, `failed`), started_at, finished_at, error_message, result_summary. The table is the source of truth for the operation state. A panel that reconnects can read it through the REST API to catch up on events it missed.
- Events are not the only record. If the WebSocket connection is down when an event is sent, the operation still ends in its stored state, and the panel reads that state on reconnect.
- The `operations` table follows the same retention as the logs (10 days).

**Examples**

- The operator saves a clock. The save returns `accepted` with an operation ID. When the clock's hours are recalculated, the backend sends `clock_build.completed`. If a track fails the rules and no candidate is found, it sends `clock_build.failed` with the reason.
- The rotation engine extends the track plan for the next hour. On success it sends `track_plan.completed`. On failure it sends `track_plan.failed` and creates a filler request or emergency warning as described in §7.
- Icecast drops. The outage detector opens an outage and sends `broadcast_outage.started`. When Icecast reconnects, the outage closes, the missed items are marked, and the backend sends `missed_items.completed` with the count, then `broadcast_outage.ended` with the same count.

**Scope**

- The rule applies to every operation that runs asynchronously in the backend, including those started by the scheduler, not only those started by the operator.
- Synchronous requests that finish within the HTTP response do not need a WebSocket event.

### 6.6 Track Preview

The manager and the operator can listen to any track in the panel, without leaving the admin and without looking for the file on disk.

- **Play button.** A play button is shown next to every track in the library, in playlists, on the play rule page, and in the track pickers (filler pool, filler dialog, rescheduling a missed item). The panel has one shared player: starting a track stops the previous one. The player shows the title and artist, a progress bar with seek, and a volume control.
- **Endpoint.** `GET /api/tracks/{id}/audio` streams the file. The backend finds the track by its ID and resolves `file_path` under `MEDIA_ROOT` itself. The client never sends a path, so path traversal is not possible. The endpoint supports HTTP Range requests, so seeking works. The content type comes from the stored format. The backend reads the file through the existing read-only media mount (§8.7).
- **Browser only.** The preview plays in the operator's browser. It does not go to Liquidsoap or to air, it is not written to `play_log`, it does not count toward separation or any play rule, and it does not change the track.
- **Formats.** MP3, AAC, and Ogg Vorbis play in current browsers. If the browser cannot play the stored format, the panel shows a short message. A transcoded preview through ffmpeg is not part of this version.
- **Missing file.** If the file is not found, the endpoint returns 404. The panel shows an error popup (§6, §10.1) and the error is logged with component `api`.
- **Load.** Streaming is plain file serving and must not affect the air. The endpoint is served by the normal API process and does no heavy processing.
- The API is not authenticated (§6), so this endpoint is subject to the same network restrictions as the rest of the API (§10).

## 7. Rotation Engine (Go)

The engine works on three horizons, each with its own job.

**Grid (up to 30 days).** The grid is read, not computed. It says which clock plays in each hour. Changing the grid marks affected planned items as stale (see "Grid and clock changes" below).

**Track plan (rolling 24 to 48 hours).** Every hour, Go extends the plan so that it always covers the configured window. For each upcoming hour with a clock:

1. **Select tracks per element.** For each clock element, build the candidate set as the union of the tracks of all its playlists (`clock_element_playlists`), without duplicates. A track that is in two of the element's playlists is one candidate. From this set, choose a track that honors play rules (§7.3) and separation rules, and whose duration fits the remaining time within `duration_tolerance_ms`. Every candidate has the same weight, so a larger playlist contributes more candidates. Tracks are never altered.
2. **Check the fit.** Sum the durations of the chosen tracks.
   - **Shortfall.** If time remains after all elements, add one filler track (see "Filler" below). Repeat until the shortfall is closed or the iteration limit is reached.
   - **Overflow.** If a track runs past the end of its segment (see §7.2), it is the crossing track. The crossing track is kept, its `planned_out_ms` is set to the segment end, it is marked `truncated`, and a fade-out of `fade_out_ms` ends at that point. Every planned track after the crossing track in the same segment is removed from the plan. Removed tracks are neither aired nor missed, they do not count toward separation, and they return to their playlist. Overflow never triggers filler. The same rule applies at the hour boundary, where the segment end is the end of the hour. The next clock does not depend on this track.
3. **Write `scheduled_items`** with status `planned`.

Separation and play rule checks query both `play_log` (what aired, status `aired` or `truncated`) and `scheduled_items` (what is planned). A track counts as blocked if it is in either. Items with status `missed` count as not aired and do not block anything. All separation values are read from the `separation_rules` table.

**Filler.** When a clock is short, the filler mode decides the source:

- `pool`: take the first row of `clock_filler_pool` with `used_at IS NULL`, set `used_at`, and use that track. The track is still checked against separation and play rules. If it fails, the next pool row is tried, and the failed one is reported to the operator. If the pool is empty, the clock's default mode applies and a `filler_request` with `reason = 'pool_empty'` is created.
- `random`, `playlist`, `artist`: choose a track that matches the source and passes the rules. For `playlist` mode, the source is one playlist (`filler_playlist_id`); for `artist` mode, one artist (`filler_artist_id`). The plan row records the mode in `source` (`filler_random`, `filler_playlist`, `filler_artist`).
- `ask`: create a `filler_request` for the hour. The operator is asked in the panel. If there is no answer by `deadline_at`, the clock's default mode applies and the status becomes `defaulted`.
- `none`: not allowed for a short clock that is saved as complete. For an incomplete clock, it means no filler is created, and the warning stays visible.

The clock's default mode (`clocks.default_filler_mode`) is the fallback used by `pool` and `ask`. If the clock has none, the global fallback from §6.3 applies.

**Filler reminders.** Every incomplete clock creates a `filler_request` for each of its hours that falls inside the planning window. Reminders cannot be turned off. An operator closes one by answering it or by letting its deadline pass. `deadline_at` is calculated from air time, not from creation time, so a request created a week ahead still fires at the right moment.

**Grid and clock changes.** When a clock or the grid changes:

- `scheduled_items` with status `planned` in the affected hours are recalculated.
- Items with status `queued`, `aired`, or `missed` are not touched.
- `filler_requests` in `pending` for the affected hours are set to `expired`, and new ones are created if the clock is still incomplete.

**Emergency case.** If no candidate passes the rules:

1. Relax the tolerance for the element, within a defined limit.
2. If still no candidate, take any track from the general pool that passes separation and play rules.
3. If the pool is empty, play the emergency playlist (see §8.6) and send a warning to the panel.

The number of iterations at each step is bounded.

The order of resolution is: tolerance selection, then filler for shortfall, then truncation for overflow, then emergency. Stretching audio is not used at any point.

The engine is deterministic for a given input and seed, which makes it testable.

**Implementation decisions (v7-11).** `docs/phase-8-plan.md` §2 records how the rules above are applied with real tracks: random choice with equal weights for ordinary elements and best fit for the last element of a segment; the crossing rule at a fixed element's window (an item past the latest start is cut at the target minute); filler only for gaps of at least `rotation.min_filler_gap_ms`; reminders per hour for short segments whose holder is `none` or `ask`, resolved by an answer (`filler_answer`) or by the deadline; the emergency steps (tolerance doubled up to `rotation.max_relaxed_tolerance_ms`, then the whole library, then the element is skipped for the emergency playlist); and hours that have started are never rebuilt. Besides the operation events, the planner sends `track_plan.warning` (the warnings of one built hour) and `filler_request.created`.

### 7.1 Broadcast Outages and Missed Items

An outage is a period when the station's audio did not reach listeners, for example because the Icecast server stopped, the internet connection was lost, or the backend lost its control link to Liquidsoap in a way that prevented playout reporting. The system must record what was planned during that period and did not air.

**Outage detection**

- The outage detector runs in the backend and watches three signals:
  1. **Icecast state**: the backend checks the mount point on the Icecast server at the configured interval (§6.3). Liquidsoap also reports its output connection state through its callbacks (§8.3).
  2. **Liquidsoap callbacks**: track start and end events. If no callback arrives within the heartbeat timeout while the station should be on air, the detector treats the playout as stalled.
  3. **Liquidsoap control link**: the backend's connection to the control interface (§8.1). Loss of this link for longer than the heartbeat timeout is treated as an outage while planned items are due: Liquidsoap keeps streaming (its queue, then the emergency source), but planned items cannot be pushed, so they become missed and the manager can reschedule them (decided 2026-10-14, v7-13).
- Liquidsoap sends a heartbeat callback every 5 seconds, so a stalled playout is told apart from a long track (v7-13).
- An outage opens after the configured number of consecutive failed checks, so a single dropped request does not create a false outage. It is written to `broadcast_outages` with `started_at`, `detected_by`, and `component`.
- The outage closes when all signals report normal operation again. `ended_at` is set and `broadcast_outage.ended` is sent (§6.5).
- Detection must not block playout. It runs in its own goroutine and only writes state.

**Marking missed items**

- When an outage closes, every `scheduled_items` row in status `planned` or `queued` (aired items have already moved to `play_log`) whose planned window (`planned_start_utc` to `planned_start_utc` + `planned_out_ms`) overlaps the outage period is examined.
- An item is marked `missed` if it has no confirmed playback in the local playout journal (§8.3). Its `outage_id` and `missed_at` are set, and the reason is stored from the outage.
- Items that were confirmed as played (fully or partly) are not marked missed. A partly played item is recorded with `status = truncated` and its actual duration, as described in §7.
- Marking runs as an asynchronous operation (`missed_items.completed` or `missed_items.failed`, §6.5).
- Missed items are moved to `play_log` with `status = missed`, `resolution = 'open'`, and `outage_id` set, so the played-tracks list shows them next to aired items.

**Implementation decisions (v7-13, `docs/phase-10-plan.md` §2).** The journal decides what aired: an item Liquidsoap started counts as aired even if Icecast was down at that moment. Items that did not air while no outage was open (a leftover flushed at the hour switch, an item more than 30 seconds late) are dropped from the plan like tracks removed by overflow: not missed, no separation count. Before marking or dropping, the journal of the window is replayed through the same idempotent logic as the callbacks, so no cursor is kept. At startup, if the backend was down longer than the heartbeat timeout and planned items did not air, the downtime is recorded as a closed outage (`component = backend`) and its items are marked missed.

**Reconciliation after backend downtime**

- Liquidsoap keeps a local playout journal on disk in `PLAYOUT_JOURNAL_ROOT`: one line per track start and end, with the UTC time and the actual duration. The journal is written independently of the backend, so it survives a backend outage.
- When the backend reconnects, it reads the journal from the last reconciled point and updates the playout state:
  - items confirmed by the journal are set to `aired` or `truncated` and written to `play_log`;
  - items with no confirmation are handled by the outage rules above.
- Reconciliation runs as an asynchronous operation (`playout_reconcile.completed` or `playout_reconcile.failed`, §6.5). The `reconciled_at` field of the outage is set when it finishes.
- The journal is kept for the same 10 days as the play log.

**Played-tracks list**

- The panel's played-tracks list is served by `GET /api/play-history` (§6.4). It shows aired, truncated, and missed items in one list, sorted by start time.
- Missed items are shown with a clear "Not aired" label, their planned time, the clock and hour, and the outage reason.
- The list can be filtered by status, so the manager can view only missed items.

**Rescheduling and dismissing missed items**

- The manager can reschedule an open missed item into a following clock. `POST /api/missed-items/{id}/reschedule` creates a `scheduled_items` row with `source = reschedule`, `reschedule_of_id` set to the missed item's `play_log` row, and status `planned`. The row is placed in the target hour and element.
- A rescheduled item goes through the same checks as any other item: the element playlists, the duration tolerance, separation rules, and play rules. The placement is chosen by the manager (start, end, or a fixed minute), and the hour is rebuilt around it using the rules of §7.2. If a segment cannot hold the item, the request is refused with a validation error on the request.
- Rescheduling creates a one-time item for that hour only. The clock template is not changed (§7.2).
- On success, the missed item's `resolution` becomes `rescheduled` and `resolved_item_id` is set. The missed row then follows the 10-day retention rule from `resolved_at` (§4.1).
- A missed item can also be dismissed (`POST /api/missed-items/{id}/dismiss`). It then gets `resolution = dismissed` and `resolved_at` set. Dismissal is synchronous.
- Reschedule is asynchronous (`reschedule.completed` or `reschedule.failed`, §6.5). The manager sees the result in the panel.
- Rescheduled items are never placed in an hour that is already on air. The target must be a planned, future hour.

**Air rules during and after an outage**

- The outage does not change the clock grid. Clocks still start and end on their hour boundaries when the stream returns.
- Items planned for the rest of the outage are not pushed late to "catch up". They are marked missed when the outage closes, and the next clock starts on schedule with its own elements.
- If the outage is long, the track plan is still extended for the planning window, so the stream resumes with planned content.

### 7.2 Element Placement and Segments

**Placement.** Each clock element has a `placement`:

- `flow` (default): the element takes its place in order among the other flow elements.
- `start`: the element is placed before all other elements of the hour. Several start elements keep the order in which they were added.
- `end`: the element is placed after all other elements of the hour. Several end elements keep the order in which they were added.
- `fixed`: the element starts at `fixed_minute` within the hour, with a tolerance of `fixed_tolerance_ms` (default 60000, that is ±1 minute). The tolerance applies to the start of the element. Its target start is an exact minute, so the element may begin anywhere from one minute before to one minute after it.

**Segments.** Fixed elements split the hour into segments. A segment runs from the start of the hour (or the end of the previous fixed element) to the target start of the next fixed element, or to the end of the hour. Each segment is fitted separately with the rules of §7. Shortfall is calculated per segment, so the blocking dialog (§6.0) lists every short segment in one pass.

**Building an hour.** When the clock is saved, or the hour is planned:

1. Place the fixed elements at their target minutes. Reject the save if a fixed element's window (`fixed_minute` ± tolerance) falls outside the hour, or overlaps another fixed element's window (see "Replacement" below).
2. Place the `start` elements in the first segment and the `end` elements in the last segment.
3. Fill each segment with its flow elements, selecting tracks by the rules of §7.
4. Check each segment for shortfall and open the filler dialog for each short segment (§6.0). Filler for a segment uses the `filler_mode` of the element that ends the segment, or the clock's default mode if there is none, or the global fallback.
5. Check the crossing rule (§7, "Overflow") at every segment end.

**Refusal on save.** The save is refused, with the error shown on the responsible element, when:

- a fixed element's window falls outside the hour;
- two fixed element windows overlap and the overlap is not handled by replacement (see below);
- the nominal duration of the `start` or `end` elements of a segment does not fit into that segment at all;
- an element has no playlist (§4).

A refused save does not change the template.

**Replacement.** If a new fixed element is saved with a window that overlaps the window of an existing fixed element in the same clock, the new element replaces the existing one:

- the existing fixed element is removed from the clock template (the library tracks and playlists it used are not affected);
- the clock is rebuilt once with the new element in place;
- the save result names the replaced element, so the manager sees what was removed.

Replacement applies only to fixed elements. Flow, start, and end elements are never replaced by this rule.

**Rebuild after a change.** When a clock template changes, planned items in the affected hours are recalculated (§7, "Grid and clock changes"). Items with status `queued`, `aired`, or `missed` are not touched.

**One-time placement for a missed item.** When a missed item is rescheduled (§7.1), the manager chooses the placement (`start`, `end`, or a fixed minute) for that hour only. The clock template is not changed. The same segment, crossing, and replacement rules apply to the plan of that hour. If the one-time fixed item replaces a planned fixed item in that hour, the replaced item gets status `replaced`.

### 7.3 Play Rules

A play rule is a named, reusable set of limits on when a track may play. The manager creates rules on the Play rules page and applies them to tracks or to playlists (§4). They are meant mainly for jingles, but they work for any track. Play rules replace the earlier dayparting.

**What a rule can limit.** Every part is optional, at least one must be set, and all parts of one rule must pass:

- **Time window**: `window_from` to `window_to`, with optional weekdays, in station local time. It is checked against the planned start of the track.
- **Minimum interval**: `min_interval_minutes`. The track does not start again until this many minutes after its last start. Aired and truncated items in `play_log` and planned items in `scheduled_items` count, as in separation. Missed items do not count.
- **Plays per hour**: `max_per_hour`. The maximum number of starts of the track within the same clock hour (station local time), counting planned and aired items.

**Which rules apply to a track.** The rules set on the track itself, plus the rules of every playlist the track belongs to. All of them must pass. A playlist rule is checked for each track separately. It does not count the plays of the playlist as a whole.

**A rule limits, the clock demands.** A rule never puts a track on air by itself. How often a slot appears in the hour is set by the clock elements. The rule decides which track is allowed in that slot at that moment. Play rules and separation are both eligibility checks, and the duration fit is applied to the tracks that pass them (§7). Filler, pool tracks, and rescheduled items go through the same checks.

**Example.** A clock has four fixed elements at :00, :15, :30, and :45, each using the playlist "Jingles", and flow elements that use the playlists "Music" and "Pop". Jingle A has a rule "06:00 to 08:00". Jingle B has a rule "10:00 to 12:00" and a rule "at most once per hour". At 07:15 only jingle A is allowed. At 11:00 only jingle B is allowed, but after it has played once in that hour it is blocked, so the next jingle slots take other allowed jingles. At 09:15 neither A nor B is allowed. If they are the only jingles, that slot is skipped.

**When no track passes.** Play rules are hard limits and are never relaxed, unlike the tolerance. If an element has candidates but all of them are blocked by play rules, the element is skipped. Nothing is planned for it, the manager gets a warning (a `rotation` WARN in the log and a panel alert), and the normal shortfall rules apply to the segment (§7). The emergency steps (§7) are used only when the candidate set of an element is empty for other reasons.

**Dry check on save.** The dry check (§6.0) evaluates each element for the hours in which the clock is assigned in the grid. It warns about hours in which no track passes the play rules.

## 8. Liquidsoap Integration

### 8.1 Playlist feeding (default: request.push)

Go pushes the next items from `scheduled_items` into Liquidsoap's request queue through its control interface (`request.push`). The queue is kept 2 to 3 tracks ahead.

(Here "playlist" means Liquidsoap's own request queue. It is not the library playlist of §4, which exists only in the database and the panel.)

For each item, Go sends:

- the track path (relative to `MEDIA_ROOT`, resolved on the Liquidsoap side);
- the in point (usually 0);
- the out point (`planned_out_ms`, relative to the start of the track), if the item is truncated;
- `fade_out_ms`, applied by Liquidsoap at the out point.

Liquidsoap performs the fade itself, so control-path latency does not affect it.

**Implementation (v7-12, `docs/phase-9-plan.md` §2).** Liquidsoap has two request queues, `hour_0` and `hour_1`; each station hour is pushed into the queue of its parity, and a time switch in the script plays the queue of the current hour (§8.10). Each request is annotated with its `item_id` and, for a truncated item, `liq_cue_out` and `liq_fade_out`. The backend keeps `rotation.lookahead_items` items waiting in the queue of the hour on air, pushes the first items of the next hour `playout.prepush_minutes` before it starts, marks pushed items `queued`, and never pushes an item that is already more than 30 seconds late (no catch-up, §7.1).

### 8.2 Emergency mode: request.dynamic

`request.dynamic` is used only for emergencies, for example when the Go plan is empty or the backend is unavailable and Liquidsoap must ask for the next item itself. It is not the normal path because it makes Go's response time critical.

### 8.3 Feedback and playout journal

- Liquidsoap reports actual playback events to Go (an HTTP callback on track start and end). Go writes them to `play_log`, including `truncated` and the actual duration. The log reflects what aired, not what was planned. A plan item moves from `scheduled_items` to `play_log` when it starts (the `aired` status of `scheduled_items` is not used); its end sets the actual duration, and an item that played more than one second less than planned is `truncated` (v7-12).
- Liquidsoap also reports its Icecast output state (connected or not) in the same callbacks, so the outage detector knows whether listeners could receive the stream (§7.1).
- Liquidsoap writes every track start and end to a local playout journal in `PLAYOUT_JOURNAL_ROOT`, independently of the callbacks (§7.1). If a callback cannot be delivered, the journal still holds the event, and reconciliation uses it after the backend recovers.

### 8.4 Live voice

- The browser captures the microphone and streams it via WebRTC to the Go bridge.
- The Go bridge decodes or transcodes the stream to the format Liquidsoap expects and feeds it into a Liquidsoap input (for example, a harbor input).
- Liquidsoap treats the live input as a source with higher priority than automation.
- Go sends the live state (on/off) to the panel over WebSocket.
- **Latency target:** at most 1 second end to end, from microphone to air. The system runs on a single local machine with local Docker, so the network path adds negligible delay.
- Live voice is not an automation item. Live time is not marked as missed and does not appear as a missed item. If the live session drops during an outage, the automation items for that time are handled by §7.1.

**Implementation (v7-15, `docs/phase-12-plan.md` §2).** The bridge does not decode: the browser's Opus packets are written unchanged as an Ogg/Opus stream (channel count taken from the packets) and sent with the Icecast source protocol to the harbor mount `live`, with the Liquidsoap control credentials. Signalling is one HTTP exchange without trickle ICE; every session uses one UDP port (`LIVE_UDP_ADDR`), and the host candidates carry `LIVE_PUBLIC_IPS`, so the bridge works behind the Docker port mapping. No STUN or TURN (§10). One session at a time; a new offer can take over. A session ends when it is stopped, replaced, gets no audio for 10 s after the answer, has a 5 s dropout, or the backend shuts down. The harbor buffer is 0.3 s. Nothing is written to `play_log`; the log records each session.

### 8.5 Mixing, ducking, and fades

- Music gain is a variable inside Liquidsoap (for example `music_gain`), changed at runtime by Go through the control interface. Typical values: 1.0 (normal), 0.2 (background under voice), 0.0 (music off).
- Automatic ducking (lowering music when voice is present) is implemented inside Liquidsoap, so control-path latency does not affect it. The script mixes with `smooth_add`: while the live input has a source, automation fades over 0.5 s to `duck_gain` (setting `liquidsoap.duck_gain`, set by the backend on connect) and back afterwards (v7-15).
- Go sends explicit commands only for scenarios such as "music off for interview" or "music back on".
- Fades are defined in Liquidsoap to avoid clicks. The end-of-clock fade-out uses `fade_out_ms` (default 2500 ms) and finishes at the hour boundary.

### 8.6 Fallback

- Liquidsoap uses `fallback` so that if live voice drops, automation takes over.
- If the automation source fails, a safety playlist or silence is played.
- Go is informed of state changes but is not required for the fallback to work.
- Items already in `scheduled_items` with status `planned` remain valid during a Go outage. Liquidsoap's queue keeps playing, and the planned tracks that were already pushed are not affected. Their outcome is recorded by the journal and reconciled later (§7.1).
- The emergency playlist is a set of tracks chosen by the operator in settings. It plays when the pool and the plan are both empty. Tracks played from the emergency playlist are logged as `source = emergency` and are not counted as missed items.

### 8.7 Media files (not stored in Docker)

Audio files are about 800 files and must not be copied into Docker images or volumes.

- Media files stay on the host filesystem, outside Docker, in a single directory configured as `MEDIA_ROOT`.
- The host directory is bind-mounted into the containers that need it: `liquidsoap` (read-only) and `backend` (read-write: an import moves files from the inbox folder into the library folder, and a transcode writes fixed files; v7-14).
- PostgreSQL stores only metadata and the path relative to `MEDIA_ROOT`. Relative paths let the same database work on different machines where the absolute location differs.
- Import and quality analysis (ffmpeg) read files from the mounted directory. New files arrive through an inbox folder under the media root; there is no upload through the panel. Transcoded output is written under the media root, so Liquidsoap can play it: the manager chooses whether it replaces the original or is written as `<name>.optimal.<ext>` next to it, the original then staying on disk (decided 2026-10-14, v7-14, `docs/phase-11-plan.md` §2). It is written to a hidden temporary file first and renamed into place, and a track that is queued, on air, or about to be pushed is skipped.
- Checksums are stored so that a moved or changed file can be detected.
- The backend also streams a track to the panel for preview, through the same read-only mount (§6.6).

### 8.8 Audio output configuration

All Icecast and output parameters are settings, not code, so they can be changed per server without a rebuild. The manager edits them in the admin panel (§6.3). Fields include:

- Icecast host, port, mount point, admin and source credentials (source password from environment, not committed);
- codec (for example MP3, AAC, or Ogg Vorbis), bitrate, sample rate, channels;
- loudness target and limiter settings used by Liquidsoap;
- stream name, description, and genre metadata;
- HLS output on or off, if used later.

The Docker Icecast service is a test server. Its connection is entered in the admin panel like any other setting, and switching to the production server is a settings change, not a code or file change. Only the Icecast source password stays in the environment.

### 8.9 Control interface security

- Liquidsoap's telnet and HTTP control ports are bound to localhost or a private Docker network only.
- No control port is published to the host network or the internet.

### 8.10 Hour boundary

- `request.push` does not guarantee a start at a specific second. Clock starts are therefore tied to the hour grid: Liquidsoap switches sources by time, aligned to UTC hour boundaries adjusted for the station timezone (§10.2).
- Each clock start is a scheduled switch. Items from the new clock are pushed before the boundary, so the switch is a source change, not a late push.
- The script switches between its two hour queues at `floor((t − hour_shift) / 3600)` parity, where `hour_shift` (set by the backend) is the station UTC offset modulo one hour. The queue an hour leaves is flushed, and a track cut by the switch reports its end at once (v7-12). Measured on the dev stack: the clock started 18 ms after the boundary.
- The start and end accuracy are verified on the test stand before the first air date. Acceptance target: a clock starts within one second of the scheduled time.
- If the station timezone ever becomes DST-dependent, the switch logic must handle 23- and 25-hour days (§10.2).

## 9. Docker and Environments

Services in a single `docker-compose.yml` (kept in `radio-atlantida-deploy`):

- `backend`: Go backend (image `radio-atlantida-backend`, built from `radio-atlantida-backend`).
- `db`: PostgreSQL with a named volume for database data.
- `liquidsoap`: Liquidsoap image (Linux), media directory mounted read-only, and a bind mount of `PLAYOUT_JOURNAL_ROOT` for the playout journal (see §7.1).
- `icecast`: test stream server with default configuration.
- `frontend`: Angular build served by a static server (image `radio-atlantida-frontend`, built from `radio-atlantida-frontend`).
- `proxy` (optional): reverse proxy for the panel and API.
- `grafana` (optional, `monitoring` switch): a live view of the database (§10.3) and log search (§10.4) for the operator. With it, `grafana-role`: a one-shot job that keeps the read-only database role of Grafana in step with `.env` and exits; `loki`: log storage; `alloy`: the collector that reads the log and journal files into Loki.

Guidelines:

- Each service has its own image; no secrets are baked into images.
- Configuration comes from environment variables and `.env` files that are not committed, limited to the database connection, secrets, and host paths. All other settings are edited in the admin panel (see §6.3).
- Media files are never inside a container image or a named volume (see §8.7).
- Health checks are defined for `db`, `backend`, `liquidsoap`, `grafana`, `loki`, and `alloy`.
- The log directory (`LOG_ROOT`) is a host bind mount writable by `backend`. Logs are never kept only inside a container (see §10.1).
- The playout journal directory (`PLAYOUT_JOURNAL_ROOT`) is on the host. `liquidsoap` writes to it read-write, and `backend` mounts the same directory read-only for reconciliation (§7.1). It survives container recreation and is included in the backup set (§10).
- Services communicate over an internal Docker network. Only the panel port, the Icecast listener port, and the Grafana port are published to the host. The Grafana port is always bound to `127.0.0.1` (§10.3); `loki` and `alloy` are not published.

### 9.1 Development and runtime platforms

- **Development**: macOS. Docker Desktop runs the Linux containers.
- **Runtime**: Windows 10 or 11 PC on SSD storage, running Docker Desktop. The Docker engine inside is Linux (WSL2 backend), so all containers are Linux containers.
- Because development is on macOS and runtime is on Windows, the `MEDIA_ROOT`, `LOG_ROOT`, and `PLAYOUT_JOURNAL_ROOT` paths are set per machine in `.env`. Paths in the database stay relative (§8.7).
- Bind-mounted files from the Windows filesystem are accessed through the WSL2 layer. Read performance is adequate for playout.
- File paths and line endings are checked in the build pipeline because they differ between macOS and Windows.

### 9.2 Deployment and Updates

The production machine runs the stack from the **radio-atlantida-deploy** repository. The application code is not on the machine. The machine only pulls prebuilt images from the registry and runs them.

**Repositories and images**

- `radio-atlantida-backend` (Go) and `radio-atlantida-frontend` (Angular) are built by CI (GitHub Actions) on tagged releases and pushed to GitHub Container Registry as `ghcr.io/astronin/radio-atlantida-backend:<tag>` and `ghcr.io/astronin/radio-atlantida-frontend:<tag>`.
- Both images are built from the same tag, so a release is one version number for the whole system. The OpenAPI specification and the generated TypeScript client are part of that same tag (§6.4).
- Images are private. The production machine logs in with a read-only token (`read:packages`) stored in `.env`.
- **Implemented in Phase 13 (`docs/phase-13-plan.md`).** The Git tag is `vX.Y.Z`; the image tag and the version are `X.Y.Z` (no `latest`, never overwritten). The release workflow runs the full CI, builds `linux/amd64`, and attaches the `openapi.json` produced by the shipped image to the GitHub release `vX.Y.Z`; the frontend copies it into its snapshot. `backend version` prints the version.

**Deploy repository contents (`radio-atlantida-deploy`)**

- `docker-compose.yml`: services `backend`, `migrate`, `db`, `liquidsoap`, `icecast`, `frontend`, and the monitoring services `grafana-role`, `grafana`, `loki`, and `alloy` (§10.3, §10.4).
- `.env.example`: the template for the machine's `.env`. The real `.env` is not committed.
- `IMAGE_TAG`: the release tag in use, for example `1.4.2`.
- `update.bat`: the entry point the manager runs on double-click or from a terminal.
- `update.ps1`: the update logic in PowerShell, called by `update.bat`.
- `config/`: Liquidsoap script, Icecast configuration, Grafana provisioning (role script, data sources, dashboards; §10.3, §10.4), and the Loki and Alloy configuration (§10.4).
- `backups/`: database dumps, not committed.
- `logs/`: update run logs, not committed.
- `README.md`: a short description of the steps above.

The Compose file references images by tag, for example `image: ghcr.io/astronin/radio-atlantida-backend:${IMAGE_TAG}`.

**Migrations**

- The `radio-atlantida-backend` image contains the migration files (from `radio-atlantida-backend`) and the `migrate` subcommand (golang-migrate library). The same image is used for both the server and the migration.
- The Compose service `migrate` runs `backend migrate up` once and exits. The `backend` service starts only after it completes successfully (`depends_on: condition: service_completed_successfully`).
- Migrations use the expand and contract approach: add new columns or tables first, switch the code, and remove old columns in a later release. This keeps the previous version of the API working against the new schema, so a code rollback does not break the database.
- The v7-6 changes (`broadcast_outages`, the `missed` status, the new `play_log` columns) are added as a single expand migration. The previous code ignores the new columns and the new status values, so it still works after rollback.
- The v7-7 changes (the rename of categories to playlists, `playlist_types`, `clock_element_playlists`) follow the same rule. The expand migration creates `playlist_types`, `playlists`, `track_playlists`, and `clock_element_playlists`, seeds the four system types, and copies the data from the old category tables (`categories`, `track_categories`, and `clock_elements.category_id`, which becomes one `clock_element_playlists` row per element). The old tables and columns are left in place and are removed by a contract migration in a later release. Until then, the previous code keeps working against the old tables.
- The v7-8 changes (`play_rules`, `track_play_rules`, `playlist_play_rules`) use the same approach. The expand migration creates the new tables and copies each row of `daypart_rules` into a `play_rules` row (time window and weekdays) with a matching target row (a track or a playlist; the old `jingle` target is copied as a track). `daypart_rules` stays in place until a contract migration in a later release.
- The v7-9 changes (`clocks.default_filler_mode`, `default_filler_playlist_id`, `default_filler_artist_id`, and the typed filler columns of `clock_elements`) are added as an expand migration with nullable columns. The previous code ignores them.

**Forward-only migrations (no rollback)**

- Migrations are forward-only. Each migration has only an `.up.sql` file. No `.down.sql` files are written, and no command or script runs `migrate down`.
- Rolling back schema changes in PostgreSQL is unreliable: data may already have been transformed, objects dropped, or a migration partly applied. The project does not rely on down migrations.
- If a migration fails, the recovery procedure is:
  1. Restore the database from the dump made for this update, seconds before the migration ran (§9.2, step 7), using `update.bat restore`. This also clears a "dirty" migration state, so no down migration is needed.
  2. Restore the previous code tag (`update.bat rollback`).
  3. Fix the problem, write a new forward migration, release it, and apply it through the normal update.
- Because the dump is taken right before `migrate`, the data lost by a restore is only what was written between the dump and the restore. The restore decision is manual.
- Destructive migrations (dropping data) are reviewed before release and are never shipped in the same release as the code that stops using the data.

**The update script (`update.bat` → `update.ps1`, in `radio-atlantida-deploy`)**

Usage:

- `update.bat`: update to the tag in `IMAGE_TAG` (or the latest tag from the deploy repository, if set).
- `update.bat 1.4.3`: update to a specific tag.
- `update.bat rollback`: return to the previous tag recorded before the last update.
- `update.bat restore`: restore the database from the dump recorded for the last update (`pg_restore --clean`). The script asks for confirmation and shows the dump file name and its UTC time first.

Steps, in order:

1. **Preflight**: check that Docker Desktop is running and that the Docker engine responds. If not, stop with a message. The script does not install or upgrade Docker Desktop, because that installer is manual. When monitoring is enabled in `.env` (§10.3), its passwords must be set; otherwise the script stops before any change and names the missing variable.
2. **Live check**: query the API for the live status. If a broadcast is on air, the script asks for confirmation before continuing. If an outage is open (§7.1), the script stops and reports it.
3. **Fetch the deploy repository**: `git pull` (or a zip download if Git is not installed). Show what changed.
4. **Record the current tag** in `state.json` so rollback knows where to return.
5. **Login to the registry** with the token from `.env` (`docker login ghcr.io`), if not already logged in.
6. **Pull the images**: `docker compose pull`.
7. **Backup the database (mandatory)**: `docker compose exec db pg_dump -Fc` into `backups/db-YYYY-MM-DDTHHMMSSZ.dump`. The file name uses the UTC time (§10.2). A dump is made on every update, including updates without migrations, and the script has no flag to skip it. If the dump fails, the update stops. The dump path is saved in `state.json` for this run, so a restore uses exactly this dump. The script keeps the last 14 dumps and deletes older ones.
8. **Migrate**: `docker compose run --rm migrate`. If it fails, the script stops, does not start the new `backend`, and reports the error.
9. **Restart services**: `docker compose up -d`. Compose recreates only containers whose image or configuration changed, so `liquidsoap` and `icecast` keep running if their images did not change. When monitoring is enabled, the monitoring services are started after that in a separate call (§10.3).
10. **Health check**: wait for the healthchecks of `db`, `backend`, and `liquidsoap` to pass, with a timeout. Grafana is not part of this check: a failure of the monitoring services is reported as a warning and never triggers step 11, because monitoring does not affect the air.
11. **On failure**: set `IMAGE_TAG` back to the recorded previous tag, run `docker compose up -d`, and report the error. Migrations are not rolled back, and the script never runs down migrations. Because of the expand and contract rule, the previous code still works with the new schema. If the migration itself failed, the database is restored with `update.bat restore`, using the dump recorded for this run (see "Forward-only migrations" above).
12. **Log the run**: write the steps, the tags, and the result to `logs/update-YYYY-MM-DDTHHMMSSZ.log` (UTC).

**Effect on air**

- Restarting `backend` interrupts the WebRTC voice session and the WebSocket connection of the panel. It does not stop playout, because Liquidsoap keeps the planned queue (§12). The playout journal records what aired during the restart, and reconciliation runs afterwards (§7.1). The restart is recorded as an outage with `component = backend` if the stream is affected.
- Restarting `liquidsoap` or `icecast` interrupts the stream. The script restarts them only when their images or configuration changed, and the live check in step 2 warns before that. Such a restart is detected as an outage, and the items planned during it are marked missed unless the journal confirms them.

## 10. Operational Requirements

- Access control: none in the panel by design (single operator). The API and control ports must therefore be reachable only from localhost or a trusted private network. Firewall rules on the Windows host enforce this.
- Time synchronization via NTP on the host, because clock start times depend on it.
- Time and timezones: all stored times are UTC; the panel displays them in the selected timezone. See §10.2.
- Fallback content available if the engine or database is unavailable.
- Logging: structured logs in the backend; Liquidsoap logs to a file or stdout.
- Monitoring: an optional Grafana shows the live state of the database (§10.3) and searches the application logs and the playout journal (§10.4).
- Backups: daily PostgreSQL dumps, a copy of the media directory kept outside the machine that runs the stack (the media directory itself is not duplicated inside Docker), and a copy of the playout journal directory with the database dumps.

### 10.1 Logging and diagnostics

**Format and library**

- Logs are written as JSON Lines: one JSON object per line. The panel and tools like `jq` can read them directly.
- Library: `log/slog` from the Go standard library, with its JSON handler. It needs no extra dependency and is fast enough for this volume. `zap` or `zerolog` are alternatives if profiling shows a need.
- Each entry contains: `time` (RFC 3339 with offset), `level`, `msg`, `component`, and error details as fields. `component` is one of `api`, `db`, `rotation`, `filler`, `liquidsoap`, `icecast`, `webrtc`, `retention`, `outage`, `reconcile`, `media` (import, analysis, transcoding; v7-14). API errors also carry `request_id`.
- Secrets (`ICECAST_SOURCE_PASSWORD`, `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD`) are never logged. A handler masks any field with a sensitive name.

**What is logged**

- ERROR and WARN by default; INFO for connection state changes.
- **Icecast (remote server):** connection failed, lost, or re-established; authentication rejected; mount point busy; reconnect attempts.
- **Liquidsoap control:** connection refused, timeout, unexpected reply; playback callback errors.
- **Outages:** outage opened and closed (with component and duration), number of items marked missed, reschedule and dismiss actions on missed items.
- **Reconciliation:** start and end of each journal reconciliation, number of items confirmed, number of journal lines that could not be parsed.
- **Application:** recovered panics (caught by middleware), failed HTTP handlers (5xx), rotation failures (no candidate found, tolerance limit reached, element skipped because of play rules), filler failures (pool empty, pool track refused by rules, reminder expired).
- **Database:** failed writes (for example, `play_log` insert), transaction rollbacks, connection pool errors, failed migrations.
- **WebRTC bridge:** session setup failures, ICE errors, live voice dropouts.

**Error alerts in the panel**

- Errors are also pushed to the panel over WebSocket (see §6).
- The panel shows each error as a popup window with the error text, so the operator sees problems without opening the logs.
- An open outage is shown as a persistent banner until it closes, not as a popup that can be missed.
- Alerts are shown with their UTC timestamp converted to the selected timezone.

**Rotation**

- One file per day: `app-YYYY-MM-DD.log` in a host directory set by `LOG_ROOT`. The date is the UTC date. Timestamps inside entries are UTC as well.
- Cleanup of files older than 10 days is also computed in UTC.
- A small daily rotator (an `io.Writer` in the backend) switches to a new file at midnight. On each rotation it deletes files older than 10 days.
- Alternative for size limits: `gopkg.in/natefinch/lumberjack.v2`, which supports size-based rotation, maximum age, and backup count. It does not rotate by day by default, so it is a fallback if a single day's file grows too large.
- Logging must never block playout. Entries are sent through a buffered channel. If the buffer is full, entries are dropped, the drop count is incremented, and a warning is logged once the buffer drains. A log write failure never stops the stream.

**Access from the panel**

- The Logs section in Angular has a dropdown of available files (one per day, newest first, showing date and size), loaded from `GET /api/logs/files`.
- `GET /api/logs/files/{name}` returns entries, newest first, with optional filters `level`, `component`, `limit`, `offset`.
- The file name must match `app-YYYY-MM-DD.log`, and the server reads only from `LOG_ROOT`. This prevents path traversal.
- Timestamps are converted to the timezone selected in the panel (default `Asia/Yerevan`, see §10.2).
- The API has no authentication (see §6), so this endpoint is subject to the same network restrictions.

### 10.2 Time and timezones

**Storage**

- Every column that holds an instant uses `timestamptz`. `timestamp without time zone` is not used.
- The PostgreSQL session timezone is set to UTC.
- The Go backend computes and stores all instants in UTC. Conversion happens only at the edges: parsing input and producing output.
- Logs are UTC: timestamps inside entries and the date in file names (`app-YYYY-MM-DD.log`).
- The playout journal uses UTC timestamps for every line.

**Display**

- The API sends times as ISO 8601 UTC strings (ending with `Z`). Angular converts them for display.
- The panel has a timezone setting. Default: `Asia/Yerevan` (UTC+04:00). The setting uses IANA names, not fixed offsets, so the browser's timezone database handles any future offset changes.
- All calendars and time views use the selected timezone: grid, clocks, track plan, play log, played-tracks list, logs, and alert timestamps.

**Station schedule**

- Grid rules (weekday, hour) and play rule time windows are defined in station local time, because "08:00" means 08:00 at the station. The station timezone is a backend setting, default `Asia/Yerevan`.
- The rotation engine resolves these rules to UTC instants when it plans. Plans and the play log store UTC.
- If the station timezone changes, grid rules keep their local meaning, and plans already generated keep their UTC times.
- If the station timezone ever observes daylight saving time, the planner must handle days with 23 or 25 hours explicitly, so a clock does not start an hour off. Armenia currently does not use DST, so this is a future-proofing requirement only.

### 10.3 Database monitoring (Grafana)

**Purpose and scope**

- Grafana gives the operator a live view of PostgreSQL: connections, running queries, locks, database and table sizes. It answers "what is the database doing now", not "what did it do last night".
- There is no metrics history: no Prometheus, no exporter, no `/metrics` endpoint in the backend. The backend code does not change for monitoring.
- A history of database metrics can be added later with `postgres_exporter` and Prometheus, without backend changes. Log search in the same Grafana is §10.4.

**Service**

- `grafana`: the official open source image, pinned to an exact tag like every infrastructure image. One named volume for Grafana's own state; it holds nothing that cannot be rebuilt from the provisioning files.
- Published only on `127.0.0.1` (port from `.env`, default 3000), like the panel (deploy plan §8.1). The address is not configurable, so Grafana cannot be exposed to the LAN by mistake.
- Monitoring is optional and enabled in `.env` (`MONITORING_ENABLED`), so it can be switched off to save memory on the production machine. In the dev stack it is the compose profile `monitoring`.
- The machine may be offline: update checks, usage reporting, and the news feed are switched off.
- No other service depends on `grafana`. Its failure never affects playout, the backend, or an update (§9.2, step 10).

**Access**

- Grafana requires a login, unlike the panel (§10): it is a separate tool with its own users and its own configuration. Anonymous access and sign-up are off.
- One admin user. Its password is `GRAFANA_ADMIN_PASSWORD` in `.env`. Grafana applies it only when its state volume is created; a later change is made with `grafana cli admin reset-admin-password` (runbook).

**Database access**

- Grafana connects as its own role, `grafana_ro`, never as the application user. The role has `LOGIN`, membership in `pg_monitor` (read access to the statistics views and to other sessions' queries in `pg_stat_activity`), no access to application tables, `statement_timeout` 5 s, and a connection limit of 3.
- The role is created and updated by the one-shot service `grafana-role` (the PostgreSQL image running `psql` with an idempotent SQL script) every time the monitoring services start. The password comes from `GRAFANA_DB_PASSWORD` in `.env`, so changing it in `.env` and restarting is enough.
- The role is not created by a migration: migrations seed only system rows and must not carry a password, and the role exists only where monitoring is enabled. The job connects as `POSTGRES_USER`, which the official image creates as a superuser, so it may create the role and grant `pg_monitor`.
- `pg_stat_statements` (statistics per query) is not enabled: it needs `shared_preload_libraries` and a restart of `db`. It is a later step if the live view is not enough.

**Configuration as files**

- The data source (PostgreSQL, fixed uid) and the dashboard are provisioned from files mounted read-only. The password reaches the data source through an environment variable, never through a file in git.
- A provisioned dashboard cannot be saved from the UI. A change is exported as JSON and committed. The production files live in `radio-atlantida-deploy` (`config/grafana/`), the dev copies in `radio-atlantida-backend` (`dev/grafana/`); they are kept in step, like the Liquidsoap script (§8.3).
- Dashboard queries read only the PostgreSQL statistics views and catalog functions. Sessions are grouped by `application_name` (the backend sets `radio-atlantida-backend`, migrations `radio-atlantida-backend-migrate`).

### 10.4 Log search (Loki)

**Purpose and scope**

- The same Grafana (§10.3) searches and filters the application logs (§10.1) and the playout journal (§7.1), and shows how many errors each component logged over time. It complements the Logs section of the panel; it does not replace it.
- No alerts in this version. Grafana alert rules on log queries can be added later without backend changes.
- The backend code does not change: the log format of §10.1 (JSON Lines, UTC `time`, `level`, `component`) is read as it is.

**Services**

- `loki`: log storage, single process, filesystem storage in a named volume. Not published; Grafana reaches it on the internal network. No multi-tenancy (`auth_enabled: false`), since only Grafana and Alloy can reach it.
- `alloy` (Grafana Alloy): the collector. It reads `LOG_ROOT` and `PLAYOUT_JOURNAL_ROOT` read-only and sends every new line to Loki. Its read positions live in a named volume, so a restart neither loses nor repeats lines. Not published.
- Both are pinned images, part of the monitoring switch of §10.3 (one switch for all monitoring services), and nothing else depends on them.

**What is collected**

- Application logs: the files `app-YYYY-MM-DD.log` in `LOG_ROOT`, not the container's stdout, so each entry is collected once and survives container recreation. Labels: `job="backend"`, `level`, `component`. `request_id`, `err`, and other fields stay in the line and are filtered in queries, not as labels, so the label set stays small.
- Playout journal: the files `journal-YYYY-MM-DD.jsonl` in `PLAYOUT_JOURNAL_ROOT`. Labels: `job="playout-journal"`, `event` (`start`, `end`, `output`), `source`. The line time is the entry's `time` field, for both sources.
- Not collected: stdout of `liquidsoap`, `icecast`, `db`, and the other containers (Docker `json-file` logs, deploy plan D2). Collecting them needs access to the Docker socket and is a later step.
- The files on the host stay the source of truth (§10.1, §7.1). Loki is a searchable copy.

**Retention and files**

- Loki keeps 10 days, like the log files (`retention.log_days` default) and the journal. Lines up to 10 days old are accepted, so a first start or a long stop of the collector loads the files still on disk.
- The files are on the Windows file system through the WSL2 layer, where file change notifications are not reliable. Alloy polls the files and the folders, so new lines, the daily file switch, and the deletion of old files are seen without notifications.
- Loki's and Alloy's volumes are not backed up: both can be rebuilt from the files on the host while they exist.

**Configuration as files**

- The Loki data source (fixed uid) and a "Logs" dashboard are provisioned like the database ones (§10.3). The Loki and Alloy configuration files are mounted read-only. The production files live in `radio-atlantida-deploy`, the dev copies in `radio-atlantida-backend` (`dev/grafana/`, `dev/loki/`, `dev/alloy/`), kept in step.

## 11. Resolved Decisions

The open questions from the previous version are closed. The decisions are recorded here so the reasoning stays visible.

1. **Frontend and backend contract.** One contract for the whole system: an OpenAPI specification generated from the Go backend, which also covers the WebSocket event schemas. The Angular TypeScript client is generated from it (§6.4). No separate frontend API document.
2. **Separation rules.** Values are stored in the `separation_rules` table and copied from the reference software. Rotation reads them from the table, so no values are hard-coded (§4, §7).
3. **Play log volume.** No partitioning. A nightly `DELETE` of aired rows older than 10 days is sufficient for the expected volume, which PostgreSQL handles easily (§4.1). Open missed items are exempt from this deletion.
4. **Live voice latency budget.** Target of at most 1 second from microphone to air. The system runs locally on one machine with local Docker (§8.4).
5. **Production Icecast details.** Entered in the admin panel and stored in the database, like all other output settings. Nothing is set in code or files (§8.8, §6.3).
6. **Windows host.** Windows 10 or 11 with Docker Desktop (WSL2 backend), SSD storage. No separate performance test is required before going live (§9.1).
7. **Host paths.** `MEDIA_ROOT`, `LOG_ROOT`, and `PLAYOUT_JOURNAL_ROOT` are set once in the Compose `.env` file and never change. They are not editable in the UI (§6.3, §3.1).
8. **Viper.** Removed. Runtime configuration lives in the admin settings, and no YAML profile is used (§3.1).
9. **Source repositories.** Three repositories: `radio-atlantida-backend`, `radio-atlantida-frontend`, `radio-atlantida-deploy`. Their roles and release coupling are described in §1.1.
10. **Missed items are not automatically re-aired.** A missed item is never pushed back into the plan by the system. Only the manager decides whether to reschedule or dismiss it (§7.1). Reason: an automatic repeat could place content in a wrong context, for example a news segment after its time.
11. **Missed items do not count as aired.** Separation and play rules use only aired and truncated items. A track whose broadcast failed can be played again in the normal rotation (§7).
12. **Outage truth comes from the playout journal.** Liquidsoap's local journal, not the backend's memory, decides what aired. This keeps the played-tracks list accurate when the backend is down (§7.1, §8.3).
13. **Fixed-time elements.** An element can be placed at the start of the hour, at its end, or at a fixed minute with a default tolerance of ±1 minute on its start (§7.2).
14. **Overflow keeps the crossing track.** When a track runs past a segment end, that track is kept and fades out at the boundary, and the tracks after it are removed from the plan. Filler is not used for overflow (§7, §7.2).
15. **Replacing a fixed element.** A new fixed element whose window overlaps an existing one replaces it, and the clock is rebuilt once (§7.2).
16. **"Category" is renamed to "playlist" everywhere.** The word is used in the database, the API, the code, and the interface. The term "category" caused confusion with another meaning the manager has for it. A playlist is a pool: the order of tracks inside it is not significant, and the rotation engine chooses the track (§4, §7).
17. **A clock element can use several playlists.** The tracks of all its playlists form one candidate set (union, without duplicates), and every candidate has the same weight. The playlists may be of different types, so one element can mix, for example, music and jingle. Weighting by playlist and a selection order (for example round robin) are not part of this version (§4, §7).
18. **Playlist types are dynamic.** Types are rows in `playlist_types`, not a fixed enum. The manager can add types and assign them to playlists. The four system types (`music`, `jingle`, `news`, `segment`) cannot be deleted, and a type in use cannot be deleted either (§4, §6.3, §6.4). The track type (`tracks.type_id`) uses the same table. A type is only a label for the manager and does not limit what a clock element can combine.
19. **Play rules replace dayparting.** Time windows, minimum intervals, and plays per hour are one kind of reusable rule. The manager creates rules on a separate page and applies them to tracks or playlists (§4, §7.3). The `daypart_rules` table is superseded.
20. **Rules limit, the clock demands.** A play rule only decides whether a track is allowed at a given moment. How often a jingle appears is set by the clock structure. Play rules are never relaxed. If every candidate of an element is blocked, the element is skipped with a warning (§7.3).
21. **Track preview in the panel.** The backend streams a track by ID with Range support. Playback happens in the browser only and never reaches the air or the play log (§6.6).
22. **References to plan rows.** Missed items live in `play_log`, so `scheduled_items.reschedule_of_id` references `play_log`. `play_log.scheduled_item_id` and `resolved_item_id` are trace ids without a foreign key, because `scheduled_items` rows are deleted after they air (§4, §4.1).
23. **History blocks deletes.** Tracks, artists, and clocks referenced by the play log or the track plan cannot be deleted (§4).
24. **Default filler mode belongs to the clock.** The fallback mode for a clock is set in the clock editor and stored on the clock. The global setting in §6.3 is only the fallback for clocks that have none (§4, §6.2, §7).
25. **Journal path is an environment variable.** `PLAYOUT_JOURNAL_ROOT` is a host path set in `.env`, like `MEDIA_ROOT` and `LOG_ROOT`. Liquidsoap writes the journal to it, and the backend reads it read-only (§3.1, §7.1, §9).

26. **Repository layer.** All SQL lives in `internal/repository/<domain>`, one package per domain, named as the domain package. Domain code depends on interfaces it declares itself, and the repositories implement them. Reason: business rules stay free of SQL and driver errors, and a domain rename changes the domain and its repository in the same change (§5.1).
27. **Import and transcoding.** New files arrive through an inbox folder under the media root and are moved into the library folder on import; there is no upload through the panel. Analysis flags problems against the broadcast standard set in the admin settings, and nothing changes audio without the manager: the panel offers "Fix loudness" and "Optimize track", and the manager chooses whether the fixed file replaces the original or is kept as an optimal copy next to it (§8.7, decided 2026-10-14).
28. **Live voice without transcoding.** The bridge relays the browser's Opus packets to the harbor as Ogg/Opus, without decoding, using `pion/webrtc` (pure Go, no cgo). Its UDP port and public addresses are environment variables like `HTTP_ADDR`, because they belong to the compose port mapping (§8.4, decided 2026-10-14).
29. **Database monitoring is a live view.** An optional Grafana reads PostgreSQL's statistics views directly through a read-only role, with a login, bound to localhost. No metrics history, no Prometheus, and no backend changes; a metrics history can be added later without backend changes (§10.3, decided 2026-10-15).
30. **Log search in Grafana.** Loki stores the application logs and the playout journal, collected by Grafana Alloy from the files on the host (not from container stdout), with 10 days of retention like the files. No alerts in this version; the backend does not change (§10.4, decided 2026-10-15).

## 12. Non-functional Requirements

- The clock grid covers up to 30 days ahead. The track plan is generated automatically and covers a rolling window of 24 to 48 hours.
- A failure in the management panel or API must not stop the air. Playout continues from the planned queue.
- Rotation generation for one hour completes within a few seconds for a library of tens of thousands of tracks.
- The play log holds at most 10 days of aired history. Open missed items are kept until resolved.
- Application logs are JSON, one file per day, and are kept for 10 days (see §10.1). Logging never blocks playout.
- All changes that affect air are recorded with a UTC timestamp, including outages and missed-item decisions.
- All stored times are UTC; the panel displays them in the selected timezone (default `Asia/Yerevan`, UTC+04:00).
- Clock boundaries are exact: a clock starts within one second of its scheduled time (§8.10).
- Live voice reaches air within 1 second of the microphone input (§8.4).
- A fade-out on a truncated track ends at or before the hour boundary (§8.5, §7).
- An interruption of the stream is detected within 10 seconds of its start under the default settings (§6.3, §7.1).
- After the stream recovers, missed items are marked and shown in the played-tracks list within 60 seconds (§7.1).
- The playout journal records every track start and end, so the played-tracks list is complete after any single component restart (§7.1, §8.3).
