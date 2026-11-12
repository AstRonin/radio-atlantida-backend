# Deploy Initialization Plan (radio-atlantida-deploy)

Status: Draft for Claude Code (rewritten to match the current design and code; host on Docker Engine in WSL2)
Based on: `tech-design.md` v7-25 (§1.1, §3.1, §6.3, §7.1, §8, §9, §10), `base.md`, backend plans `backend-init-plan.md`, `phase-12-plan.md` (live voice), `phase-13-plan.md` (release pipeline), `phase-21-plan.md` (stream metadata, mount charset), `phase-22-plan.md` (cue fades), `safety-on-air-state-plan.md`, `logs-loki-plan.md`; frontend plans `frontend-init-plan.md`, `f12-release-plan.md`, `f13-grafana-link-plan.md`
Last updated: 2026-11-12

## 0. How to use this plan

- **Context documents.** The central copies live in `radio-atlantida-docs`: shared documents at the root (`base.md`, `tech-design.md`), and per-repository plans in `radio-atlantida-docs/backend/docs/`, `radio-atlantida-docs/frontend/docs/`, and `radio-atlantida-docs/deploy/docs/`. The `docs` sync agent pushes the copies this repository needs into `radio-atlantida-deploy/docs/`: `base.md`, `tech-design.md`, `backend-init-plan.md`, `frontend-init-plan.md`, and this plan. Before any work, Claude Code reads them:
  - `docs/base.md`: product requirements.
  - `docs/tech-design.md`: the authority on how the system is built (v7-25 or later). §9.1 and §9.2 are the specification for this repository.
  - `docs/backend-init-plan.md`, `docs/frontend-init-plan.md`: the build conventions.
  - `docs/deploy-init-plan.md`: this plan.
- The **source of truth for the dev stack** is the code of `radio-atlantida-backend` (public repository), not a plan: `docker-compose.dev.yml`, `dev/liquidsoap/radio.liq`, `dev/grafana/`, `dev/loki/`, `dev/alloy/`. File references below (`file:line`) are to the backend at commit `5856a45` and the frontend at its current `main`, both untagged so far.
- This repository contains **no application code**. It runs prebuilt images from GitHub Container Registry and holds the stack definition, the production Liquidsoap script, configuration copies, the Windows host installer, and the update tool.
- Work one phase at a time. Each phase ends with a passing validation run. Commits and PRs are made by the developer (one PR per phase).
- Section 11 lists decisions and open points. **ASK** items must be confirmed by the developers before the phase that needs them. **DEFAULT** items are applied unless the developers say otherwise.
- Section 12 lists **cross-repo tasks** (B1, B6, and B9 matter most). Phases that depend on one say so; they must not be worked around in this repository.
- Every statement about Windows and WSL behaviour that could not be tested on the developer's Mac is marked **(verify on Windows)**; each one is part of the Windows acceptance run in D4.
- Scope of initialization: Phases D1 to D7. They produce a complete, validated deploy repository, a tested installer, and a dry run. Running the installer on the production machine is the first step of the runbook (D7).

## 1. Ground rules

- **No application code.** No Go, no Angular, no build steps for them. Application images are referenced by tag and digest: `ghcr.io/astronin/radio-atlantida-backend:${BACKEND_TAG}@${BACKEND_DIGEST}` and `ghcr.io/astronin/radio-atlantida-frontend:${FRONTEND_TAG}@${FRONTEND_DIGEST}`. The values come from the committed `release.env` (§10.1), from the command line (`update.bat <tag>`), or from `state.json` on a rollback.
- **Host: Docker Engine in WSL2, no Docker Desktop.** The production machine runs Windows 10 (21H2, build 19044, or later; most likely Home without ESU, 11.17); Windows 11 is supported the same way and recommended later (6.1). Docker Engine and the Compose plugin run in a dedicated WSL2 distribution `radio-atlantida` (pinned Ubuntu LTS, systemd enabled), set up by `install.bat` (D4). A scheduled task of a dedicated local Windows account starts and keeps the distribution running from boot, without a sign-in.
- **Infrastructure images are pinned** to exact tags, written literally in `docker-compose.yml` (not in `.env`, so a change is a reviewed commit). No `latest` anywhere. The tags equal the backend dev stack at the pinned backend tag, checked in CI (D7): `postgres:17.11` (`docker-compose.dev.yml:23`, `:152`), `savonet/liquidsoap:v2.4.6` (`:57`), `grafana/grafana:13.2.3` (`:175`), `grafana/loki:3.7.8` (`:220`), `grafana/alloy:v1.20.1` (`:234`). Docker Engine, the Compose plugin, the Ubuntu image, and the minimum WSL version are pinned in `install/versions.env` (D4).
- **No Icecast container.** Production streams to an **external Icecast server**: `https://listen.teploe.net:8001/atlantida` (Icecast 2.4.4, TLS only on port 8001; 5.4). Its connection (host, port, mount, TLS, codec, bitrate, sample rate, channels, stream name, description, genre) is the backend's `stream.*` settings, edited in the panel under "Stream output (Icecast)" (`internal/settings/definitions.go:188-200`). Only the source password is in `.env`. The dev stack keeps its own Icecast test container.
- **Secrets never in git.** `.env` is git-ignored. `.env.example` lists every variable with a placeholder and a comment. Secrets: `POSTGRES_PASSWORD`, `ICECAST_SOURCE_PASSWORD` (of the external server), `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD`, `GHCR_READ_TOKEN`, `GRAFANA_ADMIN_PASSWORD`, `GRAFANA_DB_PASSWORD`. The Windows account password is entered once for the scheduled tasks and stored only by Windows Task Scheduler.
- **Data locations** (D4, 6.6): the media directory stays in the station's existing **Windows folder** (for example `D:\RadioAtlantida\media`), seen in WSL through the Windows drive mount (`MEDIA_ROOT=/mnt/d/RadioAtlantida/media`); the playout journal, the backups, and the deploy repository live on the distribution's ext4 file system (`/srv/radio/journal`, `/srv/radio/backups`, `/opt/radio-atlantida/deploy`). Media files are never inside a container or a named volume (design §8.7). The database is the named volume `pgdata` of that Docker Engine. Data moves one way only, production to development, by restoring a dump (D6); there is no live replication.
- **Network exposure.** The panel (`frontend`) and Grafana are bound to `127.0.0.1` in the compose file (no variable can widen them); Windows reaches them at `127.0.0.1` through WSL's localhost forwarding (Windows 10, NAT mode) or mirrored networking (Windows 11 22H2+) (6.5). The live voice UDP port of `backend` is bound to `${LIVE_UDP_BIND}:8189:8189/udp`, where `LIVE_UDP_BIND` is written only by the tool: `127.0.0.1` in mirrored mode, `0.0.0.0` in NAT mode, where the WSL VM is reachable from the Windows host only (6.5). Nothing else is published: not `db`, not `liquidsoap` (control and harbor stay internal, design §8.9), not `loki`, not `alloy`. **No listener port** is published: listeners connect to the external server. The panel and live voice are localhost-only (11.1).
- **Forward-only migrations.** The tooling never runs `migrate down` and never restores a database without the manager's typed confirmation (design §9.2).
- **The update tool is the only way to change the production stack.** Manual `docker compose` commands are for diagnostics only (`update.bat compose ps`), because the tool records the state needed for rollback and restore.
- **Liquidsoap is restarted only in a maintenance window** (11.5). A normal update never restarts `liquidsoap` (or `db`); it reports the pending change, and `update.bat maintenance` applies it after confirmation. Stream output settings do not need a restart: they apply at runtime over the control link (D3).
- **Line endings.** The repository default is LF (`* text=auto eol=lf`); `*.bat` and `*.ps1` are stored with CRLF (11.8). Shell scripts must stay LF: bash fails on CRLF.

### 1.1 Conventions (rules for the AI)

**Compose**

- Compose Specification, no top-level `version:` key. Fixed project name: `name: radio-atlantida` (the dev stack is `radio-atlantida-dev`, `docker-compose.dev.yml:19`), so the volume names (`radio-atlantida_pgdata`, ...) do not depend on the folder name.
- Every service has an explicit `image:` and `restart: unless-stopped`, except the one-shot jobs `migrate` and `grafana-role`, which have `restart: "no"` and `healthcheck: disable: true` (their images carry a server health check that a job never passes; dev does the same, `docker-compose.dev.yml:105-108`, `:162-165`). With `unless-stopped`, Docker starts the containers again when the engine starts at boot. No service builds locally.
- Health checks: `db` (`pg_isready`), `backend` (the image's `HEALTHCHECK` on `/healthz`, `docker/Dockerfile:40-41`), `frontend` (the image's `HEALTHCHECK`, frontend `docker/Dockerfile:29-30`), `liquidsoap` (telnet `quit`/`Bye`, as `docker-compose.dev.yml:86-92`), `grafana` (`/api/health`), `alloy` (`/-/ready` through bash, as `:246-252`). `loki` has none (distroless image, design §9).
- Dependencies use `depends_on` with `service_healthy`, `service_completed_successfully`, or, for `loki` only, `service_started`. `frontend` has **no** dependency on `backend`: nginx resolves `backend` per request through Docker's DNS (frontend `docker/default.conf.template:16-19`), so the panel starts and answers `/api` with 502 until the backend is up. `liquidsoap` has no dependency either: it plays from its queues, its emergency source, and the safety tone without the backend.
- Named volumes: `pgdata` (backed up by dumps), `grafana-data`, `loki-data`, `alloy-data`, `liquidsoap-state` (the last stream output that connected, D3; rebuildable). Everything else is a bind mount of a host path from `.env` or a read-only file of this repository.
- Service names equal the dev stack, minus `icecast`: `db`, `liquidsoap`, `migrate`, `backend`, `frontend`, `grafana-role`, `grafana`, `loki`, `alloy`. The backend settings defaults (`liquidsoap.control_host=liquidsoap`, `definitions.go:214`), the frontend nginx (`backend:8090`), and the Liquidsoap URLs depend on them.
- Logs: `json-file` with `max-size: 10m`, `max-file: 3` for every container. The application log goes to Loki (design §10.1); Docker's copy is a fallback.
- Resource limits are not set in v1 (DEFAULT); the WSL VM has a memory limit (6.4).

**Bash** (`bin/radioctl`, `install/*.sh`)

- Bash 5, `set -Eeuo pipefail`, `shellcheck` clean, small functions named as verbs, `"$var"` quoting everywhere.
- Secrets are never printed or logged; `.env` is read with a parser that does not `source` it (no code execution from `.env`). The GHCR token goes to `docker login --password-stdin`.
- Every external command's output goes to the run log; the console shows the steps and the result.
- No interactive prompt without a `--yes`-style non-interactive answer for tests; destructive steps always need the typed word (`RESTORE`, `MAINTENANCE`, `CUTOVER`), which tests pass on stdin.

**PowerShell** (`install.ps1`, the wrappers)

- Windows PowerShell 5.1; PowerShell 7 must not be required.
- `Set-StrictMode -Version Latest` and `$ErrorActionPreference = 'Stop'`. External commands (`wsl.exe`, `dism.exe`, `powercfg.exe`) are checked through `$LASTEXITCODE`. `wsl.exe` writes UTF-16 for some commands; the script sets `WSL_UTF8=1` before calling it.
- Native command output is captured into the install log; secrets are never printed or logged.
- Small functions named as verbs, no aliases, paths built with `Join-Path`. Files saved as UTF-8 with BOM. PSScriptAnalyzer in CI, no suppressions unless commented.

## 2. Repository layout

```
radio-atlantida-deploy/
  .github/workflows/ci.yml            # every PR: compose, sync with backend dev files, Liquidsoap, scripts, tests (D7)
  release.env                         # BACKEND_TAG, BACKEND_DIGEST, FRONTEND_TAG, FRONTEND_DIGEST (committed, §10.1)
  install.bat                         # entry point of the host install: runs install.ps1 elevated
  install.ps1                         # Windows half of the install (D4)
  install/
    versions.env                      # pinned Ubuntu rootfs URL + SHA-256, Docker Engine and Compose plugin apt versions, Docker apt key fingerprint, minimum WSL version
    setup-distro.sh                   # Linux half of the install, run as root inside the distribution (D4)
    wsl.conf                          # /etc/wsl.conf of the distribution
    wslconfig.template                # %UserProfile%\.wslconfig of the radio account
    systemd/                          # radio-backup.service/.timer, radio-boot-check.service
    radio-keepalive                   # /usr/local/bin/radio-keepalive (keeps the distribution running)
  update.bat                          # thin wrapper: wsl.exe -d radio-atlantida ... bin/radioctl %*
  bin/radioctl                        # update, rollback, restore, maintenance, backup, cutover, status, first-run (D5, D6)
  config/
    liquidsoap/
      radio.liq                       # production script = backend dev/liquidsoap/radio.liq at v${BACKEND_TAG} + production.patch (D3)
      production.patch                # the production overrides, possibly empty (D3)
    grafana/                          # = backend dev/grafana/ at v${BACKEND_TAG} (role.sql, provisioning/, dashboards/)
    loki/loki.yaml                    # = backend dev/loki/loki.yaml
    alloy/config.alloy                # = backend dev/alloy/config.alloy
  docs/                               # pushed copies (§0)
  docker-compose.yml                  # the production stack (D2)
  .env.example                        # every per-machine variable, placeholders, comments
  tests/
    install.Tests.ps1                 # Pester tests of install.ps1 with wsl.exe, dism, powercfg, Task Scheduler mocked (D7)
    radioctl/*.bats                   # bats tests of radioctl with docker, git, curl mocked (D7)
    liquidsoap/                       # runtime smoke test of radio.liq (D3, D7)
  scripts/
    check-line-endings.ps1
    check-backend-sync.sh             # CI: compares config/ and image pins with the backend at v${BACKEND_TAG}
  .gitattributes  .gitignore  .editorconfig
  README.md                           # runbook (D7)
```

Runtime files, created by the scripts and never committed, all inside the distribution under `/opt/radio-atlantida/deploy`: `.env`, `state.json`, `state.lock`, `runtime/applied.env` (the tags, digests, and derived values of the running stack), `logs/`. Backups are under `BACKUP_ROOT` (`/srv/radio/backups`) with a copy on the Windows drive (D6). The installer's own state and log are on Windows: `C:\ProgramData\RadioAtlantida\install-state.json` and `C:\ProgramData\RadioAtlantida\logs\`.

## 3. Phase D1 — Skeleton and conventions

**Goal:** structure, ignore rules, line endings, the environment template, and the release file. Nothing runs yet.

Tasks:

1. `.gitignore`: `.env`, `state.json`, `state.lock`, `runtime/`, `backups/`, `logs/`, `*.dump`, `.DS_Store`, `Thumbs.db`.
2. `.gitattributes`:
   ```
   * text=auto eol=lf
   *.bat text eol=crlf
   *.ps1 text eol=crlf
   *.sh text eol=lf
   bin/* text eol=lf
   *.dump binary
   ```
3. `.editorconfig`: UTF-8, LF by default, CRLF for `*.bat` and `*.ps1`, final newline, 2 spaces for YAML.
4. `.env.example`, grouped and commented:
   - Registry: `GHCR_USER`, `GHCR_READ_TOKEN` (fine-grained, read-only, 11.10).
   - Host paths (Linux paths inside the distribution): `MEDIA_ROOT=/mnt/d/RadioAtlantida/media` (the Windows media folder as WSL sees it, written by the installer from the folder the operator chooses, 6.6), `PLAYOUT_JOURNAL_ROOT=/srv/radio/journal`, `BACKUP_ROOT=/srv/radio/backups`, `BACKUP_WINDOWS_COPY=/mnt/c/RadioAtlantida/backups` (empty disables the copy).
   - Database: `POSTGRES_USER`, `POSTGRES_PASSWORD` (letters and digits only, because it is embedded in `DATABASE_URL`), `POSTGRES_DB`. `DATABASE_URL` is built in the compose file, as in dev (`docker-compose.dev.yml:104`).
   - Stream and control secrets: `ICECAST_SOURCE_PASSWORD` (the external server's source password, entered by hand, never generated), `LIQUIDSOAP_CONTROL_USER` (default `source`), `LIQUIDSOAP_CONTROL_PASSWORD`.
   - Panel: `PANEL_PORT` (default `8080`, always on `127.0.0.1`).
   - Monitoring (11.11): `MONITORING_ENABLED` (default `true`; switches Grafana only), `GRAFANA_PORT` (default `3000`, always on `127.0.0.1`), `GRAFANA_ADMIN_PASSWORD`, `GRAFANA_DB_PASSWORD`.
   - Backups: `BACKUP_DAILY_KEEP` (14), `BACKUP_UPDATE_KEEP` (14), `BACKUP_BOUNDARY_KEEP` (3).
   - Not in `.env` (fixed in the compose file, because they are facts of the service names and port mapping, not deployment choices): `HTTP_ADDR`, `LIVE_UDP_ADDR`, `LOKI_URL`, `BACKEND_CALLBACK_URL`, `BACKEND_EMERGENCY_URL`, `BACKEND_OUTPUT_URL`, all image pins. The stream server (host, port, mount, TLS, format) is not in `.env` either: it is a backend setting (D4, 6.8). `GRAFANA_URL` is derived by the tool (D2 task 4). `LIVE_UDP_BIND` and `LIVE_PUBLIC_IPS` are written by the tool and the boot unit into `runtime/network.env` (6.5).
5. `release.env` with placeholders until the first release (`BACKEND_TAG=1.0.0`, digests empty). CI fails on an empty digest from D7 on.
6. `install/versions.env` with placeholders and a comment per value (filled in D4).
7. `docs/`: filled by the `docs` sync agent (§0), not by hand.
8. `README.md`: a placeholder pointing to the runbook written in D7.

**Done when:** the layout of Section 2 exists without running configuration; `git check-attr` shows `eol=crlf` for `*.bat` and `*.ps1` and `eol=lf` for `bin/radioctl` and `*.sh`; `.env` is ignored.

## 4. Phase D2 — Compose stack

**Goal:** `docker compose config` renders the full production stack with pinned images, health checks, the correct environment, and the allowed ports only.

Tasks:

1. `docker-compose.yml`, `name: radio-atlantida`, one internal network `radio`:

   | Service | Image | Purpose | Published port | Mounts |
   |---------|-------|---------|----------------|--------|
   | `db` | `postgres:17.11` | Database | none | named volume `pgdata` |
   | `migrate` | backend `${BACKEND_TAG}@${BACKEND_DIGEST}` | `command: ["migrate", "up"]`, exits. Only `DATABASE_URL` (`internal/config/config.go:78-83`) | none | none |
   | `backend` | backend `${BACKEND_TAG}@${BACKEND_DIGEST}` | API, WebSocket, rotation, outage detection, live voice bridge, Loki push | `${LIVE_UDP_BIND}:8189:8189/udp` (live voice, 6.5) | `${MEDIA_ROOT}:/media` **read-write** (transcode and replace write files, design §8.7), `${PLAYOUT_JOURNAL_ROOT}:/journal:ro` |
   | `liquidsoap` | `savonet/liquidsoap:v2.4.6` | Playout, fallback, harbor, journal, output to the external Icecast | none | `${MEDIA_ROOT}:/media:ro`, `${PLAYOUT_JOURNAL_ROOT}:/journal`, `./config/liquidsoap/radio.liq:/etc/liquidsoap/radio.liq:ro`, `liquidsoap-state:/var/cache/liquidsoap` |
   | `frontend` | frontend `${FRONTEND_TAG}@${FRONTEND_DIGEST}` | Panel (nginx, proxies `/api` to `backend`) | `127.0.0.1:${PANEL_PORT}:8080` | none |
   | `grafana-role` | `postgres:17.11` | Profile `monitoring`. `psql -v ON_ERROR_STOP=1 -q -f /role.sql` as in dev (`:150-168`) | none | `./config/grafana/role.sql:/role.sql:ro` |
   | `grafana` | `grafana/grafana:13.2.3` | Profile `monitoring`. Database view and log search | `127.0.0.1:${GRAFANA_PORT}:3000` | `grafana-data`, `./config/grafana/provisioning` ro, `./config/grafana/dashboards` ro |
   | `loki` | `grafana/loki:3.7.8` | Core service. Log storage | none (dev publishes 3100; production does not) | `loki-data`, `./config/loki/loki.yaml` ro |
   | `alloy` | `grafana/alloy:v1.20.1` | Core service. Collects the playout journal | none (dev publishes 12345; production does not) | `alloy-data`, `${PLAYOUT_JOURNAL_ROOT}:/journal:ro`, `./config/alloy/config.alloy` ro |

2. `backend` environment (as dev, `docker-compose.dev.yml:116-130`, with production values):
   - `DATABASE_URL=postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@db:5432/${POSTGRES_DB}?sslmode=disable`
   - `MEDIA_ROOT=/media`, `PLAYOUT_JOURNAL_ROOT=/journal`
   - `HTTP_ADDR=0.0.0.0:8090` (the code default is `127.0.0.1:8090`, `config.go:39`, unreachable from nginx; the image health check assumes port 8090, `Dockerfile:39`)
   - `LOKI_URL=http://loki:3100`
   - `LIVE_UDP_ADDR=0.0.0.0:8189`, `LIVE_PUBLIC_IPS=${LIVE_PUBLIC_IPS:?written by radioctl}`: `127.0.0.1` in mirrored mode, the WSL VM's `eth0` address in NAT mode (6.5; `config.go:44-48`, phase-12 decision 4)
   - `ICECAST_SOURCE_PASSWORD`, `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD` from `.env`
   - `group_add: ["10001"]`: the media folder's files appear in WSL with group `radio-media` (gid 10001) and group write (6.6), so the backend user (uid 10001, `Dockerfile:34`) can write fixed files there.
   - The media mount uses the long syntax with `bind: create_host_path: false` (also on `liquidsoap`): if the Windows drive is not mounted, the container fails to start instead of seeing an empty media folder created by Docker.
   - `stop_grace_period: 30s`: the backend drains HTTP for 15 s and then waits up to 10 s more for its components (`cmd/backend/serve.go:57`, `cmd/backend/shutdown.go:14`), 25 s in total; Docker's default of 10 s would kill it mid-drain.
3. `liquidsoap` environment: `TZ=UTC`, `ICECAST_SOURCE_PASSWORD`, `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD`, `TELNET_PORT=1234`, `HARBOR_PORT=8005`, `JOURNAL_DIR=/journal`, `BACKEND_CALLBACK_URL=http://backend:8090/api/playout/callback`, `BACKEND_EMERGENCY_URL=http://backend:8090/api/playout/emergency/next`, `BACKEND_OUTPUT_URL=http://backend:8090/api/playout/output` (D3; needs cross-repo task B1), `OUTPUT_STATE_FILE=/var/cache/liquidsoap/output.json`, `ICECAST_TLS=true`. The state file lives in the image's own `/var/cache/liquidsoap`, as in dev (`docker-compose.dev.yml`, backend commit e2b9178): the container runs as user `liquidsoap`, and a new named volume takes the owner of the directory it is mounted over, so over a directory the image lacks (`/var/lib/radio`) it would be root's and the script could not write the file. The script's built-in default of `OUTPUT_STATE_FILE` is still `/var/lib/radio/output.json` (`radio.liq:60`), so the variable must be set. The dev defaults point to `host.docker.internal` (`docker-compose.dev.yml:70-71`), so the production values must be set explicitly. No `ICECAST_HOST/PORT/MOUNT`: they come from the backend settings (D3); without the backend and without a state file the script falls back to its environment and the settings defaults (`icecast:8000/radio`), which do not exist in production, so it keeps retrying until the backend answers. `ICECAST_TLS=true` only makes that fallback match the production server's TLS. The control port and harbor are not published.
4. `frontend` environment: `GRAFANA_URL=${GRAFANA_URL:-}`. The tool sets it in `runtime/applied.env`: `http://127.0.0.1:${GRAFANA_PORT}` when `MONITORING_ENABLED=true`, empty otherwise (the panel then shows no link; frontend `Dockerfile:22-25`, `f13-grafana-link-plan.md`).
5. `grafana` environment as dev (`:177-188`): admin user and password, anonymous and sign-up off, analytics, update checks, and news feed off, `GRAFANA_DB_PASSWORD`, `POSTGRES_DB`. Monitoring variables are interpolated with `:-`, so the stack renders with monitoring off; the preflight enforces them when it is on.
6. Startup order (first start): `db` healthy → `migrate` completed → `backend`. `grafana-role` after `db` healthy; `grafana` after `grafana-role` completed and `loki` started; `alloy` after `loki` started. No application service depends on a monitoring service. At boot Docker starts the containers by their restart policy without `depends_on`; the backend exits and is restarted if the database is not ready yet, which is acceptable (checked by the reboot test, D4).
7. Image references: `image: ghcr.io/astronin/radio-atlantida-backend:${BACKEND_TAG:?run through radioctl}@${BACKEND_DIGEST:?run through radioctl}` (likewise frontend). The digest is that of the multi-arch manifest list (§10.1), so the same reference pulls amd64 on the station and arm64 on the developer's Mac. Compose is always called by `radioctl` with `--env-file .env --env-file runtime/applied.env --env-file runtime/network.env`; `update.bat compose <args>` does the same for diagnostics.

**Done when:** `docker compose --env-file .env.example --env-file <fixture applied.env> config` renders with and without `--profile monitoring`, with no `latest` tag and no `icecast` service; the CI port rule (D7) passes: the only published ports are `127.0.0.1:${PANEL_PORT}->8080/tcp` (frontend), `${LIVE_UDP_BIND}:8189->8189/udp` (backend; the fixtures render it with `127.0.0.1` and with `0.0.0.0`), and `127.0.0.1:${GRAFANA_PORT}->3000/tcp` (grafana).

## 5. Phase D3 — Production playout script and external Icecast

**Goal:** the production Liquidsoap script, derived mechanically from the backend dev script, and the stream output to the external Icecast server driven by the backend settings, applied at runtime.

**Prerequisite:** cross-repo tasks B1 (runtime output configuration), B2 (contract header), and B6 (TLS). Tasks 1 to 3 can be prepared before them; the phase is done only when the dev script at the pinned backend tag has the runtime output mechanism.

### 5.1 The script contract (current, `dev/liquidsoap/radio.liq`)

The script is the interface between the backend and this repository. The production script must keep all of it:

- two request queues `hour_0` and `hour_1`, fed with `hour_<p>.push <uri>` over telnet; a time switch plays the queue of the current station hour by `floor((t - hour_shift) / 3600)` parity; the queue an hour leaves is flushed and a cut track reports its end (`radio.liq:170-214`);
- request annotations `item_id`, `title`, `artist`, `liq_cue_in`, `liq_cue_out`, `liq_fade_in`, `liq_fade_out` (design §8.1); a custom cue fade in reading `liq_fade_in` from the track metadata, and `fade.out(..., override_duration="liq_fade_out", track_sensitive=true)` on the hour queues and on the emergency source (`:227-256`, `:280-289`; with the 2.4 default `track_sensitive=false` the queues go silent, phase-22 §10);
- the emergency source: `request.dynamic` asking `BACKEND_EMERGENCY_URL` (200 with a URI, or 204/error: nothing) (`:261-278`);
- `music_gain`, `duck_gain`, `hour_shift` as interactive variables set by the backend; ducking with `smooth_add(duration=0.5, p=duck_gain, ...)` while the harbor mount `live` has a source (`:291-342`);
- the safety source (quiet sine) behind `fallback` (`:300-301`);
- the playout journal `journal-YYYY-MM-DD.jsonl` (UTC) and an HTTP callback on every track start and end, and on every output connect and disconnect; journal files older than 10 days are deleted by the script (`:59-160`);
- a heartbeat callback every 5 s with `safety_on_air = not source.is_ready(music)` (`:308-321`, `safety-on-air-state-plan.md`);
- the harbor input `live` with the control credentials, buffer 0.3 s (`:327-336`);
- **today** a single `output.icecast` with a hard-coded `%mp3(bitrate=128, samplerate=44100, stereo=true)` and the host, port, and mount from environment variables (`:344-356`). This is replaced by 5.2 (done in backend Phase A, commit e2b9178; not yet in a release).

### 5.2 Stream output from the backend settings, applied at runtime (cross-repo B1)

The `stream.*` settings are already "reconnect" settings (`definitions.go:25-28`, `:188-199`; design §6.3: a reconnect, not a restart). The mechanism:

1. **Runtime command.** The script registers a telnet command (`output.set <json>`, `server.register`) on the existing control port. The argument is a JSON object with `host`, `port`, `mount`, `tls`, `codec`, `bitrate_kbps`, `sample_rate_hz`, `channels`, `name`, `description`, `genre`. The script parses it with `json.parse` (strings never become script code), validates it against the same enums as the settings and against the encoder limits (4), and:
   - if it equals the output that runs, answers `OK unchanged` and does nothing (so a backend restart never reconnects the stream);
   - otherwise calls `shutdown()` on the current `output.icecast`, creates a new one with the new parameters on the same `radio` source, attaches the `on_connect`/`on_disconnect` reports, and answers `OK applied`. The config is written to `OUTPUT_STATE_FILE` when the new output connects, not here, so an output that crashes Liquidsoap never becomes the startup output;
   - if it is invalid or over an encoder limit, answers `ERROR <reason>` and the running output stays.
   The source password stays in Liquidsoap's own environment and is never sent over telnet.
2. **Backend push.** When a `stream.*` setting is saved, the backend sends `output.set` over the control link and logs the answer; on every new control connection it sends it as part of the resync it already does (`internal/playout/pusher.go:134-140`), so Liquidsoap converges after any restart of either side. A failed apply is an error alert in the panel.
3. **Startup.** The script reads its initial output, in this order: `GET ${BACKEND_OUTPUT_URL}` (new backend endpoint, 2 s timeout), then the last output that connected in `OUTPUT_STATE_FILE` (`/var/cache/liquidsoap/output.json` on the named volume `liquidsoap-state`), then its environment (`ICECAST_HOST`, `ICECAST_PORT`, `ICECAST_MOUNT`, `ICECAST_TLS`) with defaults equal to the settings defaults. Every JSON field is required (strict `json.parse` into a typed record); an output from the backend or the state file that fails the checks is logged and skipped. The backend's resync corrects it as soon as the control link connects.
4. **Encoder construction.** One branch per codec and channel count: `%mp3(bitrate=…, samplerate=…, stereo=true|false)`, `%vorbis.cbr(…)`, `%fdkaac(aot="mpeg4_aac_lc", …)` (AAC-LC: the HE-AAC v2 default fails on mono). Mono outputs take `mean(radio)`. **Encoder limits** (design §8.8, v7-25): some Vorbis combinations make Liquidsoap exit when the output starts, which `--check` does not see: mono 22050 Hz above 64 kbps, mono 32000 Hz above 160, mono 44100/48000 Hz above 192, stereo 22050 Hz above 160. The script refuses them (`ENCODER_LIMITS`), and the backend refuses to save such a combination (B12).
5. **TLS.** With `tls` true the output uses `transport=http.transport.tls()`, which verifies the server against the image's system roots (5.4). `http.transport.ssl()` is not used: in the 2.4.6 image it rejects public chains, Let's Encrypt included ("unable to get local issuer certificate"). The backend setting that carries it is new (B6).
6. **Charset.** The script sends metadata in UTF-8 (the default `encoding`). `encoding="ISO-8859-1"` is a documented fallback only, not implemented unless the developers ask for it (11.15).

**Verified on `savonet/liquidsoap:v2.4.6` (2026-11-11, test script with a `libretime/icecast:2.4.4-alpine` container):**

- An `output.icecast` created at runtime from a `server.register` command handler starts and connects; `shutdown()` on the old one closes it. The sources are untouched: `source.time(radio)` kept counting through the switch.
- Bitrate, sample rate, mount, and codec changed at runtime on the same mount: mp3 128/44100/stereo → mp3 192/48000/mono (`/radio2`) → vorbis 128 → AAC (`%fdkaac`) → mp3 320. Icecast reported each new format.
- On the same mount the first connect gets `403 Forbidden` (Icecast has not released the mount yet), and the retry 3 s later succeeds: the stream gap is about 4 s. The script should shut down first, then create, and may shorten the retry delay (`on_error`); the backend's mount check (5 s, 2 failed checks by default) normally does not open an outage for it.
- Numeric encoder parameters (`bitrate`, `samplerate`) may be runtime values, but `%mp3(stereo=…)` must be a literal `true` or `false`, and a mono encoder needs a mono source. **`liquidsoap --check` accepts a script that fails at runtime on a non-literal `stereo`**, so CI must run the script, not only check it (D7).
- Backend Phase A (2026-11-12, commits 6b270a5, e2b9178, 7214870): the runtime smoke test (`dev/liqsmoke`, `make liquidsoap-smoke`, CI job `liquidsoap`) switches the output for every codec × channels × sample rate (22050, 32000, 44100, 48000 Hz) at 64 and 320 kbps and on both sides of every encoder limit, plain and through a TLS proxy. It found the Vorbis limits of item 4, the AAC mono failure of the HE-AAC v2 default, and the `ssl()` failure of item 5.
- Result: **every `stream.*` setting can be applied at runtime**, including the codec, within the encoder limits. No stream setting needs the maintenance window. `stream.loudness_target_lufs` and `stream.hls_enabled` are not used by the script today (B5).

### 5.3 Tasks

1. `config/liquidsoap/radio.liq` = the backend's `dev/liquidsoap/radio.liq` at tag `v${BACKEND_TAG}` with `config/liquidsoap/production.patch` applied. The patch holds only production overrides; the goal is an empty patch (the dev script already takes every value from the environment). Candidate overrides: the stream name default, log level. Any other difference is a bug in one of the two scripts.
2. **Contract header.** The dev script carries (B2):
   ```
   # radio-atlantida-contract: <N>
   # min-backend: <X.Y.Z>
   ```
   The tool refuses an install when the backend tag is below `min-backend` of the script it would run, and when the contract number of the committed script differs from the one in the running container (then a maintenance restart is due, 11.5).
3. External Icecast requirements, written into the runbook (D7) for the operator and the server owner (facts in 5.4):
   - the source password of `/atlantida` is the one in `.env` (`ICECAST_SOURCE_PASSWORD`);
   - the server owner is asked to set `<charset>UTF-8</charset>` on the mount; until then Cyrillic and Armenian titles show as `*`, which is accepted (11.15);
   - the server stays reachable over TLS on port 8001 for the source connection and for `GET /status-json.xsl`, which the backend's outage detector reads; the station's outgoing internet connection is part of the air chain;
   - outage detection watches the external server: a provider outage opens an outage and marks items missed, like any other.
4. The first-install stream settings (host, port, mount, TLS, codec MP3, 160 kbps, 44100 Hz, stereo) are written by `radioctl first-run` through the settings API (D4, 6.8).
5. Test (manual, before the first air date): the hour-boundary and fallback acceptance test of design §8.10 on the dry run stack (D7) and on the production machine.

### 5.4 The production server (checked 2026-11-12, read-only, no source connection)

- `GET https://listen.teploe.net:8001/status-json.xsl` answers 200: `server_id` "Icecast 2.4.4", Let's Encrypt certificate for `listen.teploe.net` (valid to 2027-01-31). Plain HTTP on port 8001 is reset by the server, so **8001 is TLS only**. The mounts advertise `listenurl` `http://listen.teploe.net:8100/...`: port 8100 serves listeners and the status page over plain HTTP. Whether 8100 also accepts sources was not tested (no credentials); this plan uses 8001 with TLS.
- `/atlantida` is **live now**: "Radio Atlantida", `audio/mpeg`, 160 kbps, 44100 Hz, stereo, a source of the current playout system. The switch to this system is the manual cut-over (6.9). The first-install default format matches it (11.14).
- Another mount on the same server (`/lida`) streams `audio/aac`, so the server relays AAC. AAC from this station is not tested on the production server; the default codec is MP3.
- **TLS in Liquidsoap 2.4.6:** the script uses `transport=http.transport.tls()`; a source connection over TLS to a test Icecast behind a TLS proxy connects and streams (smoke test). `http.transport.ssl()` passes `--check` but rejects public certificate chains, Let's Encrypt included ("unable to get local issuer certificate"; found 2026-11-12 in backend Phase A), so it is not used. `http.get` from the image to the real status page over HTTPS returned 200 (the image trusts the Let's Encrypt chain).
- **Backend mount check:** `internal/outage/icecast.go:52` builds `http://host:port/status-json.xsl` with a fixed scheme, so it cannot read the 8001 status page (B6). The match is by the mount path of `listenurl` (`icecast.go:109-113`), so the port 8100 in `listenurl` does not matter. While the old source is on the mount, the check reports the mount as present although the source is not ours (6.9).
- **Metadata charset (test containers: Liquidsoap v2.4.6, `libretime/icecast:2.4.4-alpine` with its default ISO-8859-1 mount, title "Привет Բարեւ café", artist "Арт"):**
  - Default (`encoding` UTF-8): Liquidsoap sends `charset=UTF-8` with the metadata (already found in phase-21 §3.1), and Icecast converts it to the mount charset: status page `*** - ****** ***** café`, in-stream ICY title `*** - ****** ***** caf\xe9` (Latin-1 bytes).
  - `encoding="ISO-8859-1"`: Liquidsoap sends the UTF-8 bytes labelled ISO-8859-1 and Icecast stores them unchanged. The in-stream ICY title that listeners' players read is correct UTF-8 (`Арт - Привет Բարեւ café`), but Icecast's status page shows it garbled.
  - Conclusion: the charset parameter cannot make Icecast keep UTF-8; the mount charset is a request to the server owner (11.15).

**Done when:** `config/liquidsoap/radio.liq` equals the dev script at `v${BACKEND_TAG}` plus the patch (CI); `liquidsoap --check` passes in the pinned image; the runtime smoke test (D7) runs the script against a test Icecast (plain and behind a TLS proxy) and applies `output.set` for every codec × channels × sample rate, on both sides of every encoder limit; the contract header exists.

## 6. Phase D4 — Host install (Windows 10 or 11, Docker Engine in WSL2)

**Goal:** `install.bat` turns a Windows 10 or 11 machine into a station host: WSL2, the `radio-atlantida` distribution, Docker Engine, autostart at boot without a sign-in, host settings, and the first configuration of the stack. It is idempotent, resumable after its reboot, logged, and has `-DryRun`.

### 6.1 Requirements and accounts

- **Windows 10, 21H2 (build 19044) or later with current cumulative updates**, or Windows 11; x64; virtualization enabled in the firmware; at least 16 GB RAM (DEFAULT: the WSL VM gets 8 GB), an SSD with at least 100 GB free on the drive that holds the distribution. The production machine is most likely Windows 10 Home without ESU (11.17); the default path uses only what Home allows.
  - WSL from the Microsoft Store (the version `wsl --update` installs) runs on Windows 10 from build 19044 with the November 2022 cumulative update (19044.2311 or later); it brings systemd support and `wsl --import ... --version 2`, which this install needs. The inbox WSL of older builds is not enough (verify on Windows: `wsl --version` must print a version).
  - The installer detects the build: Windows 11 22H2 (22621) or later gets mirrored networking; Windows 10 and older Windows 11 get NAT mode (6.5).
- **Support status.** Windows 10 reached end of support on 14 October 2025; the consumer Extended Security Updates year ended in October 2026, and paid ESU for organisations runs at most until October 2028. Without ESU the machine gets no more security fixes (Defender signatures still update). The station exposes no inbound port (everything is bound to the local machine), which limits the risk; an upgrade to Windows 11 is recommended later and changes nothing in this plan except the network mode, chosen again by `install.bat`.
- **A dedicated local Windows account `radio`** (DEFAULT name), local administrator, password that never expires. Reasons:
  - WSL distributions and the WSL VM belong to a Windows user: a distribution imported by one account does not exist for another, and `SYSTEM` cannot start a user's distribution (verify on Windows), so the boot task must run as the account that owns the distribution.
  - The boot task runs "whether the user is logged on or not", which stores the account's password in Task Scheduler; a dedicated account keeps that password away from any person's account and survives staff changes.
  - Administrator, because the install must register the distribution *for this account* while also changing Windows features and power settings: the install is run signed in as `radio`, and its elevated token keeps the same user (a standard user elevating as another admin would register the distribution for the wrong account).
  - The manager signs in as `radio` to use the panel, `update.bat`, and the media folder.
- `install.ps1 -CreateAccount` creates the account (password typed twice, never logged), sets "password never expires", and asks the operator to sign in as `radio` and run `install.bat` again.
- Docker Desktop must not be installed (it would compete for WSL and the ports); the preflight stops if it finds it.

### 6.2 `install.bat` and `install.ps1` (Windows half)

`install.bat` runs `powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*`. `install.ps1` re-launches itself elevated (`Start-Process -Verb RunAs`) when needed, writes `C:\ProgramData\RadioAtlantida\logs\install-YYYY-MM-DDTHHMMSSZ.log`, and records each finished step in `install-state.json`, so a re-run skips finished steps (each step also checks the real state, not only the record). `-DryRun` prints every step and its check result and changes nothing.

Steps, in order:

1. **Preflight**: Windows version and build (Windows 10 ≥ 19044 with a WSL-capable update level, or Windows 11), edition and patch status (the newest installed update KB and its date, and whether ESU is enrolled; logged for the record, a warning only, never a stop), x64, `HypervisorPresent` or firmware virtualization enabled (`Win32_ComputerSystem`/`Win32_Processor`), running as the `radio` account (else offer `-CreateAccount`), Docker Desktop absent, free disk space, internet reachable (`wsl --update`, Docker apt repository, GHCR, GitHub). On Windows 10 it prints the support-status note of 6.1 once.
2. **Windows features**: enable `Microsoft-Windows-Subsystem-Linux` and `VirtualMachinePlatform` (`dism.exe /online /enable-feature ... /all /norestart`). If a reboot is needed: register a one-shot scheduled task "at logon of `radio`, highest privileges" that runs `install.bat -Resume`, tell the operator, and reboot after confirmation. The task deletes itself when the install continues. (A RunOnce entry would not run elevated, hence the task.)
3. **WSL**: `wsl --update` (the Store WSL), `wsl --set-default-version 2`; `wsl --version` must report at least the version in `versions.env`.
4. **Network mode**: build ≥ 22621 → `mirrored`; otherwise `nat` (6.5). Recorded in `install-state.json` and passed to the Linux half.
5. **`.wslconfig`** of the `radio` profile (merge: unknown keys are kept): `memory=8GB`, `processors` = half the logical CPUs (at least 2), `swap=4GB`, `vmIdleTimeout=-1` where supported (6.4); in mirrored mode `networkingMode=mirrored`; in NAT mode `localhostForwarding=true` (the default, written explicitly). Key names and their section follow the installed WSL version. A change is followed by `wsl --shutdown` (nothing runs yet on a fresh machine; on a re-run this interrupts the air and needs confirmation).
6. **Media folder**: ask for the station's Windows media folder (default `D:\RadioAtlantida\media`, or the existing folder of tracks), check that it exists and is on a local NTFS drive (not a network share, not removable), and convert it to the WSL path (`/mnt/d/...`). Add a Microsoft Defender exclusion for it (6.6). The path goes to `.env` in step 11.
7. **Distribution**: download the pinned Ubuntu 24.04 LTS WSL root file system (URL and SHA-256 in `versions.env`), verify the checksum, `wsl --import radio-atlantida <InstallRoot>\wsl <rootfs> --version 2` (`InstallRoot` default `C:\RadioAtlantida`, or a data drive with `-InstallRoot D:\RadioAtlantida`). An import is used instead of `wsl --install` because it gives an exact image, a fixed name, and no interactive first-run dialog, and it works on Windows 10 (verify on Windows). If the distribution exists, it is kept.
8. **Linux half**: copy `install/` into the distribution and run `setup-distro.sh --network-mode <mode>` as root (6.3). Then `wsl --terminate radio-atlantida` so `/etc/wsl.conf` (systemd, default user, drive mount options) takes effect, start it again, and check `systemctl is-system-running` (`running` or `degraded` with only known units), `docker info`, and that the media folder is visible and writable by the group (6.6).
9. **Boot and keep-alive tasks** (6.4): ask for the `radio` password once (secure prompt) and register the tasks.
10. **Host hardening** (6.7).
11. **Deploy repository and first configuration**: inside the distribution, `git clone https://github.com/AstRonin/radio-atlantida-deploy` (public, no token) to `/opt/radio-atlantida/deploy` as the Linux user `radio` (or `git pull --ff-only` if it exists), then `radioctl first-run` (6.8); its prompts appear in the same window.
12. **Shortcuts** on the `radio` desktop: "Radio panel" (`http://127.0.0.1:8080`), "Radio media" (the Windows media folder), "Radio update" (`update.bat`), "Radio Grafana" when monitoring is on.
13. **Summary**: Windows build and network mode, versions installed, the Grafana admin password (shown once, the only secret ever shown), the next steps (import media, build clocks, the cut-over of 6.9), and the log path.

### 6.3 `setup-distro.sh` (Linux half, root, idempotent)

1. `/etc/wsl.conf`: `[boot] systemd=true`; `[user] default=radio`; `[interop] appendWindowsPath=false` (a clean `PATH`); `[automount] enabled=true`, `options="uid=1000,gid=10001,umask=002,fmask=002"` (every file on the Windows drives appears owned by `radio:radio-media` with group write; no `metadata`, so no per-file Linux permissions can drift from this, 6.6).
2. Users and groups: Linux user `radio` (uid 1000), group `radio-media` (gid 10001), `radio` in `radio-media` and `docker`.
3. Packages: `ca-certificates curl git jq iproute2` and, from Docker's apt repository (key fingerprint pinned in `versions.env`), `docker-ce`, `docker-ce-cli`, `containerd.io`, `docker-compose-plugin` at the pinned versions, then `apt-mark hold` on them, so an `apt upgrade` never changes the engine; `unattended-upgrades` for Ubuntu security updates only (the held packages are excluded). An engine upgrade is a new `versions.env` and an `install.bat` run in a maintenance window.
4. `systemctl enable --now containerd docker`. `/etc/docker/daemon.json`: `log-driver json-file` with `max-size 10m`, `max-file 3`; `userland-proxy` stays on (the default): its listening sockets are what WSL's localhost forwarding detects in NAT mode (6.5).
5. Directories on ext4: `/srv/radio/journal` owned by the uid the pinned Liquidsoap image runs as (read at install with `docker run --rm --entrypoint id savonet/liquidsoap:v2.4.6`, not hard-coded), mode `0755`; `/srv/radio/backups` owned `radio`, mode `0700`; `/opt/radio-atlantida` owned `radio`. The media folder is not created here: it is the Windows folder (6.6).
6. `/usr/local/bin/radio-keepalive` (6.4) and the systemd units: `radio-network.service` (6.5), `radio-backup.timer`/`.service` (daily backup, D6), `radio-boot-check.service` (after `docker.service` and `radio-network.service`: waits up to 10 minutes for the stack to be healthy and writes the time it took and the result to `logs/boot-YYYY-MM-DDTHHMMSSZ.log`; it only reports).
7. Checks at the end: `docker info` answers, and `docker version` and `docker compose version` match `versions.env` (no test image is pulled).

### 6.4 Boot, keep-alive, and WSL idle shutdown

- **Idle shutdown.** WSL stops a distribution shortly after its last process started through `wsl.exe` ends; systemd services such as `docker.service` do not count as such a process (verify on Windows), and the VM itself stops `vmIdleTimeout` after the last distribution stopped. So the stack would stop minutes after boot without a keep-alive. `vmIdleTimeout=-1` alone is not enough, because it does not keep the distribution running.
- **Keep-alive task** ("Radio Atlantida WSL"): trigger at system startup, plus a repetition every 5 minutes; runs as `radio`, "run whether user is logged on or not" (password stored by Task Scheduler), not "highest privileges"; action `wsl.exe -d radio-atlantida -u radio --exec /usr/local/bin/radio-keepalive`; settings: no execution time limit, restart on failure every minute without a limit, "do not start a new instance" while one runs, start on battery and do not stop on battery. `radio-keepalive` writes one line to the boot log and then `exec sleep infinity`, so the `wsl.exe` process stays and keeps the distribution running. The 5-minute repetition restarts it if it ever ends.
- **Boot sequence**: Windows starts → the task starts the distribution → WSL mounts the Windows drives (the media folder) → systemd starts containerd and Docker → `radio-network.service` checks the network values (6.5) → Docker starts the containers by `restart: unless-stopped` → `radio-boot-check` records when the stack is healthy. No sign-in is needed.
- **WSL in a non-interactive session** (the task runs without a desktop session): the Store WSL supports being started from a task that runs whether the user is logged on or not, on Windows 10 as on Windows 11; the inbox WSL of older Windows 10 builds did not (verify on Windows, on the production machine itself). Fallback if it fails: automatic sign-in of `radio` (Sysinternals Autologon, password stored as an LSA secret), a lock-screen policy, and the same task with an "at logon" trigger.
- The manager's own sign-in as `radio` joins the same running VM; `wsl.exe` from that session and from the task share one VM per user (verify on Windows).

### 6.5 Networking

Two modes, chosen by the installer from the Windows build (6.2 step 4):

- **NAT mode (Windows 10, and Windows 11 before 22H2).** The WSL VM has its own address on a virtual switch that only the Windows host reaches (it changes at every boot).
  - **TCP (panel, Grafana):** WSL's localhost forwarding makes a TCP socket that listens inside the VM on `127.0.0.1` or `0.0.0.0` reachable at Windows `127.0.0.1` on the same port. Docker publishes `127.0.0.1:8080` and `127.0.0.1:3000` through its userland proxy, which is such a listening socket, so `http://127.0.0.1:8080` works from the Windows browser (verify on Windows 10: forwarding of sockets bound to the VM's `127.0.0.1` by `docker-proxy`; if only `0.0.0.0` sockets are forwarded there, the fallback binds the panel and Grafana to the VM address, which is still reachable from this host only).
  - **UDP (live voice):** localhost forwarding is TCP only. The backend's UDP port is therefore published on `0.0.0.0:8189` inside the VM (`LIVE_UDP_BIND=0.0.0.0`), and the browser sends to the VM address directly: `LIVE_PUBLIC_IPS` is the VM's `eth0` address. The VM is not reachable from the LAN in NAT mode (the virtual switch is internal and Windows does not forward inbound traffic to it), so this binding does not expose the port (verify on Windows: no LAN host reaches `<VM address>:8189`).
  - **Boot unit `radio-network.service`** (after `docker.service`): reads the `eth0` IPv4 address (`ip -4 -o addr show dev eth0`), writes `LIVE_UDP_BIND=0.0.0.0` and `LIVE_PUBLIC_IPS=<address>` to `runtime/network.env`, and, when the address differs from the one the running `backend` container was created with, recreates it (`docker compose up -d --no-deps backend`). This takes about 30 s after boot and does not touch playout (Liquidsoap plays its queue, the backend's resync refills it). (A backend that reads the address per session, which would avoid the recreate, is not planned: the 30 s recreate is acceptable.)
  - **WebRTC:** the bridge offers one host candidate `<VM address>:8189` (phase-12 decision 4, no STUN or TURN). The browser on Windows has a route to the VM address through the WSL virtual adapter, and its own candidates do not matter for a host-to-host session on one machine (verify on Windows: a live session from the Windows browser, while the panel itself is loaded from `127.0.0.1`).
- **Mirrored mode (Windows 11 22H2 or later).** The VM shares the Windows interfaces, including loopback, for TCP and UDP: `LIVE_UDP_BIND=127.0.0.1`, `LIVE_PUBLIC_IPS=127.0.0.1`, and `radio-network.service` writes these fixed values. This is the automatic choice on Windows 11 and the target after an upgrade.
- **Microphone:** the panel is opened at `http://127.0.0.1:8080`; `127.0.0.1` and `localhost` are secure contexts in browsers, so `getUserMedia` works without HTTPS (as in the dev check of phase 12). The UDP media path to the VM address is a separate connection and does not affect the page's secure context.
- **Outbound** (GHCR, the external Icecast over TLS, NTP) goes through the VM's NAT or the mirrored interface without configuration.
- LAN exposure stays off in both modes.

### 6.6 Files

- **Media: the existing Windows folder (chosen, option a).** The station's tracks already lie in a Windows folder; it becomes the media root, seen in WSL as `/mnt/<drive>/...` and bind-mounted into `backend` (read-write) and `liquidsoap` (read-only). How the backend uses it (checked in the code):
  - the import registers files **where they lie** under the media root; it never copies, moves, or renames (design §8.7, backend phase-18, frontend `f16-import-in-place-plan.md`; `internal/ingest/import.go`, `internal/media/files.go` `Walk`/`Tree`);
  - the only writes are a transcode or "Fix loudness" (a hidden temporary file in the same folder, `fsync`, then a rename over the target, `internal/media/files.go:212-239`) and Replace file, which points a track at another existing file; a track that is queued, on air, or about to be pushed is skipped (design §8.7);
  - nothing watches the folder: the backend has no `fsnotify`/inotify code; new files are found when the manager imports a folder. Alloy watches only the journal, which stays on ext4.
- **Assessment of the Windows folder through WSL's drive mount (9P):**
  - Permissions: the mount options of 6.3 make every file `radio:radio-media`, group-writable; the backend (uid 10001 with the supplementary group 10001) can create and rename files; Liquidsoap and Alloy only read (verify on Windows: a transcode writes a fixed file and the rename succeeds).
  - Read speed: playout reads one file at a few hundred kbit/s, far below the 9P throughput; an import hashes and probes each file, and an analysis decodes it with ffmpeg, which is slower than on ext4 but bounded (about 800 files, one-time); acceptable (verify on Windows: time the first import).
  - Names: Cyrillic and Armenian file names are UTF-16 on NTFS and UTF-8 in WSL, converted by the mount; the backend's relative paths are UTF-8. Names with characters Windows forbids (`:`, `?`, `*`, ...) cannot exist there; the transcode's `<name>.optimal.<ext>` is a valid Windows name.
  - Case: NTFS folders are case-insensitive. A rename in Explorer that only changes the case keeps the file reachable by its old path; the backend matches paths exactly, so the track is reported as `file_missing` only after a real rename. Acceptable.
  - No inotify on the Windows drive: not needed (above).
  - Risk of the folder being changed while on air: the manager can rename, move, or delete a queued file in Explorer; Liquidsoap then plays the next item and the track is reported `file_missing`. The same risk exists with an ext4 folder opened through `\\wsl.localhost`. The runbook says: add files freely, never rename or delete a file that is in the plan; use Replace file instead.
  - Antivirus and indexing: a Defender exclusion for the media folder (6.2 step 6) avoids scans locking or slowing files while Liquidsoap reads them; Windows Search indexing of the folder is turned off.
  - Boot: WSL mounts the Windows drives when the distribution starts, before Docker starts the containers; `create_host_path: false` (D2) makes a container fail visibly if the drive is missing (for example a disconnected external disk) instead of seeing an empty folder.
  - Backup: the media folder is ordinary Windows files, so any Windows backup tool can copy it; this plan does not back it up (D6).
- **Alternatives (documented, not chosen):**
  - (b) **Copy once into ext4** (`/srv/radio/media`) with `radioctl import-windows-folder <Windows path> <subfolder>` (`rsync -rt --info=progress2` from `/mnt/...`), then reach it from Explorer at `\\wsl.localhost\radio-atlantida\srv\radio\media`. Faster and with real Linux permissions, but the tracks then exist twice until the old folder is removed, every new track must be copied in, and the files live inside the virtual disk that `wsl --unregister` deletes. Choose it if the 9P checks of option (a) fail on the production machine.
  - (c) **Windows folder as an import source only**, with the import copying into an ext4 media root: the backend does not support it. The import is in place since phase 18, which removed the inbox and library folders of v7-14; bringing back a copying import would be backend and frontend work for little gain over (b).
- **Journal, backups, deploy repository: ext4** (`/srv/radio/journal`, `/srv/radio/backups`, `/opt/radio-atlantida/deploy`): the journal is written continuously and read by Alloy with polling and by the backend; ext4 gives real `fsync` and speed. The backups get a copy on the Windows drive (D6). The operator reaches these folders, when needed, at `\\wsl.localhost\radio-atlantida\srv\radio`.
- **Disk**: the distribution lives in one growing virtual disk (`ext4.vhdx`) under `InstallRoot`; it does not shrink by itself. `radioctl status` warns when the Windows drive of the virtual disk or of the media folder has less than 15 % free.
- **Never `wsl --unregister radio-atlantida`**: it deletes the virtual disk with the database, the journal, and the backups inside it (the media folder on Windows is not affected). The runbook says so in bold; the Windows copy of the backups (D6) is the protection against it.

### 6.7 Host hardening (by `install.ps1`)

- Power: `powercfg /change standby-timeout-ac 0`, `hibernate-timeout-ac 0`, `disk-timeout-ac 0`, `powercfg /hibernate off` (this also turns off Fast Startup, so a shutdown is a real cold boot); the monitor may still turn off.
- Windows Update (11.17), Home-compatible default path: `install.ps1` sets active hours to the longest window Windows allows (18 hours), chosen by the operator at install; the default covers the broadcast day (06:00–24:00). No Pro-only policy (`AUOptions`) is set. Without ESU, Windows 10 gets no cumulative updates after its end of support, so there are no update-driven reboots and the reboot risk is low; Defender definition updates continue and do not reboot. If ESU turns out to be enrolled, updates and reboots resume: the runbook then says to use "Pause updates" (up to 5 weeks) and to install updates by hand in a maintenance window, followed by the reboot check of D4.
- Security note (runbook): the host is unpatched; do not use it for email or browsing; it exposes no inbound port; an upgrade to Windows 11 is recommended later, where `install.bat` switches to mirrored networking by itself.
- Time: Windows Time service automatic with NTP (`w32tm /config /syncfromflags:manual /manualpeerlist:"time.windows.com,0x9 pool.ntp.org,0x9" /update`), `w32tm /resync`. The WSL VM takes its clock from Windows; the reboot test compares the two.
- Media folder: Defender exclusion, indexing off (6.6).
- BIOS/UEFI (operator note in the runbook, not scripted): "Restore on AC power loss: Power On", virtualization (VT-x/AMD-V) enabled; a UPS is recommended.

### 6.8 First configuration (`radioctl first-run`, called by the installer)

The first run is any run where no successful install of the stack is recorded (`state.json` absent, or `last_result` not `ok` with no `current`).

1. If `state.json` is absent but the volume `radio-atlantida_pgdata` exists, stop: data without state needs a manual decision.
2. `.env` from `.env.example`: random letters-and-digits values for `POSTGRES_PASSWORD`, `LIQUIDSOAP_CONTROL_PASSWORD`, `GRAFANA_ADMIN_PASSWORD`, `GRAFANA_DB_PASSWORD`; prompts for `GHCR_USER`, `GHCR_READ_TOKEN`, `ICECAST_SOURCE_PASSWORD`, refusing empty values. Nothing generated or entered is printed, except the Grafana admin password, shown once.
3. GHCR login inside the distribution (`docker login ghcr.io --password-stdin`); the credentials file `~/.docker/config.json` of `radio` has mode `0600` (it holds the read-only token).
4. Release check and pull of `release.env` (as 7.3 steps 3 and 5). `radio-network.service` has written `runtime/network.env` (6.5); `.env` gets `MEDIA_ROOT` from the folder chosen in 6.2 step 6.
5. Start in order: `db` (healthy), `migrate`, `backend` (health gate without the `queued` condition: there is no plan yet), `frontend`, `loki`, `alloy`, monitoring if enabled. `liquidsoap` is **not** started: the mount still carries the old source (6.9).
6. Stream settings through `PUT /api/settings/{key}`, proposed and confirmed in the prompt: `stream.icecast_host=listen.teploe.net`, `stream.icecast_port=8001`, `stream.mount=/atlantida`, `stream.icecast_tls=true` (B6), `stream.codec=mp3`, `stream.bitrate_kbps=160`, `stream.sample_rate_hz=44100`, `stream.channels=2`, `stream.name="Radio Atlantida"`. They stay panel settings, changeable at any time (applied at runtime, D3).
7. Enable `radio-backup.timer` (D6) and run the first backup.
8. Record `current`, `applied`, and `last_result = ok` in `state.json`; write the run log.
9. On failure: `last_result = failed`, the failing step is reported, and the next `install.bat` or `update.bat` takes the first-run path again.

### 6.9 Cut-over on `/atlantida` (manual runbook step, `radioctl cutover`)

The external mount carries the current playout system's source until the developer switches over by hand.

- Preconditions, checked by `radioctl cutover`: the stack is healthy; the media is imported, the grid and clocks exist, and the plan covers the next 24 hours (`GET /api/air-window` and the plan status); an emergency playlist is set; the stream settings are those of 6.8.
- Steps:
  1. The operator stops the old source (on the old system).
  2. `radioctl cutover` asks for the typed word `CUTOVER`, starts `liquidsoap`, and waits up to 60 s for `GET /api/playout/status` to show `control_link.connected`, `output.state = connected`, and queued items, and for `GET /api/broadcast/status` to show `connected`. It prints the result and the listen URL to check by ear.
  3. The operator listens to the stream.
- Outage detector during the cut-over: while the old source is on the mount, the backend's mount check sees the mount as present and cannot tell that the source is not ours; only Liquidsoap's own output report (connected or not) shows whether our stream reaches the server. That is why `liquidsoap` stays stopped until step 2: a running Liquidsoap would retry against the busy mount (403) and report `reconnecting`.
- Rollback: `radioctl cutover --undo` stops `liquidsoap` (typed confirmation), and the operator restarts the old source. The backend may record an outage for the gap; it is not marked as missed content if the plan was empty.
- After the cut-over, `state.json` records `cutover_utc`; from then on `liquidsoap` is started at boot by its restart policy like every other service.

**Done when:**

- Pester tests of `install.ps1` pass (D7); `setup-distro.sh` runs twice in a row on an Ubuntu 24.04 VM with systemd without changes on the second run.
- On a Windows 10 test machine of the production machine's build (or the production machine before air): a full install from a fresh Windows, including the feature reboot and the resume, finishes; a second `install.bat` run changes nothing.
- **Reboot test:** after a cold reboot with nobody signed in, the stack is healthy and Liquidsoap is connected to the server (or, before the cut-over, all services except `liquidsoap` are healthy) within **5 minutes** of power-on, as recorded by `radio-boot-check`; the operator then signs in and `update.bat status` confirms it.
- The Windows checks marked (verify on Windows) pass: Store WSL with systemd and `wsl --import` on the production build, WSL from the task without a sign-in, no idle shutdown with the keep-alive, localhost forwarding of the panel and Grafana (NAT mode), a live voice session from the Windows browser to the VM address after two reboots (the address changed and `radio-network.service` recreated the backend), no LAN host reaching the VM's UDP port, the media folder: group write (a transcode writes a fixed file and renames it), the first import's duration, Cyrillic file names, clock agreement between Windows and WSL (difference under 1 s).

## 7. Phase D5 — Update tool (`radioctl` inside WSL)

**Goal:** the manager updates, installs a given release, rolls back, restores, and runs maintenance from a double-click, with the least interruption of the air.

### 7.1 Architecture (chosen: bash inside the distribution)

`update.bat` is a thin wrapper: `wsl.exe -d radio-atlantida -u radio --cd /opt/radio-atlantida/deploy -- bin/radioctl %*`, keeping the exit code and pausing only when started by double-click. If the distribution does not exist for the signed-in account, it says to sign in as `radio`.

Reasons for bash in WSL instead of PowerShell calling `wsl -- docker compose ...` for every step:

- every tool the update needs (`docker`, `docker compose`, `git`, `curl`, `jq`) is native there, with no quoting and encoding layer of `wsl.exe` around each call and no Windows/Linux path translation;
- the same script runs on the Mac dry run and in Linux CI, so the update logic is tested where it runs; PowerShell is left only for what must happen on Windows (`install.ps1`, the wrappers);
- Git is always present in the distribution, so the zip-download fallback of the earlier plan is gone.

### 7.2 Commands

- `update.bat` — install the release in `release.env` of the freshly fetched repository.
- `update.bat <backendTag> [<frontendTag>]` — install any release ≥ 1.0.0, newer or older. The frontend tag defaults to the backend tag. The tool resolves the digests from the registry, checks compatibility and, for an older target, the downgrade guard (§10.4). The run log and `state.json` note that the running release differs from `release.env`; the next plain `update.bat` returns to `release.env`.
- `update.bat rollback` — install the release that ran before the last update (the `previous` entry, by digest); the same checks as an older target.
- `update.bat restore [<dump>]` — D6.
- `update.bat maintenance` — apply pending changes that need a restart of `liquidsoap` or `db` (7.5).
- `update.bat status` — release, container health, boot-check result, air state, pending maintenance, last backup, disk space, media permission problems.
- `update.bat backup`, `update.bat restore-drill` — D6. `update.bat cutover [--undo]` — 6.9. `update.bat compose <args>` — diagnostics only.
- `--dry-run` with any command — prints every step and changes nothing.

Every command takes `state.lock` (`flock`): a second instance, or the backup timer during an update, waits or stops with a message.

### 7.3 Update steps, in order

1. **Preflight.** `docker info` answers and Docker and Compose match `install/versions.env` (otherwise: run `install.bat`); `.env` exists (else the first-run rules apply, 6.8) and has every required variable; with `MONITORING_ENABLED=true` also `GRAFANA_ADMIN_PASSWORD` and `GRAFANA_DB_PASSWORD` (the missing name is shown, never the value).
2. **Fetch this repository.** `git pull --ff-only`; show what changed. If `bin/radioctl` itself changed, re-run the new script once with the same arguments.
3. **Release check.** The target is `release.env`, or the tags given on the command line (digests from `docker manifest inspect`). Log in to GHCR if needed. Both images exist, and with `release.env` the digests belong to the tags. Both tags are ≥ 1.0.0. Compatibility: the `min-backend` of `config/liquidsoap/radio.liq` ≤ the backend tag (a target older than the running script needs is refused; installing it would need the older script in a maintenance window, out of scope); frontend and backend have the same MAJOR.MINOR (11.7). **Older target:** read `min_app_version` from the database (`docker compose exec -T db psql -tAc "SELECT min_app_version FROM schema_compat"`; no table means 1.0.0). If the target is below it, stop and offer the restore path of §10.4. Stop on any failure, nothing changed yet.
4. **Air check** (skipped before the cut-over). Through `docker compose exec -T backend curl -fsS http://127.0.0.1:8090/...` (the backend is not published, and nginx does not proxy `/readyz`):
   - `GET /api/broadcast/status`: `state = unreachable` (an outage is open) → stop and report.
   - `GET /api/status/live`: `on_air` true or null → ask for confirmation (live voice will drop; null is never read as off-air).
   - `GET /api/playout/status`: `control_link.connected` must be true and `queued` must hold at least 2 items (Liquidsoap keeps playing them while the backend restarts); otherwise wait and re-check for up to 2 minutes, then stop.
   - Time window: not within `playout.prepush_minutes` (read from `GET /api/settings`) + 1 minute before a station hour, nor in the first minute after it, because the next hour's items are pushed then. The tool waits for the window to pass (showing the wait) or stops if asked.
   - An unreachable endpoint (refused, timeout, 5xx) → ask for confirmation; never read as "off air".
5. **Pre-pull and record.** `docker compose pull` for every service (pulling restarts nothing). Record `RepoDigests` of the pulled application images and the planned release in `state.json`: `previous` = the running release, `target` = the new one, `last_result = pending`.
6. **Backup, mandatory.** `pg_dump -Fc` into `BACKUP_ROOT/update/db-YYYY-MM-DDTHHMMSSZ.dump` (UTC), plus a copy of the journal files and the Windows copy (D6). No flag skips it. A failed dump stops the update. The path goes into `state.json`.
7. **Migrate with the old backend running** (newer targets only; an older binary would do nothing on a newer schema, `migrations.go:75-78`). `docker compose run --rm migrate`. Migrations are expand-only (§10.3), so the running release keeps working against the new schema. The migration connection gives up on a lock after 10 s instead of stalling the running service (`internal/db/db.go:35`, `:99-108`). If the running release is below the target's `migrate target-compat` (a contract migration in this update that the running release cannot work with, possible only when releases are skipped), the old backend is stopped before the migration and started again on a rolled-back failure. The outcome is read from the exit code of `backend migrate up` (backend B7); **no path asks the operator anything**:
   - `0`: continue with step 8.
   - `4` (**rolled back**: the failed migration was transactional; the backend reset the version, the database is not dirty; migrations applied earlier in the same run stay and are expand-only): abort the update, keep the old images (nothing was recreated), run no restore, append the failure to `history`, and show: "Update to X failed and was undone. Broadcasting continues on Y. Send the log `logs/update-….log` to the developer."
   - `5` (**dirty**: a non-transactional migration failed, or the outcome is unknown): restore this run's pre-update dump automatically (D6 task 4 without the confirmation: stop `backend`, `pg_restore --clean --if-exists`, start the old `backend`, health gate). The data written between the dump and the restore is lost, at most minutes (mostly play log rows and operations); the play log of that window is rebuilt from the playout journal by the backend's reconciliation (design §7.1). Then the same message as for `4`, and a `history` entry with `restored: true`.
   - any other non-zero (the migration did not start, for example the database was unreachable): the database was not changed; the same message as for `4`.
   - If the automatic restore itself fails, the old backend is started anyway if it passes its health gate, and the message says "the database could not be restored automatically; the developer must check it" (the only message that is not "continues as before").
8. **Re-check the time window** of step 4 (the pull may have taken minutes).
9. **Backend.** `docker compose up -d --no-deps backend`. The old container gets SIGTERM and up to 30 s (D2 task 2). Liquidsoap plays its queue; the panel and a live session drop for the restart.
10. **Health gate** (default 90 s, at least 60 s): `/readyz` returns 200 (database reachable, `internal/httpapi/health.go:32-51`), `GET /api/playout/status` shows `control_link.connected = true` and `queued` not empty (before the cut-over: `/readyz` only). On timeout: roll back (step 14).
11. **Frontend.** `docker compose up -d --no-deps frontend`, then wait for its health check. On failure: roll back.
12. **Loki, Alloy, Grafana.** Recreate a service when its image or its configuration hash changed (`up -d --no-deps --force-recreate <service>`; Compose does not see changes inside bind-mounted files, so the tool keeps SHA-256 hashes of `config/loki/`, `config/alloy/`, `config/grafana/` in `state.json`). With `MONITORING_ENABLED=true`: `--profile monitoring up -d --wait grafana-role grafana`; with `false`: stop `grafana` if it runs. Failures are warnings only (`monitoring: failed` in the log), never a rollback.
13. **Pending maintenance.** Compare the `liquidsoap` and `db` images, `config/liquidsoap/radio.liq` (hash and contract header), and their compose definitions with the applied values in `state.json`. Report each difference as "pending, run `update.bat maintenance` in a maintenance window" and store it in `state.json`. Never restart them here.
14. **On failure after step 9:** set the previous tags and digests from `state.json` in `runtime/applied.env`, run `docker compose up -d --no-deps backend frontend`, wait for the health gate, append the failure to `history`, and show the same plain message as in step 7 ("Update to X failed and was undone. Broadcasting continues on Y. Send the log … to the developer."); no prompt. Migrations are not rolled back; the previous release works against the new schema (expand-only, and `migrations.Up` accepts a newer schema with a warning). There is no install below 1.0.0 (§10.3).
15. **Finish.** Write `runtime/applied.env`, set `current`, append to `history`, set `last_result = ok` and the applied config hashes in `state.json`; write `logs/update-YYYY-MM-DDTHHMMSSZ.log` with steps, tags, digests, dump path, warnings, pending maintenance, and the result. Secrets are never logged.

**Why not blue/green.** Two backends cannot run side by side: there is one pusher, and two would push the same items into Liquidsoap's queues; the live voice bridge owns the single UDP port 8189; the safety-tone state, the control link state, and the broadcast evidence live in the backend's memory; the outage detector and the nightly plan passes would run twice. A sequential restart costs about 30 s of panel time, while the queue keeps the air going.

### 7.4 Older releases and rollback

- `update.bat rollback` and `update.bat <older tag>`: the release check with the downgrade guard (step 3), the air check, the pull, the dump, then the backend and frontend steps (9 to 11) with the target; no migration. It refuses when no previous release is recorded, the target is below 1.0.0, or the target is below `min_app_version`.
- **Below `min_app_version`:** the tool lists the pre-update dumps in `state.json` `history` that were taken while the database still accepted the target (`min_app_version_before` ≤ target), newest first, with their UTC time and the data that would be lost since then, and offers `update.bat restore <dump>` followed by the install of the target. It never does this without the typed `RESTORE` confirmation (§10.4).

### 7.5 Maintenance (`update.bat maintenance`)

Used for: a new Liquidsoap image, a changed `radio.liq` (contract or patch), a changed `liquidsoap` or `db` compose definition, a new PostgreSQL image (11.5). Not used for stream settings (they apply at runtime, 5.2). A Docker Engine or WSL change is an `install.bat` run in the same kind of window.

1. Show the pending changes and the items planned for the next 15 minutes (`/api/playout/status`, `/api/air-window`), warn that the stream stops for some seconds and that the backend will record an outage and mark items missed unless the journal confirms them.
2. Ask for confirmation by typing `MAINTENANCE`.
3. A dump (as step 6 of 7.3) when `db` is affected.
4. `docker compose up -d --no-deps liquidsoap` (and/or `db`, then wait for `backend` readiness). Liquidsoap starts with empty queues; the backend's resync pushes the queued items again and sends the output config (`pusher.go:134-140`, 5.2).
5. Health: the `liquidsoap` health check, `control_link.connected`, an output report `connected` within 60 s (`/api/playout/status`, `output.state`). No automatic rollback of infrastructure images; on failure the tool reports and offers to re-apply the previous image from `state.json`.
6. Record the applied images and hashes in `state.json`; clear the pending list.

### 7.6 `state.json`

```json
{
  "schema": 1,
  "current":  { "backend_tag": "1.0.1", "backend_digest": "sha256:…", "frontend_tag": "1.0.1", "frontend_digest": "sha256:…" },
  "previous": { "backend_tag": "1.0.0", "backend_digest": "sha256:…", "frontend_tag": "1.0.0", "frontend_digest": "sha256:…" },
  "applied": {
    "liquidsoap_image": "savonet/liquidsoap:v2.4.6", "liquidsoap_script_sha256": "…", "liquidsoap_contract": 3,
    "db_image": "postgres:17.11",
    "config_sha256": { "loki": "…", "alloy": "…", "grafana": "…" }
  },
  "pending_maintenance": [],
  "history": [
    { "utc": "2026-11-11T12:00:00Z", "from": "1.0.0", "to": "1.0.1",
      "dump": "/srv/radio/backups/update/db-2026-11-11T120000Z.dump",
      "schema_version_before": 21, "min_app_version_before": "1.0.0", "min_app_version_after": "1.0.0",
      "boundary": false }
  ],
  "cutover_utc": "2026-11-20T03:00:00Z",
  "last_dump": "/srv/radio/backups/update/db-2026-11-11T120000Z.dump",
  "last_update_utc": "2026-11-11T12:00:00Z",
  "last_backup_utc": "2026-11-11T02:00:00Z",
  "last_result": "ok"
}
```

`history` gets one entry per install (update, older release, rollback), with the pre-update dump and `min_app_version` before and after; `boundary` is true when the install raised `min_app_version`. Its dumps are the restore points of §10.4.

### 7.7 Safety

The tool never runs `docker compose down -v` or `wsl --unregister`, never deletes files outside `BACKUP_ROOT`, the Windows backup copy, and `logs/`, and never restarts `liquidsoap` or `db` outside `maintenance`, `cutover`, or the first run.

**Done when:** every command runs with `--dry-run` and prints its full step list; the bats tests pass; the dry run (D7) completes the first run, an update with a migration, an install of an older release, a refused downgrade below `min_app_version` (with a test schema), a rollback, a restore, a maintenance run, and the cut-over against a test Icecast.

## 8. Phase D6 — Backups and restore

**Goal:** the database and the playout journal can be recovered to at most 24 hours old, on the machine itself, also after a loss of the distribution.

Tasks:

1. `update.bat backup` (`radioctl backup`): `pg_dump -Fc` into `BACKUP_ROOT/daily/db-YYYY-MM-DDTHHMMSSZ.dump`, then a copy of the journal files of `PLAYOUT_JOURNAL_ROOT` into `BACKUP_ROOT/daily/journal-YYYY-MM-DD/` (the journal decides what aired, design §7.1). Runs while on air (`pg_dump` is online). Then the same set is copied to `BACKUP_WINDOWS_COPY` (`/mnt/c/RadioAtlantida/backups`), so a damaged virtual disk or an unregistered distribution does not take the backups with it. Writes `last_backup_utc`; a failure is written to the log and shown by `status` and the next update.
2. **Schedule (chosen): a systemd timer in the distribution** (`radio-backup.timer`, daily at 04:30 station time, `Persistent=true` so a missed run happens after the next boot), running `radioctl backup` as `radio`. It runs in the same always-on distribution as Docker and needs no stored Windows password. A Windows Task Scheduler task calling `wsl.exe -d radio-atlantida -u radio -- bin/radioctl backup` is the documented alternative if the developers prefer to see the job in Windows (11.12).
3. Retention, applied to both locations: the newest `BACKUP_DAILY_KEEP` (14) daily sets and the newest `BACKUP_UPDATE_KEEP` (14) update dumps. Never deleted by retention: the dump recorded as `last_dump`, and the pre-update dumps of the last `BACKUP_BOUNDARY_KEEP` (3) `history` entries with `boundary: true` (the restore points for a downgrade across a contract migration, §10.4). The runbook says how to delete an old boundary dump by hand when disk space is short.
4. `update.bat restore [<dump>]`: the dump recorded for the last update, or a named one (from `BACKUP_ROOT` or the Windows copy). Show the dump name and its UTC time, ask the manager to type `RESTORE`, take a safety dump of the current database, stop `backend` (its connections would block the restore; Liquidsoap keeps playing its queue), `pg_restore --clean --if-exists` into the database, start `backend`, run the health gate. It does not change the release; a restore after a failed migration is followed by `update.bat rollback` if needed (design §9.2), and a restore of a boundary dump by the install of the older target. The schema then has the version of the dump, so the tool runs `migrate up` only if the running backend is newer than that schema.
5. `update.bat restore-drill` (monthly, in the runbook checklist): restore the newest daily dump into a scratch database `radio_restore_drill` in the same `db` container, check the schema version and the row counts of the main tables, drop the scratch database, log the result. It never touches the production database.
6. RPO is 24 hours (daily dump) between updates and seconds before an update. Accepted by the developers.
7. **Risk, stated in the runbook:** there is no off-site copy; a loss of the machine's disk loses the database, the dumps (both copies), the journal, and the media directory together.
8. Production data to development: copy a dump from `C:\RadioAtlantida\backups` to the Mac by hand and restore it into the dev stack (`pg_restore --clean --if-exists`). Never the other way.

**Done when:** backup, both copies, retention, restore, and the drill run on the dry run; on Windows, the timer runs after a reboot and the Windows copy appears in `C:\RadioAtlantida\backups`.

## 9. Phase D7 — CI, runbook, and dry run

**Goal:** every change to this repository is checked before it reaches the production machine, and the procedure is rehearsed.

### 9.1 CI (`.github/workflows/ci.yml`, every PR)

1. Compose renders with `.env.example` and a fixture `applied.env`, with and without `--profile monitoring`. From `docker compose config --format json`: no `latest`; no `icecast` service; the only published ports are the three of D2: the panel and Grafana with `host_ip: 127.0.0.1`, the UDP port with `host_ip` equal to the fixture's `LIVE_UDP_BIND` (rendered once with `127.0.0.1` and once with `0.0.0.0`) and `protocol: udp`; the media mounts have `create_host_path: false`; `loki`, `alloy`, `db`, `liquidsoap` publish nothing; `migrate` and `grafana-role` have `healthcheck.disable: true`; `backend` has `stop_grace_period: 30s`, `group_add` 10001, its media mount read-write and its journal mount read-only.
2. `release.env`: tags match `^\d+\.\d+\.\d+$`, digests are set and match the tags (`docker manifest inspect` with a read token), the frontend and backend MAJOR.MINOR rule holds.
3. **Sync with the backend at `v${BACKEND_TAG}`** (`scripts/check-backend-sync.sh`; fetches the public backend repository at that tag, no token, 11.13):
   - infrastructure image tags equal those of `docker-compose.dev.yml`;
   - `config/grafana/`, `config/loki/loki.yaml`, `config/alloy/config.alloy` equal `dev/grafana/` (without its Go test file), `dev/loki/loki.yaml`, `dev/alloy/config.alloy`;
   - `config/liquidsoap/radio.liq` equals the dev script with `production.patch` applied.
4. Loki and Alloy in the pinned images, as the backend CI does (`.github/workflows/ci.yml:49-60` of the backend): `loki -config.file=… -verify-config`; `alloy validate`; `alloy fmt` output diffed against the committed file.
5. Grafana provisioning YAML and dashboard JSON parse; dashboard data source uids match the provisioned ones.
6. Liquidsoap: `liquidsoap --check config/liquidsoap/radio.liq` in `savonet/liquidsoap:v2.4.6`; then the **runtime smoke test** (`tests/liquidsoap/`): the script runs for a few seconds against a test Icecast container (plain, and behind a TLS proxy) with a stub backend (callbacks, emergency, output endpoint), and `output.set` is applied for every codec × channels × sample rate with the lowest and highest bitrate and both sides of every encoder limit (as the backend's `dev/liqsmoke`). Required because `--check` does not catch encoder errors that appear at runtime (5.2).
7. `install/versions.env`: the pinned Docker apt versions exist in Docker's repository for Ubuntu 24.04 (`apt-cache madison` in an `ubuntu:24.04` container with the pinned key); the rootfs URL answers and its SHA-256 matches.
8. `shellcheck` on `bin/radioctl`, `install/*.sh`, `install/radio-keepalive`, `scripts/*.sh`; PSScriptAnalyzer on `install.ps1` and `scripts/*.ps1`.
9. **bats tests** of `radioctl` on Ubuntu, with `docker`, `git`, and `curl` replaced by stubs on `PATH`. Cases: happy path; dump failure stops; migrate exit `4` (rolled back) aborts with the plain message, keeps the old images, runs no restore, and records `history`; migrate exit `5` (dirty) restores the pre-update dump without a prompt, restarts the old backend, and records `restored: true`; a failed automatic restore still starts the old backend and reports it; a migrate that did not start (exit 1) aborts without restore; a target whose `migrate target-compat` is above the running release migrates with the old backend stopped and restarts it on exit `4`; a database already dirty at preflight stops with the plain message; health gate timeout rolls back by digest; frontend failure rolls back; monitoring failure is only a warning; `release.env` digest mismatch stops before any change; incompatible versions stop; open outage stops; `on_air` null asks; unreachable endpoint asks; fewer than 2 queued items waits then stops; prepush window waits; a changed Liquidsoap image or script becomes pending and `liquidsoap` is not restarted; a changed Loki config hash recreates `loki`; `maintenance` asks for `MAINTENANCE`; restore asks for `RESTORE` and stops `backend` first; install below 1.0.0 refused; `update.bat <tag>` with a missing image stops; `update.bat <b> <f>` with different MAJOR.MINOR stops; a frontend tag defaults to the backend tag; a newer target migrates, an older one does not; an older target below `min_app_version` is refused and the boundary dumps are offered; a missing `schema_compat` table counts as 1.0.0; an older target below the script's `min-backend` is refused; `history` gets an entry per install; retention keeps boundary dumps and the recorded dump; backup writes both copies and an unwritable Windows copy is a warning; `radio-network` in NAT mode writes the `eth0` address and recreates `backend` only when it changed, and in mirrored mode writes `127.0.0.1`; a missing media mount is reported by `status`; first run skips the air check and the dump and does not start `liquidsoap`; first run with a volume and no state stops; a failed first run does not roll back; `cutover` checks its preconditions, asks for `CUTOVER`, and `--undo` stops `liquidsoap`; Docker or Compose version different from `versions.env` stops; `--dry-run` changes nothing; the lock blocks a second run.
10. **Pester tests** of `install.ps1` under Windows PowerShell 5.1 on a Windows runner, with `wsl.exe`, `dism.exe`, `powercfg.exe`, `w32tm.exe`, `Register-ScheduledTask`, the registry, and downloads mocked. Cases: a build below 19044 or without the Store WSL stops; Windows 10 19044+ chooses NAT mode, prints the support note, and writes `localhostForwarding`; Windows 11 22621+ chooses mirrored mode; virtualization off stops; the media folder must exist on a local NTFS drive, its WSL path is derived correctly (drive letter, spaces, Cyrillic names), and the Defender exclusion is added; Docker Desktop present stops; wrong account offers `-CreateAccount`; features already enabled skip the reboot; features enabled now register the resume task and ask to reboot; `-Resume` continues at the recorded step and removes the resume task; WSL below the minimum runs `wsl --update`; `.wslconfig` merge keeps unknown keys and sets the chosen network mode; an existing distribution is kept; a rootfs checksum mismatch stops; `setup-distro.sh` failure stops with its log; the keep-alive task has the startup and repetition triggers, no time limit, "run whether logged on or not", and `IgnoreNew`; the password is never written to the log; active hours are set to the operator's window (at most 18 hours) and no Pro-only policy is written; the edition and the newest KB with its date are logged and an old patch level only warns; a second run changes nothing; `-DryRun` changes nothing.
11. `scripts/check-line-endings.ps1` (CRLF for `*.bat`/`*.ps1`, LF for everything else, in particular `bin/radioctl` and `*.sh`).

### 9.2 Runbook (`README.md`)

- Before the install: Windows 10 21H2+ (19044) with current updates or Windows 11 (Home is enough), the support-status and security note (6.1, 6.7); BIOS settings (power on after power loss, virtualization); a UPS; the `radio` account; the GHCR read token (with its expiry date); the external Icecast source password.
- Install: `install.bat` (signed in as `radio`), the feature reboot and resume, what the summary shows, the reboot test.
- Media: copy new folders into the station's Windows media folder ("Radio media"), then import the folder in the panel; never rename, move, or delete a file that is in the plan (use Replace file); the Defender exclusion and indexing; `update.bat status` for a missing drive or permission problems.
- The cut-over from the old source on `/atlantida` (6.9) and its rollback.
- Regular update, install of a given release, rollback, restore, maintenance, with what each does to the air; Windows Update and Docker Engine upgrades in a maintenance window, followed by the reboot check.
- Backups: where they are (both copies), the timer, the monthly restore drill, the disk-loss risk; **never `wsl --unregister radio-atlantida`**.
- Stream output: changed in the panel ("Stream output (Icecast)"), applied at once with a gap of a few seconds; the request to the server owner for the UTF-8 mount charset.
- Monitoring: `http://127.0.0.1:${GRAFANA_PORT}`, admin login, the dashboards, `MONITORING_ENABLED`, `grafana cli admin reset-admin-password` (through `update.bat compose exec grafana ...`), changing `GRAFANA_DB_PASSWORD`.
- Troubleshooting: the stack did not start after boot (keep-alive task history, `logs/boot-*.log`), the panel does not open (localhost forwarding or mirrored networking, `wsl --status`), live voice does not connect (`runtime/network.env`, the VM address), registry login, health timeout, Liquidsoap not connected to the external server (password, TLS, mount busy), non-Latin titles shown as `*`.

### 9.3 Dry run (no staging)

There is no staging machine. The dry run of a release, and of every change to this repository, has two parts:

1. **Linux side on the developer's Mac (Apple Silicon)**: an arm64 Ubuntu 24.04 VM with systemd (for example OrbStack or UTM) where `setup-distro.sh` runs as on the production distribution (in NAT-like mode: the browser on the Mac reaches the VM by its address, which `radio-network.service` writes as on Windows 10), with a media folder on the VM's disk, then `radioctl first-run`, an update to the next release, an older release, rollback, restore, maintenance, backup, restore drill, and cut-over against a test Icecast (the dev stack's `icecast` with a TLS proxy, or a test mount agreed with the server owner). The application images are multi-arch (§10.1, B10, F4), so the VM pulls native arm64 images with the same tags and digests as production. Until the first multi-arch release, or for an amd64 parity run, `DOCKER_DEFAULT_PLATFORM=linux/amd64` set for `radioctl` forces amd64 under emulation (Liquidsoap timing is then not representative); it is a dry-run switch only, not an `.env` setting.
2. **Before a release with migrations:** restore a recent production dump in the VM and run `migrate up` of the new release against it, with the previous release's backend running (as step 7 of 7.3). For a release with a contract migration, also check that `min_app_version` was raised and that `update.bat <previous tag>` is refused.

What the Mac cannot test, checked on Windows (the D4 acceptance run, and again after a Windows update or a WSL or Docker Engine upgrade): `install.ps1` itself (features, reboot and resume, `wsl --import`, `.wslconfig`), WSL started by the task without a sign-in, idle shutdown and the keep-alive, WSL's localhost forwarding and the UDP path to the VM address (or mirrored networking on Windows 11), the Windows media folder through the drive mount (permissions, speed, names, Defender), Windows power and update settings, the Windows backup copy on `/mnt/c`, clock agreement, the amd64 images on the real CPU, and the reboot test.

**Done when:** CI is green on a PR; the dry run passes; the D4 Windows acceptance run passes; the runbook lets someone who has never seen the system install, update, and recover it.

## 10. Releases, versions, and migrations

### 10.1 Release file and image facts

- Backend and frontend each release with a Git tag `vX.Y.Z` on `main`; the image tag is `X.Y.Z`, immutable (the release workflow refuses an existing tag), private in GHCR (backend `.github/workflows/release.yml:61-72`, frontend `release.yml`). Today they are built for `linux/amd64` only; cross-repo tasks B10 and F4 publish **one multi-arch manifest list per tag with `linux/amd64` (the production target) and `linux/arm64`** (the developer's Mac), so Docker pulls the right architecture by itself. No tags exist yet in either repository.
- Every pinned infrastructure image has `linux/arm64` as well (checked 2026-11-12 with `docker buildx imagetools inspect`): `postgres:17.11`, `savonet/liquidsoap:v2.4.6`, `grafana/grafana:13.2.3`, `grafana/loki:3.7.8`, `grafana/alloy:v1.20.1`, `libretime/icecast:2.4.4-alpine` (dev only), and the build bases `golang:1.27.4-trixie`, `debian:trixie-20261005-slim`, `node:24.21.0-alpine`, `nginxinc/nginx-unprivileged:1.29.8-alpine`.
- `release.env` (committed) names the release this repository installs by default: `BACKEND_TAG`, `BACKEND_DIGEST`, `FRONTEND_TAG`, `FRONTEND_DIGEST`. A release of the station is a PR that changes this file (and, when needed, `config/` from the backend at the new tag), checked by CI.
- **The digest is the manifest list's** (the index digest `docker buildx imagetools inspect` prints for the tag), not a per-architecture digest. One value per image is valid on every architecture, so production (amd64) and the dry run (arm64) use the same `release.env`; it changes whenever any platform's image changes, so it pins the whole release; per-architecture digests would double the file and make the tool choose by platform. `state.json` records the index digest (what the tool pins and rolls back to) and, for diagnostics, the platform and image ID that were pulled.

### 10.2 Separate backend and frontend tags

`BACKEND_TAG` and `FRONTEND_TAG` may differ: a frontend-only fix does not force a backend release, and the other way round. The rule: same MAJOR.MINOR (an API change bumps at least MINOR in the backend). Today the frontend release refuses a tag whose `api/openapi.json` `info.version` is not equal to the tag (frontend `release.yml:43-52`), so a frontend-only patch release is impossible; relaxing it is cross-repo task F1. Until F1 is done, releases use equal tags.

### 10.3 Migration rules

- The first release, **1.0.0, is the baseline**. Migration `0013` is not expand-only: it deletes clocks, the grid, and the plan, and assumes the backend is stopped (`internal/migrations/0013_clock_playlists_inserts.up.sql:4-6`). It runs only on the first install. No install below 1.0.0.
- From 1.0.0 on, every release's migrations are **expand-only** and safe to run while the previous release runs (7.3 step 7): add tables, nullable columns, indexes created concurrently where tables are large; no rename, no drop, no type change of a column in use.
- **Contract** (dropping what an older release used) comes no earlier than release N+1 after the release N that stopped using it, and the same migration sets `schema_compat.min_app_version` to N (§10.4). Any other migration that an older release cannot work with (a changed meaning of a column, a new NOT NULL without default that old code does not write) raises it the same way.
- Every migration file runs in one implicit transaction (golang-migrate sends it as one simple-protocol query; checked 2026-11-12), so a failure, including the 10 s lock timeout, rolls the whole file back; the backend then resets the version (B7). Files with explicit transaction control are rejected by the backend's CI; a statement that cannot run in a transaction (`CREATE INDEX CONCURRENTLY`, `VACUUM`) is the only statement of a file marked `-- migrate: non-transactional`, reviewed, and its failure takes the dump-restore path (7.3 step 7).
- Destructive migrations are reviewed before release and marked in the release notes (annotated tag line `migrations: none|expand|contract`, backend B8). The release notes say whether a release has migrations, so the dry-run rehearsal (9.3 item 2) is not missed.
- The tool relies on `migrations.Up` accepting a newer schema (downgrade) and on the 10 s migration lock timeout (no stall of the running backend).

### 10.4 Install of any release and the downgrade guard (cross-repo B9)

- `update.bat <backendTag> [<frontendTag>]` installs any release ≥ 1.0.0. A newer target is a normal update (7.3). An older target replaces only the images; the schema stays, because every release from 1.0.0 on works with a newer schema until a contract migration says otherwise.
- **`schema_compat`**: a one-row table created by an expand migration in the first release that ships B9, seeded with `min_app_version = '1.0.0'`. A contract migration updates it to the oldest release that works with the schema after it. A missing table means 1.0.0 (releases before B9).
- **Backend guard**: `backend serve` reads `min_app_version` at startup and exits with a clear error when its own version is below it (a `dev` build skips the check), so a manual `docker compose up` of an old image cannot run against a schema it does not understand. `backend migrate check` prints the schema version, the dirty flag, `min_app_version`, and whether the binary's own version is compatible, as JSON.
- **Tool check**: before an older install the tool reads `min_app_version` directly with `psql` (7.3 step 3), so the check also works for target images that predate `migrate check`. Target below it: refuse.
- **Restore path**: `state.json` `history` records, for every install, the pre-update dump and `min_app_version` before and after. For a refused target T, the candidate restore points are the dumps of entries with `min_app_version_before` ≤ T. The tool shows the newest one, its UTC time, and that every change since then is lost (plan, play log, settings, library edits), and on typed `RESTORE` restores it (D6 task 4) and installs T.
- **Retention**: the boundary dumps (entries that raised `min_app_version`) of the last 3 boundaries are kept outside the normal 14-dump rotation (D6 task 3), so a downgrade across a recent contract migration stays possible; older boundaries need a manual decision.
- Both images of the target are compatible (same MAJOR.MINOR), and the target backend must satisfy the `min-backend` of the running Liquidsoap script; otherwise the install is refused.

## 11. Decisions and open points

### 11.1 Panel and live voice exposure — DECIDED

Localhost only: the panel on `127.0.0.1:${PANEL_PORT}`; live voice on the WSL VM address (Windows 10, NAT mode, reachable from the host only) or `127.0.0.1` (Windows 11 22H2+, mirrored), 6.5. `http://127.0.0.1` is a secure context, so the browser allows the microphone. LAN access (with HTTPS, a firewall rule, and a review of the missing authentication, design §10) is a possible future item only.

### 11.2 Production Liquidsoap script — DECIDED

Dev script at the pinned backend tag plus `production.patch`, checked in CI. The backend's dev script is developed and tested first; this repository never edits the script by hand.

### 11.3 External Icecast — DECIDED

No Icecast container in production. `https://listen.teploe.net:8001/atlantida`, TLS only (5.4). The connection is the `stream.*` settings; the source password is in `.env`.

### 11.4 Stream output applied at runtime — DECIDED (needs B1, B6)

A `stream.*` change is sent by the backend to Liquidsoap over the control link and applied by replacing the `output.icecast`, without a container restart; Liquidsoap fetches its initial output from the backend, with the last applied file and defaults as fallbacks (5.2). Verified possible on 2.4.6, including codec changes and TLS; gap about 4 s.

### 11.5 Maintenance window — DECIDED

Only for: a new Liquidsoap image, a changed `radio.liq` (contract or patch), a changed `liquidsoap` compose definition, and anything 5.2 cannot apply at runtime (nothing known today). Also, by this plan's choice, a new PostgreSQL image or `db` definition: recreating `db` interrupts the backend and a pending migration, and needs a dump first. Docker Engine, WSL, and Windows updates are done in the same kind of window. A normal update reports pending items; `update.bat maintenance` applies them after confirmation.

### 11.6 Install of any release — DECIDED

`update.bat` installs `release.env`; `update.bat <backendTag> [<frontendTag>]` installs any release ≥ 1.0.0, newer or older, guarded by `schema_compat` (§10.4); `update.bat rollback` installs the recorded previous release.

### 11.7 Version compatibility — DEFAULT

Backend and frontend: same MAJOR.MINOR until the frontend declares a minimum backend version (F2). Script: `min-backend` header ≤ the backend tag.

### 11.8 Line endings — DECIDED

`*.bat` and `*.ps1` CRLF, everything else LF (bash scripts must be LF).

### 11.9 Scripting languages — DECIDED

Windows PowerShell 5.1 for `install.ps1` and the wrappers; bash inside the distribution for `radioctl` and the Linux half of the install (7.1).

### 11.10 Registry access — DECIDED

A fine-grained read-only token for the `radio-atlantida-*` packages, only in the production `.env` and the `radio` user's Docker credentials file. The token shared in plain text during planning must be revoked. The expiry date is recorded in the runbook.

### 11.11 Monitoring — DECIDED

Grafana optional (`MONITORING_ENABLED`), login required, `127.0.0.1` only, reads PostgreSQL as `grafana_ro`. Loki and Alloy are core services. Monitoring failures are warnings, never a rollback. Configuration files are copies of the backend `dev/` files at the pinned tag.

### 11.12 Backups — DECIDED

On the machine only: a dump before every update and a daily dump with a journal copy at 04:30 station time (DEFAULT, outside the nightly full pass of the plan at 03:00, `definitions.go:230-231`), by a systemd timer in the distribution (DEFAULT; a Windows task calling into WSL is the alternative), copied to `C:\RadioAtlantida\backups` as well, 14 daily and 14 update dumps kept plus the last 3 boundary dumps, 24 h RPO, monthly restore drill. No off-site copy, risk accepted.

### 11.13 CI access to the backend repository — DECIDED

The backend repository is public; the sync check reads it at the tag without a token.

### 11.14 Cut-over and first format — DECIDED

The developer stops the current source on `/atlantida` and switches over by hand with `radioctl cutover`; rollback is `cutover --undo` and restarting the old source (6.9). The first-install format is MP3, 160 kbps, 44100 Hz, stereo, matching the current stream; it stays a panel setting.

### 11.15 Metadata charset — DECIDED

The script sends UTF-8 (default `encoding`). The server owner is asked to set `<charset>UTF-8</charset>` on `/atlantida`; until then non-Latin titles show as `*`, which is accepted. `encoding="ISO-8859-1"` stays a documented fallback only (5.4).

### 11.16 Host platform — DECIDED

Docker Engine in a dedicated WSL2 distribution, no Docker Desktop, on the production machine's Windows 10 (19044+, Store WSL), with Windows 11 supported and recommended later; a dedicated local administrator account `radio` owns the distribution and runs the keep-alive task without a sign-in; NAT mode on Windows 10 (localhost forwarding for TCP, the VM address for live voice UDP written at boot), mirrored mode chosen automatically on Windows 11 22H2+ (6.5). The marked Windows behaviours are verified in the D4 acceptance run; the fallback for the boot task is automatic sign-in (6.4).

### 11.17 Windows edition and update policy — DECIDED

The production machine is most likely Windows 10 Home, most likely without ESU (confirm on the machine: `install.ps1` logs the edition, the newest installed KB and its date, and the ESU status). The default path uses only what Home allows: active hours over the broadcast day (at most 18 hours), no update policy. Without ESU no cumulative updates arrive, so update-driven reboots are unlikely; with ESU, the runbook's "Pause updates" and manual installs in a maintenance window apply (6.7). The host is unpatched and is not used for email or browsing; a Windows 11 upgrade is recommended later.

### 11.18 Multi-arch images and the dry run — DECIDED

The developer's Mac is Apple Silicon. Application images are published as one multi-arch manifest list per tag (amd64 main target, arm64 for the Mac; B10, F4); `release.env` pins the manifest list digest (§10.1); the dry run runs native arm64. `DOCKER_DEFAULT_PLATFORM=linux/amd64` is a dry-run switch for an amd64 parity run only.

### 11.19 Deploy repository access — DECIDED

The deploy repository is public; the installer clones it and `radioctl` pulls it without a token.

### 11.20 Media folder on Windows — DECIDED

The station's existing Windows folder of tracks is the media root, through WSL's drive mount with fixed owner, group, and group write, bind-mounted into the backend (read-write) and Liquidsoap (read-only); the import works in place and nothing watches the folder, so the drive mount's limits do not matter. Copying into ext4 (6.6 option b) is the fallback if the Windows checks fail; a copying import (option c) is not supported by the backend.

## 12. Cross-repo tasks

Backend (`radio-atlantida-backend`):

- **B1 (prerequisite of D3).** Runtime stream output: in `dev/liquidsoap/radio.liq`, the `output.set` telnet command and the startup fetch with fallbacks (5.2), one encoder branch per codec × channels, `shutdown()` before the new output, a short retry delay; in the backend, a `GET /api/playout/output` endpoint with the resolved `stream.*` values (no secret), sending `output.set` on every `stream.*` save and in the resync on a new control connection, alerting on a failed apply. Dev compose: `BACKEND_OUTPUT_URL` and the `liquidsoap-state` volume. A dev check like phase-21's, including a codec change. **Done** (backend Phase A, commits e2b9178, 7214870; the manual dev check A10 is the backend's last open item of the phase).
- **B2.** Contract header in the dev script (`radio-atlantida-contract: N`, `min-backend: X.Y.Z`), bumped whenever the telnet commands, callback payload, journal format, emergency or output endpoints change. **Done** (e2b9178: contract 1, `min-backend: 1.0.0`).
- **B3.** `liquidsoap --check` and a short runtime run of the dev script in the backend CI, so the script is checked where it is developed. **Done** (7214870: `make liquidsoap-smoke`, CI job `liquidsoap`).
- **B4.** `CLAUDE.md` lists `golang:1.27.2-trixie` (line 184); the Dockerfile and `go.mod` use 1.27.4 (`docker/Dockerfile:4`, `go.mod:3`). Resolved as a `CLAUDE.md` update in backend step D1, still open.
- **B5.** `stream.loudness_target_lufs` and `stream.hls_enabled` are settings that nothing applies to the output; decide whether the script applies the loudness target (normalisation) and whether HLS stays a setting.
- **B6 (prerequisite of the first air date).** TLS to the stream server: a setting `stream.icecast_tls` (bool, reconnect), passed in `output.set` and used by the script (`transport=http.transport.tls()`), and the mount check over `https` when it is on. **Done** (6b270a5, e2b9178).
- **B7 (required, before the first release).** Automatic recovery of a failed migration: CI lint for transaction-unsafe files and the `-- migrate: non-transactional` marker; `backend migrate up` resets the version after a transactional failure and exits `4` ("rolled back, schema unchanged at version N"), exits `5` when the database is dirty; `backend migrate target-compat` prints the `min_app_version` the image's migrations set (7.3 step 7).
- **B8 (decided).** Annotated release tags carry a line `migrations: none|expand|contract`; the release notes repeat it (10.3).
- **B10 (before the first release).** Multi-arch backend image: `release.yml` publishes one manifest list per tag for `linux/amd64` and `linux/arm64`. Recommended: build each architecture natively (`ubuntu-24.04` and `ubuntu-24.04-arm` runners, free for public repositories), because the runtime stage runs `apt-get install ffmpeg` and would need QEMU on a single runner; push by digest, then `docker buildx imagetools create` the list under `X.Y.Z`. The Go stage can also cross-compile (`FROM --platform=$BUILDPLATFORM`, `GOOS=$TARGETOS GOARCH=$TARGETARCH`, `CGO_ENABLED=0` already). Smoke-test both images (`backend version`, `openapi` equal on both; the attached `openapi.json` from amd64). The "tag is not taken" check inspects the manifest list (`imagetools inspect`). `ci.yml` builds both architectures. The Dockerfile's `HEALTHCHECK` and `curl` are per-arch packages, no change.
- **B12 (with B1, before the first release; decided 2026-11-12, the developer may override).** Batch settings save with encoder limit validation (decided 2026-11-12): a new endpoint saves several settings at once, all or nothing, with one change notification (one `output.set`, one reconnect). The backend refuses a `stream.codec`, `stream.bitrate_kbps`, `stream.sample_rate_hz`, or `stream.channels` value that, with the others (from the batch, else saved), violates the encoder limits (5.2 item 4), with a 422 and a field error naming the limit, so the panel shows it at save time regardless of the order of the fields. The table moves to one Go package shared by the settings validation and `dev/liqsmoke`. Backend `deploy-prereqs-plan.md` step A11.
- **B9 (before the first contract migration).** Downgrade guard (§10.4): the `schema_compat` table with `min_app_version` (expand migration, seeded `1.0.0`), the startup check in `backend serve`, the `backend migrate check` subcommand (JSON), and the rule in the migration guidelines that a contract migration raises `min_app_version`.

Frontend (`radio-atlantida-frontend`):

- **F1.** Relax the release check of `api/openapi.json` (`release.yml:43-52`) from "`info.version` equals the tag" to a compatibility check (same MAJOR.MINOR, or a declared minimum backend version), so a frontend-only fix can be released.
- **F2 (optional).** Declare the minimum backend version as an image label, so the deploy compatibility check (11.7) can read it.
- **F4 (before the first release).** Multi-arch frontend image, the same manifest list for `linux/amd64` and `linux/arm64`. The node build stage is architecture-independent: `FROM --platform=$BUILDPLATFORM node:24.21.0-alpine AS build` builds once; the nginx stage has no `RUN`, so one `docker buildx build --platform linux/amd64,linux/arm64` on an amd64 runner needs no emulation for the build. The build and the release smoke test (`/` and `/config.json`) run natively on `ubuntu-24.04` and `ubuntu-24.04-arm` runners (the repository is public), as in the backend's B10. The "tag is not taken" check inspects the manifest list.
- **F3 (with B1).** The settings page text for "Stream output (Icecast)": the change applies at once with a gap of a few seconds in the stream. The settings page saves all changed settings in one call to the batch endpoint (B12) and shows a refusal, the encoder limit included, on the refused field at save time; on a refusal nothing is saved and the form keeps every edit. Needs B12.

Frontend status (2026-11-12): F1, F2, F4 done (commits a458e18, 8de99b5; the CI image job is now `docker (linux/amd64)` / `docker (linux/arm64)`); F3 is unblocked (backend A1 and A8 are in).

## 13. tech-design.md changes

The design changes this plan needed are applied in `tech-design.md` v7-24 (2026-11-12): §1.1 (separate tags, digests, deploy contents including `install.bat` and `radioctl`), §3 (stream server row), §6.3 (reconnect is not a restart), §7.1 (mount check over HTTPS), §8.8 (runtime output, TLS, charset findings and decision, production server and first format), §9 (no production Icecast, published ports, health checks), §9.1 (Windows 10 or 11 host with Docker Engine in a dedicated WSL2 distribution, autostart without sign-in, NAT or mirrored networking, the media folder on Windows and the other data on ext4, database volume, Windows 10 support status), §9.2 (multi-arch images, release file, migrations baseline and downgrade guard, install and update tooling, steps, effect on air), §10 (access, backups with the systemd timer and the Windows copy), §10.3 (reference), §10.4 (journal location), §11 (items 5 and 6, new items 32 to 36).

v7-25 (2026-11-12, backend Phase A findings): §6.3 (encoder limits checked at save; batch save `PATCH /api/settings`, all or nothing, one `output.set`), §8.8 (startup order with the environment, strict JSON, state file path and ownership, state written on connect, encoder limits table, AAC-LC, TLS with `http.transport.tls()` instead of `ssl()`), §11 (new item 39).

## 14. Prompt for the first Claude Code session

Before the session, the `docs` sync agent pushes `base.md`, `tech-design.md`, `backend-init-plan.md`, `frontend-init-plan.md`, and `deploy-init-plan.md` into `docs/` of the repository.

> You are starting the deployment repository of a radio automation system. It contains no application code.
>
> 1. Read `docs/base.md` for the goal of the product.
> 2. Read `docs/tech-design.md`, especially §1.1, §3.1, §6.3, §8, §9, and §10. It is the authority.
> 3. Read `docs/deploy-init-plan.md`. It is the task list. The dev stack it must match is the code of `radio-atlantida-backend` (`docker-compose.dev.yml`, `dev/`).
>
> Implement Phase D1 of `docs/deploy-init-plan.md`. Follow Sections 1 and 1.1. Apply the DEFAULT items of Section 11. There are no open ASK items; if a new one appears, do not guess: list it in your final message and stop before the phase that needs it. Do not start D2. Do not commit; the developer commits. Finish with `git check-attr` confirming the line endings, `.env` ignored, and a short summary of what was created.
