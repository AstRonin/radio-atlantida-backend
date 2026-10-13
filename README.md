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
make run             # starts the backend on 127.0.0.1:8080
curl localhost:8080/readyz
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

From Phase 4 the backend reaches Icecast and Liquidsoap through admin settings whose defaults are the compose service names (`icecast`, `liquidsoap`). For a backend started on the host, add this line to `/etc/hosts` (the ports are published on `127.0.0.1`):

```
127.0.0.1 icecast liquidsoap
```

## Configuration

Only these come from the environment (or `.env`, which never overrides variables already set). Everything else is an admin setting in the database.

| Variable | Required | Purpose |
|----------|----------|---------|
| `DATABASE_URL` | yes | PostgreSQL connection string |
| `MEDIA_ROOT` | yes | Directory with the audio files |
| `LOG_ROOT` | yes | Directory for daily logs `app-YYYY-MM-DD.log` (UTC, kept 10 days) |
| `HTTP_ADDR` | no | Listen address, default `127.0.0.1:8080` |
| `ICECAST_SOURCE_PASSWORD`, `LIQUIDSOAP_CONTROL_USER`, `LIQUIDSOAP_CONTROL_PASSWORD` | later phases | Secrets; never logged or returned by the API |

## Make targets

Run `make help`.
