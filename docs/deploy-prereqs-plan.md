# Plan — Deploy Prerequisites: Runtime Stream Output, TLS, Migration Recovery and Downgrade Guard, Multi-arch Release (radio-atlantida-backend)

Status: Approved scope (deploy plan and tech-design v7-24, 2026-11-12). Phase A done (commits 6b270a5, e2b9178, 7214870; see §2.3; dev check A10 passed 2026-11-12; A11 done in 460e8f0, a722b20). Phase B done (137cc5d; B3 dirty-database decision 341702c), Phase C done (37cd98c), Phase D1 done (8efa7bc). Next: tag `v1.0.0` (§5 item 8). Deviations: §2.3, §2.4.
Based on: `docs/tech-design.md` v7-25 (§1.1, §6.3, §7.1, §8.1, §8.5, §8.8, §9, §9.2, §11 items 32–38), `radio-atlantida-docs/deploy/docs/deploy-init-plan.md` (§5 Phase D3, §6 Phase D4, §7.3, §10, §12 cross-repo tasks B1–B10, B12), frontend `docs/deploy-prereqs-plan.md` (F1–F4)
Last updated: 2026-11-12

## 1. Goal

The deploy repository (`radio-atlantida-deploy`) can only finish its phases D3 and D4 when the backend:

- applies the `stream.*` settings to Liquidsoap at runtime, without a container restart, and streams over TLS to the station's external Icecast server (`https://listen.teploe.net:8001/atlantida`, TLS only);
- carries a contract version header in its Liquidsoap script, which the deploy repository copies;
- records which oldest release can run against the schema, so the deploy tool can install older releases safely;
- undoes a failed migration by itself, so the deploy tool can keep the running release on air without asking the operator anything;
- publishes its image for `linux/amd64` (production) and `linux/arm64` (the developer's Mac) under one tag.

This plan covers the deploy plan's cross-repo tasks B1, B2, B3, B4 (resolved as a `CLAUDE.md` update to 1.27.4, step D1), B5, B6, B7 (required: automatic recovery of a failed migration), B8 (decided), B9, B10, and B12 (encoder limits at save, A11). B11 is not planned (§7).

**Precondition:** the uncommitted working-tree change (`docker/Dockerfile` `GO_VERSION=1.27.4`, `go.mod`) is committed first; it is not part of this plan.

## 2. Decisions (2026-11-12, from the deploy plan)

1. **A reconnect is not a restart** (design §6.3, §8.8). A saved `stream.*` value is sent to Liquidsoap over the existing control link; the script replaces its `output.icecast` and leaves the sources, queues, fallback, and journal alone.
2. **One telnet command, JSON argument.** `output.set <json>` on the control port. The script parses it with `json.parse`, so no setting value ever becomes script code. The source password stays in Liquidsoap's environment and is never sent.
3. **Idempotent.** An `output.set` equal to the running output answers `OK unchanged` and does nothing, so the backend can send it after every reconnect without interrupting the stream.
4. **Shut down, then create.** On the same mount Icecast refuses the new source (403) until the old one has gone; the script shuts the old output down first and retries the new one after 1 s.
5. **Startup without the backend.** Liquidsoap reads its first output from `GET /api/playout/output`, falling back to the last output that connected (a state file, written on connect, not on `output.set`), then to its environment (`ICECAST_HOST`, ...), then to defaults equal to the settings defaults. The backend's push corrects it as soon as the control link connects.
6. **TLS is a setting**, `stream.icecast_tls` (bool, default `false` so the dev stack's Icecast keeps working; reconnect; affects air). It selects `transport=http.transport.tls()` in the script (not `ssl()`, §2.3) and `https` in the outage detector's mount check.
7. **`--check` is not enough.** `liquidsoap --check` accepts a script that fails at runtime (a non-literal `%mp3(stereo=…)`, an encoder over its limit), so CI runs the script against a test Icecast.
8. **B5, minimal option.** Nothing new is implemented. `stream.loudness_target_lufs` stays: the import and Fix loudness use it (`cmd/backend/serve.go:622`); its description says it is not applied to the output. `stream.hls_enabled` stays as a reserved setting whose description says "not applied yet". No code reads either for the output.
9. **`schema_compat` ships in 1.0.0** (the baseline), so every release from 1.0.0 has the table; a missing table still counts as `1.0.0` for safety.
10. **A failed migration never leaves the operator a decision** (B7, required). A failed transactional migration is rolled back by PostgreSQL; `backend migrate up` resets golang-migrate's version so the database is clean, and exits with code 4. A database left dirty exits with code 5, and the deploy tool restores the pre-update dump by itself (deploy plan §7.3 step 7).
11. **No release grouping.** Each migration file is its own unit. Files applied earlier in the same run stay applied: they are expand-only, so the running release works with them. Applying one release's files as one unit would need one transaction across files (golang-migrate commits per file, and a non-transactional file cannot join a transaction) or undoing committed files (which forward-only migrations do not allow); both are worse than "the schema is at version N, compatible with the running release".
12. **B8 decided:** annotated release tags with a line `migrations: none|expand|contract`.
13. **Multi-arch by native runners** (`ubuntu-24.04` and `ubuntu-24.04-arm`; the repository is public, so both are free): the runtime stage runs `apt-get install ffmpeg`, which would need QEMU on one runner. One manifest list per tag; the deploy repository pins its digest.
14. **Encoder limits are refused at save** (2026-11-12, recommended by the main agent; the developer may override). Besides the script's refusal, the backend refuses to save a `stream.*` value that makes a combination over the encoder limits (§2.3), with a 4xx and a clear message on the field, so the panel shows the error at save time (A11). **Batch save** (developer decision, 2026-11-12): saving several `stream.*` fields one by one gives order-dependent refusals, so settings are saved as a batch, checked against the merged result and written all or nothing, with one change notification (A11).

### 2.1 Verified with `savonet/liquidsoap:v2.4.6` (2026-11-11/12)

Throwaway containers, test scripts kept in the session scratchpad (`liqtest/rt.liq`, `tlsout.liq`, `cs.liq`):

- An `output.icecast` created from a `server.register` handler starts and connects; `shutdown()` on the old one closes it; `source.time(radio)` keeps counting through the switch.
- Changed at runtime on one source: mp3 128/44100/stereo → mp3 192/48000/mono (new mount) → `%vorbis.cbr` 128 → `%fdkaac` 128 → mp3 320 on the same mount. Icecast reported every format. Gap on the same mount: about 4 s (first connect 403, retry after the default 3 s).
- `bitrate` and `samplerate` may be runtime values; `%mp3(stereo=…)` must be the literal `true` or `false` (runtime error otherwise, not caught by `--check`); a mono encoder needs a mono source (`mean(s)`).
- Output ids repeat on every switch (`out_mp3`, `out_mp3.1`, ...): create outputs with `register_telnet=false`.
- `transport=http.transport.ssl()` and `http.transport.tls()` both pass `--check`; a source connection through a TLS proxy to the test Icecast connected and streamed; `http.get` over HTTPS to the production status page returned 200 (the Let's Encrypt chain is trusted in the image). Corrected in §2.3: `ssl()` rejects public chains, so the script uses `tls()`.
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

### 2.3 Found in Phase A (2026-11-12, commits 6b270a5, e2b9178, 7214870)

Deviations from the steps below, as implemented:

- **TLS: `http.transport.tls()`, not `ssl()`.** In the 2.4.6 image `http.transport.ssl()` rejects public certificate chains, Let's Encrypt included ("unable to get local issuer certificate"). `tls()` verifies the server against the system roots (`radio.liq` `make_output`). The smoke test's TLS proxy uses a throwaway CA mounted over the image's `ca-certificates.crt`.
- **State file path:** `/var/cache/liquidsoap/output.json` (`OUTPUT_STATE_FILE`) on the volume `liquidsoap-state` mounted at `/var/cache/liquidsoap`, not `/var/lib/radio`. The container runs as user `liquidsoap`; a new named volume takes the owner of the directory it is mounted over, and over a directory the image lacks it is root's. The script's built-in default is now `/var/cache/liquidsoap/output.json` too (fixed in A11, 460e8f0); the compose file still sets the variable. Environment: `BACKEND_OUTPUT_URL`, `OUTPUT_STATE_FILE`, `ICECAST_TLS`.
- **State written on connect.** The script saves the output in `on_connect`, not when it accepts `output.set`, so an output that crashes Liquidsoap never becomes the startup output. A failed write is logged at INFO ("output state not saved").
- **Encoder limits.** Some Vorbis combinations make Liquidsoap exit when the output starts: mono 22050 Hz above 64 kbps, mono 32000 Hz above 160, mono 44100/48000 Hz above 192, stereo 22050 Hz above 160. The script refuses them with `ERROR <codec> with <n> channel(s) at <rate> Hz allows at most <max> kbps` (`ENCODER_LIMITS`, kept in step with `dev/liqsmoke/limits.go`). AAC uses `%fdkaac(aot="mpeg4_aac_lc", …)`: the default HE-AAC v2 fails on a mono source.
- **Smoke test covers all four sample rates** (22050, 32000, 44100, 48000 Hz) for every codec × channels, at 64 and 320 kbps and on both sides of every limit (`testBitrates`), plain and over TLS.
- **Strict `json.parse`.** The output is parsed into a typed record in which every field is required (unknown fields are ignored), not a record with defaults; a missing field answers `ERROR invalid output: …`.

### 2.4 Found in A11 and Phases B, C (2026-11-12)

A11 (460e8f0, a722b20):
- a cross-key check (`settings.Check`) is skipped when one of its keys was already refused by its own rules or a guard;
- an error from a check that is not a refusal fails the save with 500;
- subscribers are notified while the save mutex (`saveMu`) is held;
- `UpsertMany` uses GORM `CreateInBatches` with the batch size equal to the row count, so it is one statement;
- `dev/liqsmoke/limits.go` became `bitrates.go` (the bitrates the smoke test sends, from the package's table); the drift test is `TestEncoderLimitsMatchScript` (`dev/liqsmoke/pins_test.go`);
- huma reports an unknown top-level property of the `PATCH /api/settings` body as field `values.extra`.

Phases B, C (137cc5d, 341702c, 37cd98c):
- B3, developer decision (341702c): a dirty database does not stop `backend serve`; it logs ERROR "database dirty at schema version N: a migration failed part-way; serving anyway" and keeps serving. Refusing would stop the old, working backend after a failed non-transactional migration, while the deploy tool already restores the pre-update dump after `migrate up` exit 5.
- `migrations.Compat` reads `schema_migrations` and `schema_compat` with plain SQL, without golang-migrate's lock (and creates no table).
- `backend serve` exits 3 when incompatible.
- The B7 lint also refuses `ABORT`, and refuses `BEGIN ATOMIC` function bodies (it does not parse them, so it errs on the safe side).
- An empty database reports `schema_version` 0.
- Pre-release versions such as `1.0.0-rc1` do not parse as a release, so they count as non-release builds and are always compatible.

**For the release:** `v1.0.0` must be an annotated tag with the migrations line, for example `git tag -a v1.0.0 -m "migrations: expand"` (B6 refuses a lightweight tag).

## 3. Phases and steps

Steps are in dependency order. **D3** / **D4** marks the deploy phase a step blocks.

### Phase A — Runtime stream output and TLS (blocks deploy D3; A1 and A2 also block D4) — done

A1, A2, A8: commit 6b270a5. A3–A7: e2b9178. A9 and the Liquidsoap notes of D1: 7214870. A11: 460e8f0, a722b20. The step texts below are the plan; §2.3 lists where the implementation differs.

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
- `make_output(cfg)`: one branch per codec and channel count — `%mp3(bitrate=br, samplerate=sr, stereo=true)` / `stereo=false` on `mean(radio)`, `%vorbis.cbr(bitrate=br, samplerate=sr, channels=2)` / `channels=1` on `mean(radio)`, `%fdkaac(aot="mpeg4_aac_lc", bitrate=br, samplerate=sr, channels=2)` / `channels=1` on `mean(radio)`; `transport=http.transport.tls()` when `tls` (§2.3), the default transport otherwise; `name`, `description`, `genre`, `host`, `port`, `mount`, `password=icecast_password`, `id="icecast"`, `register_telnet=false`; `out.on_error(synchronous=false, fun (restart_in, _) -> restart_in(1.))`; the existing `on_connect`/`on_disconnect` reports (`:355-356`) attached to every new output.
- `current_output` (a `ref`) and `current_cfg` (the normalised JSON string).
- `server.register("output.set", fun (arg) -> ...)`: `json.parse` into a typed record, every field required (§2.3); validate the enums (codec, bitrate 64–320 in the settings set, sample rate 22050/32000/44100/48000, channels 1/2, port 1–65535, mount starting with `/`) and the encoder limits (`ENCODER_LIMITS`, §2.3); equal to `current_cfg` → `"OK unchanged"`; otherwise `current_output().shutdown()`, create, store → `"OK applied"`; the new output writes `OUTPUT_STATE_FILE` (`file.write`, atomic) when it connects; invalid → `"ERROR <reason>"` and the running output stays.
- Startup: `http.get(timeout=2., BACKEND_OUTPUT_URL)` → state file → environment (`ICECAST_HOST`, `ICECAST_PORT`, `ICECAST_MOUNT`, existing `:28-30`, plus `ICECAST_TLS`) → defaults; the chosen source is logged.
- Header comment (`:1-22`): replace the output bullet; note the new contract (A6).
- New environment: `BACKEND_OUTPUT_URL`, `OUTPUT_STATE_FILE` (set to `/var/cache/liquidsoap/output.json`, §2.3; a missing directory means no state file, not an error), `ICECAST_TLS`.

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

**A7. Dev compose (B1, blocks D3).** `docker-compose.dev.yml` `liquidsoap` (`:56-92`): `BACKEND_OUTPUT_URL: ${BACKEND_OUTPUT_URL:-http://host.docker.internal:8090/api/playout/output}`, `OUTPUT_STATE_FILE: /var/cache/liquidsoap/output.json`, `ICECAST_TLS: "false"`, volume `liquidsoap-state:/var/cache/liquidsoap` (§2.3; was `/var/lib/radio` in the plan) (and in `volumes:` `:257-261`). The header comment (`:11-15`) keeps saying that the settings defaults must match this file.

**A8. Setting descriptions (B5).** `internal/settings/definitions.go`: `StreamLoudnessLUFS` (`:195`) → "Loudness target of the tracks: the import checks and Fix loudness use it; the stream output is not normalised."; `StreamHLSEnabled` (`:199`) → "Reserved: HLS output is not implemented yet; this value has no effect." Apply mode and range unchanged. Frontend help texts follow in F3.

**A9. Runtime smoke test of the script in CI (B3, blocks D3).**
- `dev/liqsmoke/main.go` (a dev tool like `dev/livecheck`): serves a stub backend (`/api/playout/callback` 204, `/api/playout/emergency/next` 204, `/api/playout/output` with a given config), connects to the telnet port, sends `output.set` for every codec × channel count with the lowest and the highest bitrate (64 and 320) and, as implemented, all four sample rates and both sides of every encoder limit (§2.3), and after each waits up to 10 s for Icecast's `/status-json.xsl` to list the mount with the expected `server_type` and `audio_info`. Then one switch to TLS through the proxy. Exit code non-zero on any miss.
- `dev/liqsmoke/compose.yml`: `libretime/icecast:2.4.4-alpine`, a TLS proxy (pinned `nginx` alpine image with a `stream` block and a throwaway certificate generated in the job, as in the 2026-11-12 check), `savonet/liquidsoap:v2.4.6` with `dev/liquidsoap/radio.liq` (the dev stack's pins, read from `docker-compose.dev.yml`).
- `.github/workflows/ci.yml`: a job `liquidsoap` running `liquidsoap --check` and then the smoke test.
- `Makefile`: `make liquidsoap-smoke`.

**A10. Dev check (manual, before closing Phase A) — done (2026-11-12).** On the dev stack: change codec, bitrate, and mount in the panel while a track plays; the stream follows within a few seconds, the queue and the journal continue; restart the backend: no reconnect (`OK unchanged`); restart Liquidsoap with the backend down: it starts from the state file. Run by script by the Phase A agent: Liquidsoap recreated → `OK unchanged`; codec, bitrate, and mount changed through the API and Icecast followed each; the journal kept growing. The backend-restart path was covered by the smoke test instead of on the dev stack.

**A11. Batch settings save with encoder limit validation (B12; before the first release) — done (460e8f0, a722b20; deviations in §2.4).** Decision 14. The backend refuses a stream format the encoders cannot start, so the panel shows the error when the manager saves, not later as a playout alert; and it saves several settings at once, so the refusal does not depend on the order of the fields and a change of several stream fields gives one reconnect.

- **One table in Go.** A new package `internal/streamformat` (named after what it provides; no SQL, no settings import) holds the encoder limits of the pinned Liquidsoap (the five rows of §2.3) and `func Check(codec string, channels, sampleRateHz, bitrateKbps int) *Limit` (nil when allowed; `Limit` carries the codec, channels, rate, and `MaxKbps`). `dev/liqsmoke` uses it instead of its own `limits.go` (`encoderLimits`, `encoderLimit`; `testBitrates` stays in the tool, reading the package's table), so the smoke test still sends both sides of every limit. A unit test in `dev/liqsmoke` (next to `pins_test.go`) parses the `encoder_limits` list of `dev/liquidsoap/radio.liq` and compares it with the package's table, so a drift fails `make test` without Docker.
- **Endpoint `PATCH /api/settings`** (operation `set-settings`, `internal/httpapi/handlers/settings.go`, next to `set-setting`; PATCH because it changes some settings of the collection, as `move-hour-insert` does for one resource). Body `dto.SetSettings{Values map[string]any}` (`json:"values"`, `minProperties:"1"`, keys as in `PUT /api/settings/{key}`). 200 → `dto.SettingList` of the saved settings, in registry order. 422 `validation_failed` → `errors[]` in the existing field-error format, one per refused key, `field = "values.<key>"` (for example `values.stream.bitrate_kbps`), `code` from `settingRuleCodes`, so a type, range, or enum error and an encoder limit error come back together. An unknown key or a secret in the batch is a field error on that key (`unknown_field`; the message says that a secret is set in the environment) rather than the 404/409 of the single-key route. Add the path to `internal/httpapi/router_test.go`.
- **Service** (`internal/settings/settings.go`): `func (s *Service) SetMany(ctx, values map[string]json.RawMessage) ([]Setting, error)`:
  1. every key: `editable` and `d.parse` (its own rules); collect all refusals instead of stopping at the first;
  2. the merged view: current values (`store.List` once) with the parsed batch on top;
  3. the per-key guards (`Guard`, the retention guard unchanged) for every key of the batch, and the new cross-key checks (below) against the merged view;
  4. any refusal → return `*InvalidValuesError{Errors []*InvalidValueError}` and write nothing;
  5. otherwise write the batch in one transaction (step "Store"), log one `setting changed` line per key as now, and notify once.
  A service mutex (`saveMu`) serializes `Set` and `SetMany`, so the checks and the write see the same saved values within the process (one backend per database).
  - `Set` (the single-key PUT) becomes `SetMany` with one key; its handler keeps mapping the one refusal to `field = "value"`, so the existing route and the panel's older behaviour are unchanged. For a stream key it therefore also runs the encoder check against the merged state (saved values plus that one value).
- **Cross-key checks.** Next to `Guard[T]`: `settings.Check(s, keys []string, fn func(ctx context.Context, v View) error)`, run once per save when the batch touches any of `keys`. `View` is the merged state: `settings.ViewValue[T](v, Key[T])` returns the batch value or the current one, and `v.Changed(key)` tells which keys the batch carries. The refusal is `settings.Refuse(key, rule, msg)` per changed key (several joined with `errors.Join`; the service unpacks them into `InvalidValuesError`). `Guard[T]` stays for checks against data outside the settings.
- **Encoder rule.** `settings.RuleEncoderLimit = "encoder_limit"` (`settings.go:37-52`), field code `errcodes.EncoderLimit Field = "encoder_limit"` (`internal/httpapi/errcodes/errcodes.go`, next to `IntervalBeyondRetention` `:238`), mapped in `settingRuleCodes` (`internal/httpapi/handlers/errors.go:249-260`). The check (`cmd/backend/serve.go`, next to `guardRetention` `:495`) registers `settings.Check` on `stream.codec`, `stream.bitrate_kbps`, `stream.sample_rate_hz`, `stream.channels`, reads the four from the view, calls `streamformat.Check`, and on a violation refuses each of the four that the batch changed, with a message that names the limit and the values, for example "Vorbis mono at 22050 Hz allows at most 64 kbps; the bitrate is 128 kbps." Because the check sees the final combination, there is no "change this field first" advice and no order dependence.
- **Store.** `settings.Store` gains `UpsertMany(ctx, []Stored) ([]Stored, error)`, implemented in `internal/repository/settings` as one multi-row `INSERT … ON CONFLICT (key) DO UPDATE` statement (one statement is one implicit transaction in PostgreSQL, §2.2, so the batch is saved whole or not at all and a concurrent reader, such as the pusher's pass, never sees half a batch). `Upsert` is then unused and removed (the fake stores in the tests follow).
- **One notification.** `Subscribe` changes to `func(fn func([]Change))`: one call per save, after the write, with every saved key (one element for the single-key PUT). `OnChange` loops over the slice, so its users (`ctl.Reset` on control host and port, music gain, duck gain, `cmd/backend/serve.go:291-300`) are unchanged. `wakeOnStreamChange` (`serve.go:310`) takes the slice and calls `p.Wake()` once when any key starts with `stream.`. The pusher needs no change: `Wake` coalesces (a buffered channel of one, `internal/playout/playout.go:289`), the pass reads all stream settings after the write, and `applyOutput` sends `output.set` only when the command differs from the last applied one, so one batch gives one `output.set` and one reconnect.
- **Script default.** `dev/liquidsoap/radio.liq` (`:60`): the default of `OUTPUT_STATE_FILE` becomes `/var/cache/liquidsoap/output.json` (§2.3), the path the compose file already sets.
- `make openapi` and `make openapi-lint` (new operation, `SetSettings`, `FieldErrorCode` gains `encoder_limit`); the frontend syncs the snapshot (frontend F3).
- Tests:
  - unit: `internal/streamformat` table test (each limit at and above its maximum, other codecs at 320); the `dev/liqsmoke` drift test against `radio.liq`; `internal/settings` with a fake store: `SetMany` collects every refusal (a range error and an encoder limit in one batch), one invalid value means `UpsertMany` is not called, subscribers get one call with all keys, a single `Set` gives one call with one key, `View` returns batch values over saved ones;
  - `cmd/backend` check tests: bitrate over the limit refused with `encoder_limit`, the same bitrate allowed at another sample rate, the case that failed with one-by-one saves (saved Vorbis mono 22050 Hz 64 kbps; batch sample rate 44100 Hz + bitrate 192 kbps) accepted in either key order, a single-key PUT of the sample rate against a saved bitrate refused, `mp3` with any combination allowed; `wakeOnStreamChange` wakes once for a batch of several stream keys and not for a batch without one;
  - handler (`internal/httpapi`): `PATCH /api/settings` 200 with the saved list; 422 with `errors[].field = "values.stream.bitrate_kbps"`, `code = "encoder_limit"`, and several field errors in one response; an unknown key; `PUT /api/settings/{key}` still answers `field = "value"`;
  - integration (`internal/repository/settings`, and the service on the real store): a batch with one invalid value saves nothing (the earlier values stay); `UpsertMany` writes all rows with one `updated_at`.
- `CLAUDE.md`: the settings section notes that the panel saves through `PATCH /api/settings` (all or nothing, one notification per save, `Subscribe` receives `[]Change`, cross-key rules are `settings.Check`); the Liquidsoap note: the encoder table is `internal/streamformat`, used by the settings check and `dev/liqsmoke`, kept in step with `ENCODER_LIMITS` by a unit test.

### Phase B — Migration recovery, downgrade guard, release notes (blocks deploy D5's failure handling; must ship in 1.0.0) — done

B1–B7: commit 137cc5d; B5 and B8 rules in `CLAUDE.md`: 8efa7bc; B3 dirty-database decision: 341702c. Deviations in §2.4.

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

**B3. Startup guard in `backend serve` (B9).** `cmd/backend/serve.go` `serve` (`:71`), after `db.Open` (`:84-89`): `migrations.Compat`; when `version` (`cmd/backend/main.go:31`) is incompatible, log an ERROR "database schema needs at least <min>; this backend is <version>" and return an error (the process exits, Docker's restart policy keeps it down-looping, and the deploy tool's health gate fails visibly). A database that is not reachable at startup keeps today's behaviour (warning, `/readyz` reports it, `:95-100`); the check then runs at the first successful connection. Test in `cmd/backend` with a fake compat reader. As implemented (341702c, developer decision): a dirty database does not stop `serve`; it is logged at ERROR and serving continues (§2.4).

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

### Phase C — Multi-arch images and CI (blocks the first release, which deploy D4's first run and the D7 arm64 dry run need) — done

C1–C4: commit 37cd98c.

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
- Liquidsoap notes: `output.set`, `BACKEND_OUTPUT_URL`, `stream.icecast_tls`, the contract header. **Done** in 7214870 (also `make liquidsoap-check` / `make liquidsoap-smoke` and `dev/liqsmoke/` in the layout).

**Status (2026-11-12): done** (8efa7bc; the Liquidsoap notes in 7214870, the dirty-database note in 341702c).

## 4. Tests

- Unit: settings key and descriptions (A1, A8); encoder limit table and drift test, `SetMany`, cross-key checks, one notification per save, the batch endpoint and the 422 field code (A11); mount check over HTTPS and path matching (A2); output endpoint (A3); `applyOutput` cases (A5); version parsing and comparison (B2); startup guard (B3); `migrate check` output and exit codes (B4); migration lint and `target-compat` (B7).
- Integration: `schema_compat` default, raised minimum, missing table (B2); existing migration integration tests still pass with 0022; batch save all or nothing (A11); rolled-back, lock-timeout, and dirty cases of B7.
- Script: `liquidsoap --check` and the smoke test (A9) in CI; the dev check (A10, done).
- Release: a dry run of `release.yml` on a throwaway tag in a fork or with `workflow_dispatch` and a `-rc` image name is not available (tags are immutable); instead the CI matrix (C3) proves both builds, and the first real release is checked with `imagetools inspect`.

## 5. Order of work

1. A1, A2 (setting and HTTPS check; small, independent).
2. A3, A4, A5, A7 together (endpoint, script, push, dev compose), then A6 and A8.
3. A9 (CI smoke test), A10 (dev check) — done. **The deploy repository can finish D3, and D4's first run can write `stream.icecast_tls`.**
4. A11 (batch save and encoder limits), independent of the deploy phases but needed before the release; the frontend's F3 waits for it (snapshot with `PATCH /api/settings`). **Phase A closes.** — done (460e8f0, a722b20).
5. B1–B7 (B7 can be its own PR; it must be in before the deploy repository implements D5's failure paths). — done (137cc5d, 341702c).
6. C1–C4. — done (37cd98c).
7. D1 (`CLAUDE.md`) with the last PR of each phase that changes a rule it describes, at the latest before the release. — done (8efa7bc).
8. Tag `v1.0.0` as an annotated tag with the `migrations:` line (`git tag -a v1.0.0 -m "migrations: expand"`) after Phases A–C and the frontend's F1, F3, F4. **The release unblocks deploy D4's first run and the arm64 dry run (D7).**

## 6. Done when

- `make test`, `make lint`, `make openapi-lint` pass, and `make test-integration` has no failure beyond the known ones (`TestPipeline`, `TestImportFolder`, `TestRotationSimulate`).
- `make liquidsoap-smoke` and the CI job pass for every codec × channels × sample rate, on both sides of every encoder limit, plain and TLS.
- `PATCH /api/settings` saves a batch all or nothing with one change notification (one `output.set`); a combination over an encoder limit, in a batch or a single-key PUT, is refused with 422 and the field code `encoder_limit` whatever the order of the fields; the limits live in one Go package used by the check and `dev/liqsmoke`, with a unit test against `ENCODER_LIMITS` (A11; done, a722b20, 460e8f0).
- The script's `OUTPUT_STATE_FILE` default is `/var/cache/liquidsoap/output.json` (A11; done, 460e8f0).
- The dev check of A10 passes (done 2026-11-12).
- A failing transactional migration leaves the database clean at the previous version and `migrate up` exits 4; a failing marked migration exits 5; the lint rejects explicit transaction control.
- `backend migrate check` reports `compatible: true` on a fresh dev database and `false` (exit 3) for a binary below a raised `min_app_version`; `backend serve` refuses to start (exit 3) in the same case; a dirty database is only logged at ERROR (341702c).
- A release tag produces one manifest list with `linux/amd64` and `linux/arm64`; `docker pull` on the Mac gets arm64 and on an amd64 host amd64.
- The script's header carries `radio-atlantida-contract` and `min-backend`.
- `CLAUDE.md` is updated (D1).

## 7. Out of scope and not planned

- Loudness normalisation of the stream and HLS output (B5: descriptions only).
- **Not planned: B11** (the live voice bridge reading `LIVE_PUBLIC_IPS` per session). On Windows 10 (NAT mode) the deploy tool recreates the backend when the WSL VM address changes at boot; the 30 s recreate is acceptable.
- The production Liquidsoap script, compose file, and update tool: they are in `radio-atlantida-deploy` (deploy plan D2–D5).
- Changing the metadata charset handling (the server owner sets the mount charset).
