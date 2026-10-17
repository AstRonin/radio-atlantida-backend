# Deploy Initialization Plan (radio-atlantida-deploy)

Status: Draft for Claude Code
Based on: `docs/base.md` (requirements), `docs/tech-design.md` (technical design, §1.1, §8.7–8.9, §9, §10, §10.3, §10.4), `docs/backend-init-plan.md` (dev stack and service names), `docs/frontend-init-plan.md` (panel image)
Last updated: 2026-10-15

## 0. How to use this plan

- **Context documents.** Before any work, Claude Code reads the files in `docs/`:
  - `docs/base.md` — product requirements.
  - `docs/tech-design.md` — the authority on how the system is built. §9.2 (deployment and updates) is the specification for this repository.
  - `docs/backend-init-plan.md` and `docs/frontend-init-plan.md` — the dev stack and build conventions the production stack must match.
  - `docs/deploy-init-plan.md` — this plan.
- This repository contains **no application code**. It runs prebuilt images from GitHub Container Registry and holds the stack definition, the production Liquidsoap script, and the Windows update tooling.
- Work one phase at a time. Each phase ends with a passing validation run and a commit (one PR per phase).
- Section 8 lists decisions and open points. **ASK** items must be confirmed by the developers before the phase that needs them. **DEFAULT** items are applied unless the developers say otherwise.
- Scope of initialization: Phases D1 to D5. They produce a complete, validated deploy repository. Running it on the production Windows machine is a manual step (Section 6).

## 1. Ground rules

- **No application code.** No Go, no Angular, no build steps for them. Application images are referenced by tag only, `ghcr.io/astronin/radio-atlantida-backend:${IMAGE_TAG}` and `ghcr.io/astronin/radio-atlantida-frontend:${IMAGE_TAG}`.
- **Infrastructure images are pinned** to exact tags (Icecast, PostgreSQL, Liquidsoap, Grafana, Loki, Alloy). No `latest` anywhere. The same infrastructure tags as the dev stack in `radio-atlantida-backend` (backend plan, §10.5).
- **Secrets never in git.** `.env` is git-ignored. `.env.example` lists every variable with a placeholder value and a comment. The secrets are `ICECAST_SOURCE_PASSWORD`, `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD`, the database password, the registry token, `GRAFANA_ADMIN_PASSWORD`, and `GRAFANA_DB_PASSWORD`.
- **Host paths come from `.env` and never change**: `MEDIA_ROOT`, `LOG_ROOT`, `PLAYOUT_JOURNAL_ROOT`, `BACKUP_ROOT` (design §3.1, §9, decision 25). Media files are never inside a container or a named volume (design §8.7).
- **Network exposure follows design §9 and §8.9.** Only the panel port (bound to `127.0.0.1` by default, see 8.1), the Icecast listener port, and the Grafana port (always bound to `127.0.0.1`, see 8.12) are published to the host. Loki and Alloy are never published. PostgreSQL and the Liquidsoap control port are never published. The panel has no authentication, so it must be reachable only from the local machine or a trusted private network (design §10).
- **Forward-only migrations.** The update tooling never runs `migrate down` and never restores a database without the manager's explicit confirmation (design §9.2).
- **The update script is the only way to change the production stack.** Manual `docker compose` commands are allowed for diagnostics, not for updates, because the update script records state needed for rollback and restore.
- **Windows is the target.** Windows 10 or 11 with Docker Desktop (WSL2 backend). The update tooling is Windows PowerShell. Git may be absent on the machine, so the update tooling must work without it (zip download fallback, §4 D4).
- **Line endings.** The repository default is LF (`* text=auto eol=lf`), but `*.bat` and `*.ps1` are stored with CRLF, because `cmd.exe` and Windows PowerShell handle them more reliably that way (see 8.4).

### 1.1 Docker Compose and PowerShell conventions (rules for the AI)

**Compose**

- Use the Compose Specification (no top-level `version:` key).
- Every service has a `healthcheck`, a `restart: unless-stopped` policy, and an explicit `image:`. Services that build locally do not exist in this repository.
- Dependencies use `depends_on` with `condition: service_healthy` or `service_completed_successfully`. Never rely on start order alone.
- Named volumes only for PostgreSQL data and the monitoring state: `grafana-data`, `loki-data`, `alloy-data` (all rebuildable from this repository and the files on the host, not backed up). Everything else is a bind mount from a host path in `.env` or a read-only file of this repository.
- Service names are identical to the dev stack (`db`, `icecast`, `liquidsoap`, `migrate`, `backend`, `frontend`, `grafana-role`, `grafana`, `loki`, `alloy`). The backend, the frontend nginx, and the Liquidsoap callback URL depend on these names (backend plan §3 task 9, frontend plan §3 task 9).
- Resource limits are not set in v1 (DEFAULT). They can be added after the first measured run on the production machine.

**PowerShell**

- Target Windows PowerShell 5.1, which ships with Windows 10 and 11. PowerShell 7 must not be required.
- `Set-StrictMode -Version Latest` and `$ErrorActionPreference = 'Stop'` in every script. External commands are checked through `$LASTEXITCODE`.
- Native command output is captured and written to the run log. Secrets are never printed. The `.env` file is read but its values are not echoed.
- Functions are small and named as verbs. No aliases in scripts. Paths are built with `Join-Path`.
- The file is saved as UTF-8 with BOM, so Windows PowerShell reads non-ASCII text correctly.
- PSScriptAnalyzer runs in CI with no suppressions unless commented.

## 2. Repository layout

```
radio-atlantida-deploy/
  .github/workflows/ci.yml            # validates compose, scripts, line endings, runs Pester tests (every PR)
  config/
    liquidsoap/radio.liq              # production playout script (same interface as the dev script, see 8.2)
    icecast/README.md                 # Icecast is configured only by environment variables (no file); note only
    grafana/
      role.sql                        # idempotent: role grafana_ro (pg_monitor, timeouts), password from the session setting radio.grafana_password
      provisioning/
        datasources/postgres.yaml     # PostgreSQL data source, fixed uid, password from the environment
        datasources/loki.yaml         # Loki data source, fixed uid
        dashboards/dashboards.yaml    # dashboard provider (files, no UI saves)
      dashboards/database.json        # the live database dashboard
      dashboards/logs.json            # log search: backend logs, playout journal, errors by component
    loki/loki.yaml                    # single process, filesystem storage, 10 days retention
    alloy/config.alloy                # reads LOG_ROOT and PLAYOUT_JOURNAL_ROOT (polling), labels, pushes to loki
  docs/
    base.md
    tech-design.md
    backend-init-plan.md
    frontend-init-plan.md
    deploy-init-plan.md               # this plan
  docker-compose.yml                  # the production stack (§3 D2)
  .env.example                        # every variable, placeholder values, comments
  update.bat                          # thin entry point: runs update.ps1 with the same arguments
  update.ps1                          # update, rollback, restore (§4 D4)
  tests/
    update.Tests.ps1                  # Pester tests with docker and git mocked (§4 D5)
  scripts/
    check-line-endings.ps1            # CI helper, fails on wrong line endings
  .gitattributes
  .gitignore
  .editorconfig
  README.md                           # runbook: install, first run, update, rollback, restore
```

Runtime folders, created by the scripts and never committed: `backups/`, `logs/`, `state.json`, `.env`.

## 3. Phase D1 — Skeleton and conventions

**Goal:** the repository has its structure, ignore rules, line-ending rules, and the environment template. Nothing runs yet.

Tasks:

1. `.gitignore`: `.env`, `backups/`, `logs/`, `state.json`, `*.dump`, `.DS_Store`, `Thumbs.db`.
2. `.gitattributes`:
   ```
   * text=auto eol=lf
   *.bat text eol=crlf
   *.ps1 text eol=crlf
   *.dump binary
   ```
3. `.editorconfig`: UTF-8, LF by default, CRLF for `*.bat` and `*.ps1`, final newline, 2 spaces for YAML.
4. `.env.example`: every variable the stack reads, grouped and commented:
   - Release: `IMAGE_TAG` (for example `1.4.2`).
   - Registry: `GHCR_USER`, `GHCR_READ_TOKEN` (read:packages).
   - Host paths: `MEDIA_ROOT`, `LOG_ROOT`, `PLAYOUT_JOURNAL_ROOT`, `BACKUP_ROOT` (default `./backups`).
   - Database: `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB`, `DATABASE_URL` (built from the same values, or set explicitly).
   - Secrets: `ICECAST_SOURCE_PASSWORD`, `ICECAST_ADMIN_PASSWORD`, `ICECAST_RELAY_PASSWORD`, `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD`.
   - Network: `PANEL_BIND_ADDRESS` (default `127.0.0.1`, see 8.1), `PANEL_PORT` (default `8080`), `ICECAST_PUBLIC_PORT` (default `8000`).
   - Pinned infrastructure tags: `POSTGRES_IMAGE`, `ICECAST_IMAGE`, `LIQUIDSOAP_IMAGE`, `GRAFANA_IMAGE`, `LOKI_IMAGE`, `ALLOY_IMAGE`, so a change is an explicit commit.
   - Monitoring (8.12): `MONITORING_ENABLED` (`true` or `false`, default `true`), `GRAFANA_PORT` (default `3000`, always bound to `127.0.0.1`), `GRAFANA_ADMIN_PASSWORD`, `GRAFANA_DB_PASSWORD`.
   - Callback URL: `BACKEND_CALLBACK_URL` is fixed to `http://backend:8090/api/playout/callback` in the compose file, not in `.env` (it is a service-name fact, not a deployment choice).
5. `docs/`: the four context files copied from the Claude project (`base.md`, `tech-design.md`, `backend-init-plan.md`, `frontend-init-plan.md`) and this plan.
6. `README.md`: a short placeholder that points to the runbook section to be written in D5.

**Done when:** the repository has the layout of Section 2 without any running configuration; `git check-attr` shows `eol=crlf` for `*.bat` and `*.ps1`; `.env` is ignored.

## 4. Phase D2 — Compose stack

**Goal:** `docker compose config` renders the full production stack with pinned images, health checks, and correct dependencies.

Tasks:

1. `docker-compose.yml` with these services. Image tags come from `.env`.

   | Service | Image | Purpose | Published port | Mounts |
   |---------|-------|---------|----------------|--------|
   | `db` | `${POSTGRES_IMAGE}` (`postgres:17.x`, pinned minor) | Database | none | named volume `pgdata` |
   | `migrate` | `ghcr.io/astronin/radio-atlantida-backend:${IMAGE_TAG}` | Runs `backend migrate up` and exits (profile-less, run by the update script) | none | none |
   | `backend` | `ghcr.io/astronin/radio-atlantida-backend:${IMAGE_TAG}` | API, WebSocket, rotation, outage detection | none (reached through `frontend`) | `${MEDIA_ROOT}` ro, `${LOG_ROOT}` rw, `${PLAYOUT_JOURNAL_ROOT}` ro |
   | `liquidsoap` | `${LIQUIDSOAP_IMAGE}` (pinned 2.x, same tag as dev) | Playout, fallback, harbor, journal | none (control and harbor stay internal, design §8.9) | `${MEDIA_ROOT}` ro at `/media`, `${PLAYOUT_JOURNAL_ROOT}` rw at `/journal`, `./config/liquidsoap/radio.liq` ro |
   | `icecast` | `${ICECAST_IMAGE}` (`libretime/icecast:2.4.4-alpine`, backend plan §10.5) | Stream server | `${ICECAST_PUBLIC_PORT}` | none |
   | `frontend` | `ghcr.io/astronin/radio-atlantida-frontend:${IMAGE_TAG}` | Panel (nginx, proxies `/api` to `backend`) | `${PANEL_BIND_ADDRESS}:${PANEL_PORT}` → 8080 | none |
   | `grafana-role` | `${POSTGRES_IMAGE}` | Profile `monitoring`. Runs `psql -v ON_ERROR_STOP=1 -f /role.sql` as `POSTGRES_USER` against `db`, with `GRAFANA_DB_PASSWORD` passed as the session setting `radio.grafana_password` (`PGOPTIONS`), and exits (design §10.3) | none | `./config/grafana/role.sql` ro |
   | `grafana` | `${GRAFANA_IMAGE}` (Grafana OSS, pinned, same tag as dev) | Profile `monitoring`. Live database view and log search (design §10.3, §10.4) | `127.0.0.1:${GRAFANA_PORT}` → 3000 | named volume `grafana-data`, `./config/grafana/provisioning` ro, `./config/grafana/dashboards` ro |
   | `loki` | `${LOKI_IMAGE}` (pinned, same tag as dev) | Profile `monitoring`. Log storage (design §10.4) | none | named volume `loki-data`, `./config/loki/loki.yaml` ro |
   | `alloy` | `${ALLOY_IMAGE}` (pinned, same tag as dev) | Profile `monitoring`. Collects the log and journal files into `loki` (design §10.4) | none | named volume `alloy-data`, `${LOG_ROOT}` ro at `/logs`, `${PLAYOUT_JOURNAL_ROOT}` ro at `/journal`, `./config/alloy/config.alloy` ro |

2. Networks: one internal network `radio` for all services. Only `frontend`, `icecast`, and `grafana` have published ports; the `grafana` port is bound to `127.0.0.1` in the compose file, not through a variable.
3. Startup order:
   - `db` healthy (`pg_isready`).
   - `migrate` runs with `depends_on: db: condition: service_healthy` and exits `0`. `backend` depends on `migrate: condition: service_completed_successfully`.
   - `backend` healthy (`/healthz`) before `frontend` starts (`condition: service_healthy`).
   - `liquidsoap` depends on `icecast: condition: service_healthy`.
   - `grafana-role` depends on `db: condition: service_healthy`; `grafana` depends on `grafana-role: condition: service_completed_successfully` and `loki: condition: service_healthy`; `alloy` depends on `loki: condition: service_healthy`. No application service depends on a monitoring service.
4. Environment values for every service are set in the compose file through `environment:` with values from `.env`, the same way the dev stack does it (backend plan §3, task 9). The Liquidsoap startup values match the dev script exactly: `ICECAST_HOST=icecast`, `ICECAST_PORT=8000`, `ICECAST_MOUNT=/radio`, `TELNET_PORT=1234`, `HARBOR_PORT=8005`, `JOURNAL_DIR=/journal`, `BACKEND_CALLBACK_URL=http://backend:8090/api/playout/callback`.
5. Health checks:
   - `db`: `pg_isready`.
   - `backend`: `/healthz` on port 8090 (from inside the container).
   - `frontend`: `/` on port 8080.
   - `icecast`: `/status-json.xsl`.
   - `liquidsoap`: the telnet port answers.
   - `grafana`: `/api/health` on port 3000; `loki`: `/ready` on port 3100; `alloy`: `/-/ready` on port 12345 (each with a tool the pinned image contains; checked when the tag is chosen).
6. Restart policy `unless-stopped` for every long-running service. `migrate` and `grafana-role` have `restart: "no"`.
7. Log driver: `json-file` with `max-size: 10m` and `max-file: 3` for every container. Application logs go to `LOG_ROOT` (design §10.1), Docker logs are only a fallback.
8. `grafana` environment (design §10.3): `GF_SECURITY_ADMIN_USER=admin`, `GF_SECURITY_ADMIN_PASSWORD=${GRAFANA_ADMIN_PASSWORD}`, `GF_AUTH_ANONYMOUS_ENABLED=false`, `GF_USERS_ALLOW_SIGN_UP=false`, `GF_ANALYTICS_REPORTING_ENABLED=false`, `GF_ANALYTICS_CHECK_FOR_UPDATES=false`, `GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES=false`, `GF_NEWS_NEWS_FEED_ENABLED=false`, and `GRAFANA_DB_PASSWORD` and `POSTGRES_DB` for the data source file. The monitoring variables are interpolated with `:-` (empty allowed), so the stack renders with monitoring off; the preflight (D4, step 1) enforces them when it is on. `config/grafana/`, `config/loki/`, and `config/alloy/` are copies of `dev/grafana/`, `dev/loki/`, and `dev/alloy/` in `radio-atlantida-backend`; a change to either side is made in both (8.12).

**Done when:** `docker compose --env-file .env.example config` and `docker compose --env-file .env.example --profile monitoring config` render without errors and without any `latest` tag; a check in CI (D5) confirms that no published port other than `frontend`, `icecast`, and `grafana` appears, that the `grafana` port is bound to `127.0.0.1`, and that `loki` and `alloy` publish nothing.

## 5. Phase D3 — Production playout script and Icecast

**Goal:** the production Liquidsoap script and the Icecast setup, with the same interface the backend relies on.

Tasks:

1. `config/liquidsoap/radio.liq`: the production script. It uses the same mechanisms as the dev script (backend plan §3 task 9) and the production requirements of design §8:
   - a request queue fed through telnet (`request.push`), with the queue kept 2 to 3 items ahead by the backend;
   - `request.dynamic` only as the emergency source (design §8.2);
   - a safety source and `fallback` so the stream never stops (design §8.6), and an emergency playlist from the backend's settings;
   - a harbor input for live voice (`HARBOR_PORT`), higher priority than automation (design §8.4);
   - `music_gain` as a runtime variable set through telnet, plus automatic ducking inside Liquidsoap (design §8.5);
   - `fade_out_ms` applied at the out point of truncated tracks (design §8.1, §8.5);
   - `output.icecast` to the `icecast` service with the stream format from the database settings (codec, bitrate, sample rate, channels), passed in as environment values at startup (design §8.8);
   - a UTC line in the playout journal at each track start and end, and an HTTP callback to `BACKEND_CALLBACK_URL` that ignores failures (design §8.3, backend plan §3 task 9).
2. **Interface contract.** The script's telnet commands, callback payload, and journal line format are the contract between `radio-atlantida-backend` and this repository (backend plan §10.5, item 2). The file header lists the contract version, and CI checks that the header matches a constant in `scripts/contract-version.txt`. When the contract changes, the backend and this repository are released in the same change.
3. Icecast: configured only through environment variables in `docker-compose.yml`. No Icecast config file is needed. `config/icecast/README.md` records the variables, the chosen image tag and its digest (if pinned by digest, backend plan §10.5, item 3), and where the production stream settings are kept (in the database, edited in the panel, design §6.3, §8.8).
4. Liquidsoap version is the same pinned 2.x tag as the dev stack. A comment in `docker-compose.yml` points to `radio-atlantida-backend` for the chosen tag.
5. Test: a dry run of the script is a manual step on the production machine before the first air date (design §8.10 acceptance test). The repository documents the procedure in the runbook (D5), it does not automate it.

**Done when:** the script parses (`liquidsoap --check` run in a container in CI, using the pinned Liquidsoap image); the contract version header exists; the Icecast README lists every variable that `docker-compose.yml` sets for `icecast`.

## 6. Phase D4 — Update tooling (Windows)

**Goal:** the manager can update, roll back, and restore the production stack from a double-click, with the steps of design §9.2 in the same order.

Files: `update.bat` (entry point) and `update.ps1` (logic).

Tasks:

1. `update.bat`: a thin wrapper. It runs
   `powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0update.ps1" %*` and keeps the exit code. It pauses only when started by double-click, so the window does not disappear before the result is read.
2. `update.ps1` commands:
   - `update.bat` — update to the tag in `IMAGE_TAG` in `.env`.
   - `update.bat <tag>` — set `IMAGE_TAG` to `<tag>` and update. The tag is validated against `^\d+\.\d+\.\d+$`.
   - `update.bat rollback` — return to the tag recorded before the last update.
   - `update.bat restore` — restore the database from the dump recorded for the last update (`pg_restore --clean`). Shows the dump file name and its UTC time, and asks for confirmation by typing `RESTORE`.
   - `update.bat -DryRun` (any command) — prints every step and performs no change. Used for testing and for the first run.
3. Steps of an update, in order (design §9.2):
   1. Preflight: Docker Desktop running and the engine responds. Stop with a message otherwise. Do not install or upgrade Docker. When `MONITORING_ENABLED` is `true`, `GRAFANA_ADMIN_PASSWORD` and `GRAFANA_DB_PASSWORD` must be non-empty; otherwise stop before any change and name the missing variable (the values are never printed). Then create the host paths from `.env` (`MEDIA_ROOT`, `LOG_ROOT`, `PLAYOUT_JOURNAL_ROOT`, `BACKUP_ROOT`), and `backups/` and `logs/`, if they are missing. An existing folder is left as it is; an empty `MEDIA_ROOT` is valid on a fresh machine. If `.env` is missing, the first-run rules below apply; on a regular update a missing `.env` stops the script with a message to copy `.env.example`.
   2. Live check: call the backend's live status endpoint `GET /api/status/live` (backend plan, Phase 3, task 8). If on air, ask for confirmation. If an outage is open, stop and report it. If the endpoint does not answer (connection refused, timeout, or 5xx), stop and ask for confirmation: an unreachable endpoint is never read as off-air (§8.5). This step is skipped on the first run.
   3. Fetch this repository: `git pull --ff-only` when Git is available; otherwise download the `main` zip archive and overwrite the tracked files, keeping `.env`, `backups/`, `logs/`, and `state.json`. Show what changed.
   4. Record the current tag in `state.json` (previous tag, new tag, UTC time). `last_result` is `pending` until step 12. On the first run `previous_tag` is `null`.
   5. Log in to the registry with `GHCR_USER` and `GHCR_READ_TOKEN` from `.env`, if not already logged in. The token is passed through stdin, never as an argument.
   6. Pull images: `docker compose pull`.
   7. Backup, mandatory: `docker compose exec -T db pg_dump -Fc` into `backups/db-YYYY-MM-DDTHHMMSSZ.dump` (UTC). The update stops if the dump fails. The dump path is recorded in `state.json`. The script keeps the 14 most recent dumps. There is no flag to skip the backup. The only exception is the first run, which has no database to dump (see below).
   8. Migrate: `docker compose run --rm migrate`. On failure: stop, do not start the new `backend`, report, and keep the dump recorded for restore.
   9. Restart: `docker compose up -d`. Compose recreates only changed containers. This call never names the `monitoring` profile, so it does not start `grafana-role` or `grafana`.
   10. Health: wait for the health checks of `db`, `backend`, `liquidsoap`, and `frontend`, with a timeout (default 180 seconds).
   10a. Monitoring: when `MONITORING_ENABLED` is `true`, `docker compose --profile monitoring up -d --wait grafana-role loki alloy grafana` (same timeout); when it is `false`, `docker compose --profile monitoring stop grafana alloy loki` for those that run. A failure here is logged as a warning, sets `monitoring: "failed"` in the run log, and never triggers step 11: monitoring does not affect the air (design §9.2, §10.3).
   11. On failure after step 9: set `IMAGE_TAG` back to the recorded previous tag, run `docker compose up -d`, and report. Migrations are not rolled back. Because of the expand and contract rule (design §9.2), the previous code runs against the new schema. If the migration itself failed, the database is restored only through `update.bat restore`. On the first run there is no previous tag to return to: the script does not roll back, reports the failure, and leaves the containers as they are.
   12. Log the run: `logs/update-YYYY-MM-DDTHHMMSSZ.log` with the steps, the tags, the dump path, and the result. Secrets are never written to the log.
**First run (fresh machine).** The first run is any update where no successful run is recorded: `state.json` is absent, or it is present with `last_result` other than `"ok"` and `previous_tag` null. A failed first run is therefore repeated as a first run. Rules:
- If `state.json` is absent but a PostgreSQL data volume of this project exists, the script stops and reports. A machine with data and no state needs a manual decision, so the script never takes the first-run path on it.
- If `.env` is absent, the script generates it from `.env.example`. It creates random values for the database password, `ICECAST_SOURCE_PASSWORD`, and `LIQUIDSOAP_CONTROL_PASSWORD`, and asks for `GHCR_USER`, `GHCR_READ_TOKEN`, `IMAGE_TAG`, and the host paths. It refuses an empty value for any of them. Generated secrets and entered tokens are never printed or logged. With `-DryRun` it prints what it would generate and writes nothing.
- Steps 2 (live check) and 7 (backup) are skipped: nothing is on air and there is no database to dump. The log records both as `skipped: first run`.
- Steps 3 (fetch), 4 (state with `previous_tag: null`), 5 (registry login), and 6 (pull) run as usual.
- Before step 8, the script starts the database alone with `docker compose up -d db` and waits for its health check, using the timeout of step 10. Step 8 then runs the migrations.
- When `.env` is generated, `MONITORING_ENABLED` is `true` and the script also creates random values for `GRAFANA_ADMIN_PASSWORD` and `GRAFANA_DB_PASSWORD`. The Grafana admin password is shown once at the end of the first run, with a note to keep it; this is the only secret the script ever prints, because the manager needs it to log in.
- On failure there is no tag rollback and no restore. The script sets `last_result` to `failed`, reports the step that failed, and stops. Running `update.bat` again after the fix takes the first-run path again, because no successful run is recorded.

4. `state.json` schema (created by the script on the production machine, never committed):
   ```json
   {
     "current_tag": "1.4.2",
     "previous_tag": "1.4.1",
     "last_dump": "backups/db-2026-10-13T120000Z.dump",
     "last_update_utc": "2026-10-13T12:00:00Z",
     "last_result": "ok"
   }
   ```
5. Effect on air (design §9.2): the script warns before restarting `liquidsoap` or `icecast`, and restarts them only when their image or configuration changed. Restarting `backend` interrupts the panel and the voice session, not the playout (the queue is kept by Liquidsoap). The script never restarts `liquidsoap` silently.
6. Safety: the script works from any folder, including paths with spaces, and this is covered by a test in D5. It never runs `docker compose down -v`. It never deletes files outside `backups/` and `logs/`.

**Done when:** every command runs with `-DryRun` and prints the full step list; the Pester tests of D5 pass; a manual run on a Windows test machine with a second database and the dev media directory completes update, rollback, and restore. A first run on a clean Windows machine (no `.env`, no volumes, no `state.json`) completes, and the next `update.bat` takes the regular path.

## 7. Phase D5 — Verification, CI, runbook

**Goal:** every change to this repository is checked before it reaches the production machine.

Tasks:

1. `.github/workflows/ci.yml`, on every PR:
   - `docker compose --env-file .env.example config` succeeds, with and without `--profile monitoring`; no `latest` tag; no published port except `frontend`, `icecast`, and `grafana` (bound to `127.0.0.1`); every image is pinned or uses `${IMAGE_TAG}` for the two application images only.
   - Grafana provisioning: `config/grafana/dashboards/*.json` and the provisioning YAML files parse; every dashboard's data source uids match the provisioned ones.
   - Loki and Alloy configuration: `loki -config.file=config/loki/loki.yaml -verify-config` in the pinned Loki image; `alloy fmt config/alloy/config.alloy` in the pinned Alloy image (syntax check).
   - `liquidsoap --check config/liquidsoap/radio.liq` in the pinned Liquidsoap image.
   - PSScriptAnalyzer on `update.ps1` and `scripts/*.ps1`, with the default rules; a suppression needs a comment.
   - Pester tests in `tests/update.Tests.ps1`, run under Windows PowerShell 5.1 on a Windows runner, with `docker`, `git`, and `Invoke-WebRequest` mocked. Cases: update happy path, dump failure stops the update, migration failure keeps the dump and does not start `backend`, health timeout triggers rollback of `IMAGE_TAG`, restore asks for confirmation, invalid tag is refused, `-DryRun` changes nothing, first run skips the live check and the backup, first run with a volume and no `state.json` stops, a failed first run does not roll back, an unreachable live endpoint on a regular update stops, missing Grafana passwords with monitoring enabled stop in preflight, a monitoring failure in step 10a is a warning and does not roll back, monitoring disabled skips step 10a.
   - `scripts/check-line-endings.ps1`: fails if `*.bat` or `*.ps1` files do not have CRLF, or other text files have CRLF.
   - Contract version: the header of `radio.liq` matches `scripts/contract-version.txt`.
2. `README.md` (runbook), sections:
   - Prerequisites on the production machine: Windows 10 or 11, Docker Desktop with WSL2, NTP enabled, Git (optional), the GHCR read token, and the media directory with the tracks.
   - First installation: run `update.bat -DryRun` to see the first-run plan, then `update.bat`. When `.env` is missing, the script creates it from `.env.example` and asks for the GHCR credentials, the image tag, and the host paths (D4, first run).
   - Regular update, rollback, and restore, with the warnings about air.
   - Where the logs, dumps, and the playout journal are, and how they are backed up (design §10).
   - Firewall: allow the panel port only on the private network (design §10).
   - Monitoring: open `http://127.0.0.1:3000` (or `GRAFANA_PORT`), log in as `admin`; what the "Database" and "Logs" dashboards show, and that the log files on the host stay the source (Loki keeps a 10-day searchable copy); turn monitoring off and on with `MONITORING_ENABLED`; reset a lost admin password with `docker compose exec grafana grafana cli admin reset-admin-password <new>`; change `GRAFANA_DB_PASSWORD` by editing `.env` and running `update.bat`.
   - Troubleshooting: Docker not running, registry login failed, health timeout, Liquidsoap not connected to Icecast.
3. A manual acceptance checklist in `docs/` is not added in v1. Acceptance is the procedure in the runbook.

**Done when:** CI is green on a PR; the runbook lets someone who has never seen the system install and update it.

## 8. Decisions and open points

### 8.1 Panel exposure — DECIDED

The manager works only on the production machine, so the panel is not published to the LAN. `PANEL_BIND_ADDRESS` defaults to `127.0.0.1`, and the panel is opened from that machine. The Icecast listener port stays published, because listeners must reach the stream. Changing the panel binding to a LAN address is a deliberate later decision, and it requires a firewall rule and a review of the missing authentication (design §10).

### 8.2 Liquidsoap production script — DEFAULT

The production script lives in this repository (design §1.1, backend plan §10.5, item 2). It is written against the same contract version as the dev script, and both are updated in the same release.

### 8.3 Icecast image — DEFAULT

`libretime/icecast:2.4.4-alpine`, the same as the dev stack (backend plan §10.5, item 3). Pinned by tag. Pinning by digest is a later hardening step, recorded in `config/icecast/README.md`.

### 8.4 Line endings for scripts — DECIDED

`*.bat` and `*.ps1` use CRLF. This overrides the repository default of LF (design §9.1 requires line endings to be checked in the pipeline). Reason: `cmd.exe` mishandles LF-only batch files.

### 8.5 Live status endpoint — DECIDED

`GET /api/status/live` is added to the backend plan as Phase 3, task 8. `update.ps1` uses it in the live check (D4, step 2). Until the Liquidsoap and outage phases exist, the endpoint returns `on_air: null` and `open_outage_id: null`. `null` means unknown: the script asks for confirmation as if the station were on air, and it never treats `null` as off-air.

### 8.6 Update without arguments — DEFAULT

`update.bat` with no arguments updates to `IMAGE_TAG` from `.env`. It does not fetch "the latest tag" from the network, so the manager always knows which release is installed (design §9.2 says "or the latest tag from the deploy repository, if set"; this plan drops the second option for predictability).

### 8.7 Git on Windows — DEFAULT

Git is optional. Without it the update downloads the `main` archive (step 3 of D4). Git for Windows is not installed by the scripts (design §9.2: the installers are manual).

### 8.8 PowerShell version — DECIDED

Windows PowerShell 5.1, the version built into Windows 10 and 11. No PowerShell 7 requirement.

### 8.9 Application state in the deploy repository — DECIDED

The deploy repository holds no database, media, or journal data. `backups/`, `logs/`, `state.json`, and `.env` live on the production machine only.

### 8.10 Migrate service in the dev stack — DEFAULT

The backend plan adds the `migrate` service to the dev compose file in Phase 2 (backend plan §3 task 9). This repository is the production definition, so `migrate` is defined here from the start.

### 8.11 Registry access — DECIDED

The production machine pulls images from `ghcr.io/astronin` with a read-only token (`read:packages`). The token is entered only into the production `.env` as `GHCR_READ_TOKEN` (D1) and is never written to this repository, to a plan, or to a log. The token was shared in plain text during planning, so it must be treated as exposed: revoke it and issue a fine-grained token limited to read access for the `radio-atlantida-*` packages, then use only the new one. The expiry date of the new token is recorded in the runbook (D5).

### 8.12 Database monitoring — DECIDED

An optional Grafana (design §10.3) shows the live state of PostgreSQL. It is on by default (`MONITORING_ENABLED=true`) and can be switched off to save memory. It requires a login (admin password in `.env`), is bound to `127.0.0.1` only, and reads the database through the read-only role `grafana_ro`, which the one-shot `grafana-role` service keeps in step with `.env`. Loki and Alloy (design §10.4) under the same switch add search over the application logs and the playout journal, collected from the host files, kept 10 days, not published, no alerts. There is no metrics history (no Prometheus). The files in `config/grafana/`, `config/loki/`, and `config/alloy/` are copies of `dev/grafana/`, `dev/loki/`, and `dev/alloy/` in `radio-atlantida-backend`, where the dashboards and configuration are developed and tested first (`docs/phase-14-plan.md` and `docs/phase-15-plan.md` in that repository); both are changed together. Monitoring is outside the update's health gate: its failure is a warning, never a rollback.

## 9. Prompt for the first Claude Code session

Before the session, copy `base.md`, `tech-design.md`, `backend-init-plan.md`, `frontend-init-plan.md`, and `deploy-init-plan.md` into `docs/` of the repository.

> You are starting the deployment repository of a radio automation system. It contains no application code.
>
> 1. Read `docs/base.md` for the goal of the product.
> 2. Read `docs/tech-design.md`, especially §1.1, §3.1, §8.7–8.9, §9, and §10. It is the authority.
> 3. Read `docs/backend-init-plan.md` (§3 task 9, §10.5) and `docs/frontend-init-plan.md` (§3 task 9) for the service names and the dev stack that production must match.
> 4. Read `docs/deploy-init-plan.md`. It is the task list.
>
> Implement Phase D1 of `docs/deploy-init-plan.md`. Follow Section 1 and 1.1. Apply the DEFAULT items of Section 8. For ASK items (8.1, 8.5, 8.11), do not guess: list them in your final message and stop before the phase that needs them. Do not start D2. Finish with `git check-attr` confirming the line endings, `.env` ignored, and a short summary of what was created.
