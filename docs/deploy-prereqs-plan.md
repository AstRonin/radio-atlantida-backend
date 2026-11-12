# Plan — Deploy Prerequisites: Runtime Stream Output, TLS, Migration Recovery and Downgrade Guard, Multi-arch Release (radio-atlantida-backend)

Status: Approved scope (deploy plan and tech-design v7-24, 2026-11-12); not started
Based on: `docs/tech-design.md` v7-24 (§1.1, §6.3, §7.1, §8.1, §8.5, §8.8, §9, §9.2, §11 items 32–38), `radio-atlantida-docs/deploy/docs/deploy-init-plan.md` (§5 Phase D3, §6 Phase D4, §7.3, §10, §12 cross-repo tasks B1–B10), frontend `docs/deploy-prereqs-plan.md` (F1–F4)
Last updated: 2026-11-12

## 1. Goal

The deploy repository (`radio-atlantida-deploy`) can only finish its phases D3 and D4 when the backend:

- applies the `stream.*` settings to Liquidsoap at runtime, without a container restart, and streams over TLS to the station's external Icecast server (`https://listen.teploe.net:8001/atlantida`, TLS only);
- carries a contract version header in its Liquidsoap script, which the deploy repository copies;
- records which oldest release can run against the schema, so the deploy tool can install older releases safely;
- undoes a failed migration by itself, so the deploy tool can keep the running release on air without asking the operator anything;
- publishes its image for `linux/amd64` (production) and `linux/arm64` (the developer's Mac) under one tag.

This plan covers the deploy plan's cross-repo tasks B1, B2, B3, B4 (resolved as a `CLAUDE.md` update to 1.27.4, step D1), B5, B6, B7 (required: automatic recovery of a failed migration), B8 (decided), B9, and B10. B11 is not planned (§7).

**Precondition:** the uncommitted working-tree change (`docker/Dockerfile` `GO_VERSION=1.27.4`, `go.mod`) is committed first; it is not part of this plan.

## 2. Decisions (2026-11-12, from the deploy plan)

1. **A reconnect is not a restart** (design §6.3, §8.8). A saved `stream.*` value is sent to Liquidsoap over the existing control link; the script replaces its `output.icecast` and leaves the sources, queues, fallback, and journal alone.
2. **One telnet command, JSON argument.** `output.set <json>` on the control port. The script parses it with `json.parse`, so no setting value ever becomes script code. The source password stays in Liquidsoap's environment and is never sent.
3. **Idempotent.** An `output.set` equal to the running output answers `OK unchanged` and does nothing, so the backend can send it after every reconnect without interrupting the stream.
4. **Shut down, then create.** On the same mount Icecast refuses the new source (403) until the old one has gone; the script shuts the old output down first and retries the new one after 1 s.
5. **Startup without the backend.** Liquidsoap reads its first output from `GET /api/playout/output`, falling back to the last applied config in a state file, then to its environment (`ICECAST_HOST`, ...), then to defaults equal to the settings defaults. The backend's push corrects it as soon as the control link connects.
6. **TLS is a setting**, `stream.icecast_tls` (bool, default `false` so the dev stack's Icecast keeps working; reconnect; affects air). It selects `transport=http.transport.ssl()` in the script and `https` in the outage detector's mount check.
7. **`--check` is not enough.** `liquidsoap --check` accepts a script that fails at runtime (a non-literal `%mp3(stereo=…)`), so CI runs the script against a test Icecast.
8. **B5, minimal option.** Nothing new is implemented. `stream.loudness_target_lufs` stays: the import and Fix loudness use it (`cmd/backend/serve.go:622`); its description says it is not applied to the output. `stream.hls_enabled` stays as a reserved setting whose description says "not applied yet". No code reads either for the output.
9. **`schema_compat` ships in 1.0.0** (the baseline), so every release from 1.0.0 has the table; a missing table still counts as `1.0.0` for safety.
10. **A failed migration never leaves the operator a decision** (B7, required). A failed transactional migration is rolled back by PostgreSQL; `backend migrate up` resets golang-migrate's version so the database is clean, and exits with code 4. A database left dirty exits with code 5, and the deploy tool restores the pre-update dump by itself (deploy plan §7.3 step 7).
11. **No release grouping.** Each migration file is its own unit. Files applied earlier in the same run stay applied: they are expand-only, so the running release works with them. Applying one release's files as one unit would need one transaction across files (golang-migrate commits per file, and a non-transactional file cannot join a transaction) or undoing committed files (which forward-only migrations do not allow); both are worse than "the schema is at version N, compatible with the running release".
12. **B8 decided:** annotated release tags with a line `migrations: none|expand|contract`.
13. **Multi-arch by native runners** (`ubuntu-24.04` and `ubuntu-24.04-arm`; the repository is public, so both are free): the runtime stage runs `apt-get install ffmpeg`, which would need QEMU on one runner. One manifest list per tag; the deploy repository pins its digest.

### 2.1 Verified with `savonet/liquidsoap:v2.4.6` (2026-11-11/12)

Throwaway containers, test scripts kept in the session scratchpad (`liqtest/rt.liq`, `tlsout.liq`, `cs.liq`):

- An `output.icecast` created from a `server.register` handler starts and connects; `shutdown()` on the old one closes it; `source.time(radio)` keeps counting through the switch.
- Changed at runtime on one source: mp3 128/44100/stereo → mp3 192/48000/mono (new mount) → `%vorbis.cbr` 128 → `%fdkaac` 128 → mp3 320 on the same mount. Icecast reported every format. Gap on the same mount: about 4 s (first connect 403, retry after the default 3 s).
- `bitrate` and `samplerate` may be runtime values; `%mp3(stereo=…)` must be the literal `true` or `false` (runtime error otherwise, not caught by `--check`); a mono encoder needs a mono source (`mean(s)`).
- Output ids repeat on every switch (`out_mp3`, `out_mp3.1`, ...): create outputs with `register_telnet=false`.
- `transport=http.transport.ssl()` (and `http.transport.tls()`) pass `--check`; a source connection through a TLS proxy to the test Icecast connected and streamed; `http.get` over HTTPS to the production status page returned 200 (the Let's Encrypt chain is trusted in the image).
- `on_error` is a source method in 2.4.6 (`out.on_error(synchronous=…, fun (restart_in, err) -> restart_in(1.))`); the argument form is deprecated.
- Metadata charset: Liquidsoap already sends `charset=UTF-8`; Icecast 2.4.4 converts to the mount's charset (ISO-8859-1 by default). No script change; the server owner sets the mount charset (design §8.8).

### 2.2 Verified: one migration file is one transaction (2026-11-12)

- `internal/migrations/migrations.go:54` uses golang-migrate's `pgx/v5` driver with `pgx.Config{}`: `MultiStatementEnabled` is false, so `Run` reads the whole file and executes it with one `ExecContext` (`database/pgx/v5/pgx.go`, `Run` and `runStatement`, v4.20.1). pgx v5 sends a query without arguments with the simple protocol ("Always use simple protocol when there are no arguments", `conn.go:524-527`, v5.11.0). PostgreSQL runs a multi-statement simple query in one implicit transaction.
- The migration connection sets `lock_timeout` 10 s and no `statement_timeout` (`internal/db/db.go:35`, `:99-108`).
- Checked on `postgres:17.11`:
  - an error in the third statement of a file: nothing of the file remains;
  - a lock timeout (`ALTER TABLE` on a table locked by another session, `lock_timeout` 1 s) after a `CREATE TABLE` in the same file: the table is not created;
  - `CREATE INDEX CONCURRENTLY` after another statement in the same file: refused at once ("cannot run inside a transaction block"), nothing applied; alone in a file it runs;
  - explicit `BEGIN; …; COMMIT;` followed by a failing statement: the part before `COMMIT` stays (a partial file). Hence the lint of B7.
- Checked with golang-migrate v4.20.1 and the pgx v5 driver (a throwaway program): file 2 = `CREATE TABLE t2; ALTER TABLE t1 ADD COLUMN y; SELECT 1/0;` → `Up` fails, version 2 **dirty**, `t2` and `t1.y` absent; `Force(1)` → version 1, not dirty.
- Found while checking: golang-migrate's `database.Error` has no `Unwrap`, so `errors.As(err, &*pgconn.PgError)` does not reach the server error; the runner must `errors.As(err, &database.Error{})` and type-assert `OrigErr`. Its message also contains the whole migration SQL, which the log line should shorten.
- Found while checking: a contract migration in a run that skips releases (running release below the new `min_app_version`) would break the running backend even though the migration succeeded; B7 adds `backend migrate target-compat` so the deploy tool can stop the old backend first in that case.

## 3. Phases and steps

Steps are in dependency order. **D3** / **D4** marks the deploy phase a step blocks.

### Phase A — Runtime stream output and TLS (blocks deploy D3; A1 and A2 also block D4)

**A1. Setting `stream.icecast_tls` (B6, blocks D3 and D4).**
- `internal/settings/definitions.go`: key `StreamIcecastTLS = Key[bool]{"stream.icecast_tls"}` next to `StreamIcecastPort` (`:104`); `r.add(boolDef(StreamIcecastTLS, "Connect to the Icecast server over TLS (HTTPS).", false), ApplyReconnect, true)` after `:189`.
- `internal/settings/settings_test.go` `TestDefaultsMatchDevStack` (`:88`): the dev stack's Icecast is plain, so the default `false` matches; add the key to the test's list if it enumerates the stream group.
- `make openapi` (the settings list is data, but enum and schema docs may change).

**A2. Mount check over HTTPS (B6, blocks D3 and D4).**
- `internal/outage/icecast.go`: `IcecastTarget` (`:28-32`) gains `TLS bool`; `Check` (`:52`) uses `Scheme: "https"` when set. The checker's `*http.Client` already verifies certificates with the system roots (`ca-certificates` is in the image, `docker/Dockerfile:32`).
- `cmd/backend/serve.go` `outageSettings` (`:556-598`): read `StreamIcecastTLS` and pass it in `outage.IcecastTarget` (`:591`).
- Tests: `internal/outage/icecast_test.go` (or the existing checker test): an `httptest.NewTLSServer` serving a status document with the mount, checked with `TLS: true` and the server's client; plain `TLS: false` unchanged; `listenurl` with another port (production advertises `:8100`) still matches by path (`hasMount`, `:109-113`).

**A3. Endpoint `GET /api/playout/output` (B1, blocks D3).**
- `internal/httpapi/dto/playout.go`: `StreamOutputConfig` with `host`, `port`, `mount`, `tls`, `codec` (enum `mp3,aac,vorbis`), `bitrate_kbps`, `sample_rate_hz`, `channels`, `name`, `description`, `genre`. No secret. (Not `PlayoutOutput`: that name is the output *state*, `:31-34`.)
- `internal/playout`: `Settings` (`playout.go:209-227`) gains `Output StreamOutput` (the same fields, a plain struct with a stable JSON encoding, method `Command() string` returning `output.set <compact JSON>`); `cmd/backend/serve.go` `playoutSettings` (`:314`) reads the `stream.*` keys.
- `internal/httpapi/handlers/playout.go`: operation `get-playout-output`, `GET /api/playout/output`, next to `get-playout-status` (`:93-95`); returns the resolved values (saved or default). Like the callback, it is for Liquidsoap on the internal network; the API has no authentication anyway (design §10).
- `internal/httpapi/router_test.go`: add the path to the route list (`:253`).
- Tests: handler test with the fake playout service (`internal/httpapi/playout_test.go` pattern, `newPlayoutRouter`); `make openapi`.

**A4. Script: `output.set`, startup fetch, codec × channels, TLS (B1, blocks D3).** `dev/liquidsoap/radio.liq`, replacing the fixed output (`:344-356`):
- Keep `radio` (`:342`) as the one source of every output.
- `make_output(cfg)`: one branch per codec and channel count — `%mp3(bitrate=br, samplerate=sr, stereo=true)` / `stereo=false` on `mean(radio)`, `%vorbis.cbr(bitrate=br, samplerate=sr, channels=2)` / `channels=1` on `mean(radio)`, `%fdkaac(bitrate=br, samplerate=sr, channels=2)` / `channels=1` on `mean(radio)`; `transport=http.transport.ssl()` when `tls`, the default transport otherwise; `name`, `description`, `genre`, `host`, `port`, `mount`, `password=icecast_password`, `id="icecast"`, `register_telnet=false`; `out.on_error(synchronous=false, fun (restart_in, _) -> restart_in(1.))`; the existing `on_connect`/`on_disconnect` reports (`:355-356`) attached to every new output.
- `current_output` (a `ref`) and `current_cfg` (the normalised JSON string).
- `server.register("output.set", fun (arg) -> ...)`: `json.parse` into a record with defaults; validate the enums (codec, bitrate 64–320 in the settings set, sample rate 22050/32000/44100/48000, channels 1/2, port 1–65535, mount starting with `/`); equal to `current_cfg` → `"OK unchanged"`; otherwise `current_output().shutdown()`, create, store, write `OUTPUT_STATE_FILE` (`file.write`), → `"OK applied"`; invalid → `"ERROR <reason>"` and the running output stays.
- Startup: `http.get(timeout=2., BACKEND_OUTPUT_URL)` → state file → environment (`ICECAST_HOST`, `ICECAST_PORT`, `ICECAST_MOUNT`, existing `:28-30`, plus `ICECAST_TLS`) → defaults; the chosen source is logged.
- Header comment (`:1-22`): replace the output bullet; note the new contract (A6).
- New environment: `BACKEND_OUTPUT_URL`, `OUTPUT_STATE_FILE` (default `/var/lib/radio/output.json`; a missing directory means no state file, not an error).

**A5. Backend push on save and on reconnect (B1, blocks D3).**
- `internal/playout/pusher.go`: `pusherState` (`:41-50`) gains `output string` (the last applied command); `resync` (`:140-169`) resets it like `shift`/`duck` (`:141`); new `applyOutput(ctx, set)` called in `pass` after `applyDuck` (`:121-123`): if `set.Output.Command()` equals `s.push.output`, nothing; else `s.ctl.Command(ctx, cmd)` (`internal/liquidsoap/client.go:101`); reply `OK applied` → INFO "stream output applied"; `OK unchanged` → nothing; `ERROR …` or a transport error → ERROR log with component `liquidsoap` (becomes a panel alert through `logging.Alert`, `internal/logging/alert.go:46`), `s.push.output` stays unset so the next pass retries, logged at most once per `retryLog`.
- `cmd/backend/serve.go` `newPlayout` (`:251-279`): wake the pusher on every `stream.*` change: `svc.Subscribe(func(c settings.Change) { if strings.HasPrefix(c.Key, "stream.") { p.Wake() } })` (`Subscribe` is what `settings.OnChange` uses, `internal/settings/settings.go:269`).
- Tests (`internal/playout/playout_test.go`, fake control client): first pass sends once; a second pass with equal settings sends nothing; a changed setting sends the new command; a new control generation (resync) sends again; `ERROR` reply logs an error and is retried on the next pass; `OK unchanged` leaves the state applied. `cmd/backend` wiring: a `stream.` change wakes the pusher.

**A6. Contract header (B2, blocks D3).** `dev/liquidsoap/radio.liq` first lines:
```
# radio-atlantida-contract: 1
# min-backend: <the release that ships Phase A>
```
The rule goes into the header comment and into `CLAUDE.md` (Liquidsoap section): bump the contract number whenever the telnet commands, callback payload, journal format, or the emergency/output endpoints change, and set `min-backend` to the release that first implements the new contract. Until the first release, `min-backend: 1.0.0`.

**A7. Dev compose (B1, blocks D3).** `docker-compose.dev.yml` `liquidsoap` (`:56-92`): `BACKEND_OUTPUT_URL: ${BACKEND_OUTPUT_URL:-http://host.docker.internal:8090/api/playout/output}`, `OUTPUT_STATE_FILE: /var/lib/radio/output.json`, `ICECAST_TLS: "false"`, volume `liquidsoap-state:/var/lib/radio` (and in `volumes:` `:257-261`). The header comment (`:11-15`) keeps saying that the settings defaults must match this file.

**A8. Setting descriptions (B5).** `internal/settings/definitions.go`: `StreamLoudnessLUFS` (`:195`) → "Loudness target of the tracks: the import checks and Fix loudness use it; the stream output is not normalised."; `StreamHLSEnabled` (`:199`) → "Reserved: HLS output is not implemented yet; this value has no effect." Apply mode and range unchanged. Frontend help texts follow in F3.

**A9. Runtime smoke test of the script in CI (B3, blocks D3).**
- `dev/liqsmoke/main.go` (a dev tool like `dev/livecheck`): serves a stub backend (`/api/playout/callback` 204, `/api/playout/emergency/next` 204, `/api/playout/output` with a given config), connects to the telnet port, sends `output.set` for every codec × channel count with the lowest and the highest bitrate (64 and 320) and 44100/48000, and after each waits up to 10 s for Icecast's `/status-json.xsl` to list the mount with the expected `server_type` and `audio_info`. Then one switch to TLS through the proxy. Exit code non-zero on any miss.
- `dev/liqsmoke/compose.yml`: `libretime/icecast:2.4.4-alpine`, a TLS proxy (pinned `nginx` alpine image with a `stream` block and a throwaway certificate generated in the job, as in the 2026-11-12 check), `savonet/liquidsoap:v2.4.6` with `dev/liquidsoap/radio.liq` (the dev stack's pins, read from `docker-compose.dev.yml`).
- `.github/workflows/ci.yml`: a job `liquidsoap` running `liquidsoap --check` and then the smoke test.
- `Makefile`: `make liquidsoap-smoke`.

**A10. Dev check (manual, before closing Phase A).** On the dev stack: change codec, bitrate, and mount in the panel while a track plays; the stream follows within a few seconds, the queue and the journal continue; restart the backend: no reconnect (`OK unchanged`); restart Liquidsoap with the backend down: it starts from the state file.

### Phase B — Migration recovery, downgrade guard, release notes (blocks deploy D5's failure handling; must ship in 1.0.0)

**B1. Migration `0022_schema_compat.up.sql` (B9).** `internal/migrations/0022_schema_compat.up.sql`:
```sql
CREATE TABLE schema_compat (
    id smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    min_app_version text NOT NULL CHECK (min_app_version ~ '^[0-9]+\.[0-9]+\.[0-9]+$'),
    set_by_migration integer NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO schema_compat (min_app_version, set_by_migration) VALUES ('1.0.0', 22);
```
Expand-only. (The next free number is 0022; `0021_track_air_duration` is the last.)

**B2. Version helpers and the check (B9).** `internal/migrations/compat.go` (SQL is allowed in `internal/migrations`, `internal/architecture/architecture_test.go:31`):
- `ParseVersion("X.Y.Z")`, `Compare`; `dev` and other non-release builds are "always compatible".
- `Compat(ctx, databaseURL) (Status, error)`: schema version and dirty flag (golang-migrate, as `Up` reads them, `migrations.go:67`), `min_app_version` (`SELECT min_app_version FROM schema_compat WHERE id = 1`; a missing table, `42P01`, means `1.0.0`).
- `Status.Compatible(version string) bool`.
- Tests: version parsing and comparison table; integration test (`migrations_integration_test.go` pattern): fresh database → `1.0.0`; after `UPDATE` to `1.4.0`, `1.3.9` is incompatible, `1.4.0` and `dev` are compatible; before migration 0022 (table missing) → `1.0.0`.

**B3. Startup guard in `backend serve` (B9).** `cmd/backend/serve.go` `serve` (`:71`), after `db.Open` (`:84-89`): `migrations.Compat`; when `version` (`cmd/backend/main.go:31`) is incompatible, log an ERROR "database schema needs at least <min>; this backend is <version>" and return an error (the process exits, Docker's restart policy keeps it down-looping, and the deploy tool's health gate fails visibly). A database that is not reachable at startup keeps today's behaviour (warning, `/readyz` reports it, `:95-100`); the check then runs at the first successful connection. Test in `cmd/backend` with a fake compat reader.

**B4. `backend migrate check` (B9).** `cmd/backend/main.go` `run` (`:58-62`): accept `migrate check` besides `migrate up` (update the usage text `:21-28` and the error message); `cmd/backend/migrate.go`: `migrateCheck` prints one JSON object to stdout — `{"schema_version": 22, "dirty": false, "min_app_version": "1.0.0", "binary_version": "1.0.0", "compatible": true}` — and exits 0 when compatible, 3 when not (2 stays the usage error). Uses `config.LoadMigrate` (only `DATABASE_URL`).

**B5. Migration rules (B9).** `CLAUDE.md` (migrations section) and the header of `internal/migrations/migrations.go`: from 1.0.0 every migration is expand-only and safe while the previous release runs; a contract migration (or any migration an older release cannot work with) ends with `UPDATE schema_compat SET min_app_version = '<oldest compatible release>', set_by_migration = <n>, updated_at = now() WHERE id = 1;`, is marked in the release notes (B6), and comes no earlier than the release after the one that stopped using what it drops.

**B6. Release notes: migrations line (B8, decided).** `.github/workflows/release.yml` job `check` (`:21-43`): the tag must be annotated and its message must contain one line `migrations: none|expand|contract`; read it with `git tag -l --format='%(contents)' "$GITHUB_REF_NAME"`; missing or invalid → fail. Job `release` (`:93-110`): prepend "Migrations: <value>" to `--notes`. Document in `CLAUDE.md` how to tag (`git tag -a v1.0.0 -m "migrations: expand"`).

**B7. Automatic recovery of a failed migration (B7, required; before the first release).**
- **Lint** (`internal/migrations/lint_test.go`, runs in `make test` and CI): for every embedded `*.up.sql`, after removing `--` comments, `/* */` comments, string literals, and dollar-quoted bodies (`DO $$ … BEGIN … END $$` is allowed), reject explicit transaction control (`BEGIN`, `START TRANSACTION`, `COMMIT`, `END`, `ROLLBACK`, `SAVEPOINT`, `RELEASE`, `PREPARE TRANSACTION`) everywhere, and statements that cannot run in a transaction block (`CREATE INDEX CONCURRENTLY`, `DROP INDEX CONCURRENTLY`, `REINDEX … CONCURRENTLY`, `VACUUM`, `CREATE DATABASE`, `ALTER SYSTEM`) unless the file's first line is `-- migrate: non-transactional`; a marked file must hold exactly one statement. The existing files 0001–0021 pass (they mention `CONCURRENTLY` only in comments).
- **Runner** (`internal/migrations/migrations.go`, the `Steps(1)` loop at `:93-114`): on an error of the step for version `v`:
  - read the file's header (`src.ReadUp(v)`); find the previous version (`src.Prev(v)`, or none for the first file);
  - if the file is not marked non-transactional and the error is a `database.Error` whose `OrigErr` is a `*pgconn.PgError` (the server answered, so it rolled the implicit transaction back), call `m.Force(prev)` (`-1` for none) and return `ErrRolledBack` with "rolled back, schema unchanged at version <prev>" and the SQLSTATE; a failing `Force` returns `ErrDirty`;
  - otherwise (a marked file, a network error, an error while golang-migrate writes the version) return `ErrDirty`: the outcome is unknown;
  - an already dirty database at the start (`:73-74`) returns `ErrDirty` as now.
  `dirtyError` (`:119-123`) no longer tells the operator to run `update.bat restore`; it says "the deploy tool restores the pre-update dump".
- **Exit codes** (`cmd/backend/main.go:39-48`, `cmd/backend/migrate.go`): `migrate up` exits 4 on `ErrRolledBack` and 5 on `ErrDirty`, 1 for other errors (nothing was changed, for example the database is unreachable), 2 for usage errors; `migrate check` keeps 3 for "incompatible". Before exiting, one JSON log line: `{"msg":"migration rolled back","version_failed":v,"schema_version":prev,"sqlstate":"55P03",...}` or `"msg":"database dirty"`; the migration SQL is cut to its first line.
- **`backend migrate target-compat`**: prints the `min_app_version` that the image's newest migration sets (a constant `migrations.TargetMinAppVersion`, which a test compares with the last `UPDATE schema_compat` among the embedded files, `1.0.0` when there is none). The deploy tool stops the running backend before migrating when the running release is below it (deploy plan §7.3 step 7).
- **Tests**: lint table (allowed `DO $$ BEGIN … END $$`, comment mentions, a marked single statement; refused `COMMIT`, unmarked `CONCURRENTLY`, a marked file with two statements); integration tests (`migrations_integration_test.go` pattern, extra embedded test files through an `fs.FS` parameter of an internal `up` function): a failing statement → `ErrRolledBack`, version at the previous one, not dirty, schema unchanged, earlier files of the same run applied; a lock timeout (another connection holds `ACCESS EXCLUSIVE`) → `ErrRolledBack` with `55P03`; a marked non-transactional file that fails → `ErrDirty`; `cmd/backend` exit codes 4 and 5; `target-compat` constant test.

### Phase C — Multi-arch images and CI (blocks the first release, which deploy D4's first run and the D7 arm64 dry run need)

**C1. Dockerfile for cross-builds (B10).** `docker/Dockerfile:6`: `FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-trixie AS build`, `ARG TARGETOS TARGETARCH`, and `GOOS=$TARGETOS GOARCH=$TARGETARCH` in the `go build` line (`:14-16`; `CGO_ENABLED=0` already). The runtime stage (`:18-43`) is unchanged; it is built per platform. The CI step "Go version in the Dockerfile" (`ci.yml:30-36`) reads `ARG GO_VERSION=` and keeps working.

**C2. Release workflow (B10).** `.github/workflows/release.yml` job `image` (`:52-91`):
- a matrix over `{runner: ubuntu-24.04, platform: linux/amd64}` and `{runner: ubuntu-24.04-arm, platform: linux/arm64}`: build natively, run the smoke checks on the built image (`backend version` equals the tag, `backend openapi` validates and has `info.version` equal to the tag), push by digest only (`docker buildx build --platform $platform --output type=image,name=$IMAGE,push-by-digest=true,name-canonical=true,push=true`), upload the digest as an artifact;
- a job `manifest` after both: `docker buildx imagetools create -t "$IMAGE:$VERSION" $IMAGE@<amd64 digest> $IMAGE@<arm64 digest>`, then `imagetools inspect` lists both platforms;
- "image tag is not taken" (`:61-66`) moves before the matrix and uses `docker buildx imagetools inspect "$IMAGE:$VERSION"` (a manifest list);
- the attached `openapi.json` comes from the amd64 image; the arm64 image's `openapi` must be byte-equal (checked in `manifest`).

**C3. CI (B10).** `.github/workflows/ci.yml` job `docker` (`:99-110`): a matrix building the image on both runners (no push) and running `backend version` in it.

**C4. Native arm64 smoke tests.** Both runners of C2 run the image they built, so no QEMU is needed (the same pattern as the frontend's F4).

### Phase D — Repository instructions

**D1. `CLAUDE.md`.**
- "Project context", **Runs on**: "Docker Engine in a dedicated WSL2 distribution on a Windows 10/11 PC, no Docker Desktop (tech-design v7-24 §9.1); development on macOS."
- "Dev stack notes", pinned images: `golang:1.27.4-trixie` (B4, resolved this way).
- "Required reading": add `docs/deploy-prereqs-plan.md` (deploy prerequisites: runtime stream output, TLS, migration recovery, downgrade guard, multi-arch release).
- Migration rules (the PostgreSQL and SQL section): every file runs in one implicit transaction; no explicit transaction control; a statement that cannot run in a transaction is alone in a file with `-- migrate: non-transactional`; from 1.0.0 expand-only and safe while the previous release runs; a contract migration raises `schema_compat.min_app_version` and `migrations.TargetMinAppVersion`; tag releases with `migrations: none|expand|contract`.
- Binary line: `backend migrate up` (exit 4 rolled back, 5 dirty) | `backend migrate check` | `backend migrate target-compat`.
- Liquidsoap notes: `output.set`, `BACKEND_OUTPUT_URL`, `stream.icecast_tls`, the contract header.

## 4. Tests

- Unit: settings key and descriptions (A1, A8); mount check over HTTPS and path matching (A2); output endpoint (A3); `applyOutput` cases (A5); version parsing and comparison (B2); startup guard (B3); `migrate check` output and exit codes (B4); migration lint and `target-compat` (B7).
- Integration: `schema_compat` default, raised minimum, missing table (B2); existing migration integration tests still pass with 0022; rolled-back, lock-timeout, and dirty cases of B7.
- Script: `liquidsoap --check` and the smoke test (A9) in CI; the dev check (A10) by hand.
- Release: a dry run of `release.yml` on a throwaway tag in a fork or with `workflow_dispatch` and a `-rc` image name is not available (tags are immutable); instead the CI matrix (C3) proves both builds, and the first real release is checked with `imagetools inspect`.

## 5. Order of work

1. A1, A2 (setting and HTTPS check; small, independent).
2. A3, A4, A5, A7 together (endpoint, script, push, dev compose), then A6 and A8.
3. A9 (CI smoke test), A10 (dev check). **Phase A closes; the deploy repository can finish D3, and D4's first run can write `stream.icecast_tls`.**
4. B1–B7 (B7 can be its own PR; it must be in before the deploy repository implements D5's failure paths).
5. C1–C4.
6. D1 (`CLAUDE.md`) with the last PR of each phase that changes a rule it describes, at the latest before the release.
7. Tag `v1.0.0` (with `migrations:` in the tag message) after Phases A–C and the frontend's F1, F3, F4. **The release unblocks deploy D4's first run and the arm64 dry run (D7).**

## 6. Done when

- `make test`, `make lint`, `make openapi-lint` pass, and `make test-integration` has no failure beyond the known ones (`TestPipeline`, `TestImportFolder`, `TestRotationSimulate`).
- `make liquidsoap-smoke` and the CI job pass for every codec × channel combination, plain and TLS.
- The dev check of A10 passes.
- A failing transactional migration leaves the database clean at the previous version and `migrate up` exits 4; a failing marked migration exits 5; the lint rejects explicit transaction control.
- `backend migrate check` reports `compatible: true` on a fresh dev database and `false` (exit 3) for a binary below a raised `min_app_version`; `backend serve` refuses to start in the same case.
- A release tag produces one manifest list with `linux/amd64` and `linux/arm64`; `docker pull` on the Mac gets arm64 and on an amd64 host amd64.
- The script's header carries `radio-atlantida-contract` and `min-backend`.
- `CLAUDE.md` is updated (D1).

## 7. Out of scope and not planned

- Loudness normalisation of the stream and HLS output (B5: descriptions only).
- **Not planned: B11** (the live voice bridge reading `LIVE_PUBLIC_IPS` per session). On Windows 10 (NAT mode) the deploy tool recreates the backend when the WSL VM address changes at boot; the 30 s recreate is acceptable.
- The production Liquidsoap script, compose file, and update tool: they are in `radio-atlantida-deploy` (deploy plan D2–D5).
- Changing the metadata charset handling (the server owner sets the mount charset).
