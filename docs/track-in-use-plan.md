# Track in use on the track detail: backend plan

## Goal

The track edit page must disable "Replace file" before the request when the track is in use by a clock insert, a one-time insert, or a plan item that has not aired. Today the page cannot know this: the track response has no usage data, so the refusal (409 `track_in_use`) is only seen after the click.

This plan adds one field to the track detail response. The replace rule itself does not change.

## Decisions

- New field `in_use` on the `Track` schema: boolean, nullable.
  - `true`: the track is used by a clock insert (timed or one-time), or by a plan item that has not aired. This is the same check `FileInUse` runs for replace (`internal/library/tracks.go`, built on `tracks.Users` and `tracks.PlannedHours`).
  - `false`: none of those.
  - `null`: not computed. Only `GET /api/tracks/{id}` (and the responses that already return a single track, see below) sets it. Lists (`GET /api/tracks`) leave it null, so list pages do not pay for the extra queries.
- Queued in Liquidsoap or on air is NOT part of `in_use`. The frontend already has `playout_state` on the track, and the backend refusal for those stays as it is.
- Playlist membership does not set `in_use` (same as replace).
- Responses that return one track and set `in_use`: `GET /api/tracks/{id}`. PUT and the replace response keep returning `Track` with `in_use` null, unless the implementation finds it cheap to compute there too. Decide in step 1 and state the choice in the report. Recommendation: GET only.
- The field is documented in the OpenAPI description: "null in lists; computed on the track detail".

## Steps

1. Read `internal/httpapi/dto` (the track response type), `internal/httpapi/handlers/tracks.go` (the GET single-track operation, and the list), `internal/library/tracks.go` (`FileInUse`, `Users`, `PlannedHours`), and the repository methods behind them. Find the single place that builds the `Track` response from a row, and check whether list and detail share it.
2. Add `InUse *bool` (JSON `in_use`, `omitempty` off so null is explicit) to the response DTO. Keep the field optional in the schema.
3. In the single-track GET handler only, call the existing in-use lookups (the same ones `FileInUse` calls), and set `in_use` to true or false. No new SQL unless an existing lookup cannot answer it without loading all users; if so, add the smallest repository query with `EXPLAIN ANALYZE` and a comment.
4. Lists: `in_use` stays null. Confirm with a test that `GET /api/tracks` does not run the lookups.
5. Tests:
   - unit: the handler maps the lookup result to `in_use`;
   - integration (`internal/httpapi`): GET of a track used by a timed clock insert returns `in_use: true`; of an unused track, `false`; GET list returns `in_use: null` for all items; a track used only by a played-out plan item (already aired) returns `false`;
   - repository integration: the lookup used by the handler matches `FileInUse` for the same fixtures.
6. Run `make fmt`, `make lint`, `make test`, `make test-integration` (Docker is available), `make openapi`, and `make openapi-lint`. Report the actual exit codes.

## Constraints

- No git commit, push, branch or state change.
- No permissions and no change log (none exist).
- No schema migration is expected. If one seems needed, stop and report.
- Do not touch the frontend or docs repositories.

## Done when

- `GET /api/tracks/{id}` returns `in_use` true or false for a track, and null in lists.
- The OpenAPI document shows the field with its description.
- All checks above pass, with their exit codes reported.
