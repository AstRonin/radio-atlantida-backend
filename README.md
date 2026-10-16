# radio-atlantida-backend

Go backend of the Radio Atlantida automation system: REST API, WebSocket hub, rotation engine, Liquidsoap control, outage handling.

Design documents are in [`docs/`](docs/): requirements (`base.md`), technical design (`tech-design.md`, the authority), and the initialization plan (`backend-init-plan.md`).

## Requirements

- Go (version in `go.mod`)
- Docker Desktop (the `docker` CLI on `PATH`)
- `golangci-lint` v2 and `govulncheck` for `make lint` / `make vuln`

## Quick start

```sh
make stack-up        # creates .env from .env.example, starts db + icecast + liquidsoap
make media           # generates test MP3s into .dev/media
make migrate         # applies the database migrations (backend migrate up)
make run             # starts the backend on 127.0.0.1:8090
curl localhost:8090/readyz
```

Listen to the stream at <http://127.0.0.1:8000/radio> (a quiet 440 Hz safety tone until tracks are queued). Queue a test track by hand:

```sh
printf 'request.push /media/jingle-20s-a.mp3\nquit\n' | nc 127.0.0.1 1234
```

Every track start and end is appended to `.dev/journal/journal-YYYY-MM-DD.jsonl`.

To run the backend in a container instead of on the host (the `migrate` service applies the migrations first, and `backend` starts only after it succeeds):

```sh
docker compose -f docker-compose.dev.yml --profile app up --build
```

### Migrations

Migrations live in `internal/migrations` as `NNNN_name.up.sql`, are embedded in the binary, and are forward-only: there are no down files and no `migrate down`. An applied migration is never edited; a fix is a new migration (expand and contract, §9.2 of `docs/tech-design.md`).

- `backend migrate up` needs only `DATABASE_URL`. It logs each applied version and exits non-zero on failure.
- A database newer than the binary (after a rollback to an earlier release) is accepted with a warning, so the older `backend` still starts.
- A "dirty" state means a migration failed part-way. Restore the pre-update dump (`update.bat restore` in production; in development, recreate the `pgdata` volume) and ship a fixed migration.

### Host names for `make run`

The backend reaches Icecast and Liquidsoap through admin settings whose defaults are the compose service names (`icecast`, `liquidsoap`). For a backend started on the host, add this line to `/etc/hosts` (the ports are published on `127.0.0.1`):

```
127.0.0.1 icecast liquidsoap
```

## Configuration

Only these come from the environment (or `.env`, which never overrides variables already set). Everything else is an admin setting in the database.

| Variable | Required | Purpose |
|----------|----------|---------|
| `DATABASE_URL` | yes | PostgreSQL connection string |
| `MEDIA_ROOT` | yes | Directory with the audio files |
| `LOG_ROOT` | yes | Directory for daily logs `app-YYYY-MM-DD.log` (UTC; kept for the `retention.log_days` setting, default 10) |
| `HTTP_ADDR` | no | Listen address, default `127.0.0.1:8090` |
| `ICECAST_SOURCE_PASSWORD`, `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD` | later phases | Secrets; never logged or returned by the API (`GET /api/settings/secrets` reports only whether each is set) |

### Admin settings

Every other setting (station timezone, stream output, Liquidsoap, rotation, outage detection, retention, emergency playlist) is defined in code in `internal/settings` with a typed default, limits, and an apply mode (`immediate` or `reconnect`). Only values the manager saves are stored (table `settings`); an unsaved key reads as its default.

```sh
curl localhost:8090/api/settings                       # every key: value, default, is_default, limits, apply
curl -X PUT localhost:8090/api/settings/liquidsoap.fade_out_ms \
     -H 'Content-Type: application/json' -d '{"value": 2700}'
```

A refused value returns `422` with a field error on `value`. Every saved change is logged at INFO with its UTC time.

### Library

Playlist types, playlists (with their placement rule in clocks), artists, tracks, and play rules are edited through the API (`make openapi` writes the full contract to `api/openapi.json`):

```sh
curl localhost:8090/api/playlist-types                       # music, jingle, news, segment + custom types
curl -X POST localhost:8090/api/playlists -H 'Content-Type: application/json' \
     -d '{"name": "Pop", "type_id": 1}'
curl -X POST localhost:8090/api/tracks -H 'Content-Type: application/json' \
     -d '{"title": "Test", "file_path": "music-3m-a.mp3", "type_id": 1, "playlist_ids": [1]}'
curl -o /dev/null -w '%{http_code}\n' -H 'Range: bytes=0-99' localhost:8090/api/tracks/1/audio   # 206
```

- A track is registered by its path relative to `MEDIA_ROOT` (forward slashes, no `..`, no absolute paths). If the file exists and `ffprobe` is on `PATH`, duration, format, bitrate, and sample rate that were left out are read from it. The runtime image has ffprobe; on a development host without it the backend logs one warning at startup and stores the metadata as entered.
- `GET /api/tracks/{id}/audio` streams the file for the panel's preview player (Range requests, no request timeout). It never reaches the air or the play log.
- Deletes that would break something are refused with `409` and a stable code (`playlist_type_in_use`, `playlist_in_use`, `play_rule_in_use`, `track_has_history`, ...); `used_by` names the blocking playlists, tracks, or clocks.
- A play rule interval (track or artist) is at most the play log retention (`retention.play_log_days` in minutes), because it is checked against aired history; separation is the play rule "Music rotation".

## Make targets

Run `make help`.
