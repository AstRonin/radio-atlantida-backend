# Phase 22 Plan — Track Cue Points, Waveform, Usage Marks (radio-atlantida-backend)

Status: Implemented in the backend (2026-10-16); emergency dev check and the deploy script copy pending (§10)
Based on: `tech-design.md` (§4 `scheduled_items` and `play_log`, §6 track list, §7 plan, §8.1 requests, §8.5 fades), `docs/phase-9-plan.md` (Liquidsoap requests), `docs/phase-11-plan.md` (analysis, transcode), `docs/phase-20-plan.md` (list filters and sort), `docs/phase-21-plan.md` (annotations, reviewed mark), frontend `docs/f20-track-cue-editor-plan.md`
Last updated: 2026-10-16

## 1. Goal

Live recordings end with applause, some files start with noise or talk. The operator wants to cut the edges of a track in the panel, on a waveform, without touching the file, and to see which tracks are in use.

This phase:

1. stores a start and stop point (cue in, cue out) and fade lengths per track; the planner fits the track by its on-air length, and Liquidsoap plays only that part, with the fades;
2. serves the waveform of a track (min and max amplitude per short bucket) for the panel's editor;
3. shows in the track list whether a track is planned (next planned start), queued, or on air, and filters tracks without a playlist.

## 2. Decisions (2026-10-16, operator)

1. **Cue points, never trimming.** The file is not changed. Liquidsoap applies `liq_cue_in` / `liq_cue_out`, already used for the in point and for truncated items (`internal/playout/pusher.go` `RequestURI`). A destructive trim is out of scope.
2. **Fades at the cue points.** A track stores `fade_in_ms` and `fade_out_ms` (0 = hard cut). Two new settings hold the defaults the panel proposes when the operator moves a point off the file edge: `liquidsoap.cue_fade_in_ms` (1000) and `liquidsoap.cue_fade_out_ms` (2000), 0 to 5000. The backend never fills them in on its own.
3. **On-air length.** `air_duration_ms = cue_out (or duration_ms) − cue_in`. The planner, reschedule, filler, and emergency now-playing use it instead of `duration_ms`. `duration_ms` stays the length of the file (analysis fact).
4. **A planned track may be changed; a queued or playing one may not.** `PUT /api/tracks/{id}/cue` is refused with 409 `track_on_air` while the track has a `queued` plan item or is on air (plan item or emergency). Otherwise the save is accepted, and when cue in or cue out changed, the future hours that contain a `planned` item of the track are rebuilt (`RebuildRange`, a `clock_build` operation); the response carries its `build_operation_id`. `queued`, `aired`, and `missed` items are never touched (tech-design §7). A change of fades only needs no rebuild: fades are read from the track when the request is pushed.
5. **Plan items keep the cut they were planned with.** `scheduled_items.planned_in_ms` / `planned_out_ms` are the cut; the pusher sends them, not the track's current cue. Between the save and the rebuild a pushed item uses the old cut. `play_log` gains `planned_in_ms`, so lengths stay correct after airing.
6. **Cue editing is reviewing.** A cue change does **not** clear `reviewed_at` (unlike title, artist, or file, phase 21 decision 8). The panel sends `POST /api/tracks/review` after the save when the operator leaves "Mark as reviewed" ticked.
7. **A new file keeps a cue that still fits.** When an analysis saves a different checksum (transcode, file replaced on disk), the cue is kept if it is valid for the new duration; otherwise all four cue fields are reset in the same statement and the analysis result names `cue_reset`.
8. **Waveform on demand, cached.** Computed with ffmpeg on the first `GET /api/tracks/{id}/waveform`, stored with the file checksum, and reused until the checksum changes. Not computed at import: only tracks opened in the editor pay for it.
9. **Usage marks from the backend.** `Track.next_planned_at` (earliest `planned` or `queued` item) and `Track.playout_state` (`on_air`, `queued`, absent). Filters `in_plan`, `in_playlist=false`; sort key `next_planned_at`. No new WebSocket event: the panel reloads on the playout and plan events it already receives.
10. **Fades in Liquidsoap.** The script adds `fade.in` with `override_duration="liq_fade_in"` next to the existing `fade.out` on the hour queues, and both on the emergency source; to be checked on the pinned image (§3.2).

## 3. Work

### 3.1 Data

- Migration `0012_track_cue.up.sql` (forward-only):
  ```sql
  ALTER TABLE tracks
    ADD COLUMN cue_in_ms   integer NOT NULL DEFAULT 0,
    ADD COLUMN cue_out_ms  integer,
    ADD COLUMN fade_in_ms  integer NOT NULL DEFAULT 0,
    ADD COLUMN fade_out_ms integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT ck_tracks_cue CHECK (cue_in_ms >= 0 AND (cue_out_ms IS NULL OR cue_out_ms > cue_in_ms)),
    ADD CONSTRAINT ck_tracks_fades CHECK (fade_in_ms BETWEEN 0 AND 5000 AND fade_out_ms BETWEEN 0 AND 5000);
  ALTER TABLE play_log ADD COLUMN planned_in_ms integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT ck_play_log_planned_in_ms CHECK (planned_in_ms >= 0);
  CREATE TABLE track_waveforms (
    track_id      bigint      NOT NULL,   -- pk_track_waveforms, fk_track_waveforms_track_id ON DELETE CASCADE
    file_checksum text        NOT NULL,
    bucket_ms     integer     NOT NULL,
    peaks         bytea       NOT NULL,   -- int8 pairs: min, max per bucket
    duration_ms   integer     NOT NULL,   -- the decoded length, served with the peaks
    created_at    timestamptz NOT NULL DEFAULT now());
  ```
  The checks against `duration_ms` are in the service (the duration changes with the file).
- **Apply the migration** to the dev database right after writing it: `make migrate` (the compose `migrate` service does the same for `--profile app`); check with `psql` that the columns, the constraint, and `track_waveforms` exist, then `make test-integration` (the migration test runs every migration on a fresh database).
- `library.Track`: `CueInMS`, `CueOutMS *int`, `FadeInMS`, `FadeOutMS`, `AirDurationMS()`.
- Settings `liquidsoap.cue_fade_in_ms`, `liquidsoap.cue_fade_out_ms` in `internal/settings/definitions.go` (int, 0–5000, applied at once, no Liquidsoap call).

### 3.2 Playout

- `repository/rotation` library snapshot: `DurationMS` is the on-air length (`COALESCE(t.cue_out_ms, t.duration_ms) - t.cue_in_ms`) and the snapshot carries `CueInMS`.
- `rotation/build.go`: an item gets `InMS = cue_in`, `OutMS = cue_in + air length`; a crossing item gets `OutMS = cue_in + (target − cursor)`. `repository/rotation/plan.go` writes `planned_in_ms`. Reschedule uses the same snapshot.
- `repository/playout` `Items`: add `t.fade_in_ms`, `t.fade_out_ms`. `End`: the planned length is `planned_out_ms − planned_in_ms` (the comment "the planner never sets an in point" goes). The move to `play_log` copies `planned_in_ms`; `repository/outage` likewise for missed items.
- `RequestURI`: `liq_cue_in` when `InMS > 0` (as now); `liq_cue_out` when the item is truncated **or** `OutMS` is before the end of the file; `liq_fade_in` when the track's fade in > 0; `liq_fade_out` = `fade_out_ms` setting for a truncated item, else the track's fade out when > 0.
- `callbacks.go` emergency request: the track's cue and fades; now playing `OutMS` is the on-air length.
- `airwindow`: played rows use `planned_out_ms − planned_in_ms`; now playing carries `InMS`.
- DTOs: `planned_in_ms` next to every `planned_out_ms` (track plan, missed items, played tracks, air window), and `in_ms` next to `out_ms` in now playing. Descriptions: "Out point from the start of the file; the length is `planned_out_ms − planned_in_ms`."
- `dev/liquidsoap/radio.liq`: `hours = fade.in(duration=0., override_duration="liq_fade_in", hours)` before the existing `fade.out`; the same pair on the emergency source. Copy to deploy's production script.
- **Dev check** (pinned `savonet/liquidsoap:v2.4.6`): a 60 s tone with a click at 0 s and 50 s; push it with `liq_cue_in="5."`, `liq_cue_out="45."`, `liq_fade_in="1."`, `liq_fade_out="2."`; record the stream and confirm by ear and in the recording: starts at 5 s with a 1 s rise, ends at 45 s after a 2 s fade, the clicks are not heard. Then an emergency track with the same annotations. Result written into this plan.

### 3.3 Cue endpoint

- `library.Service.SetCue(ctx, id, Cue) (SetCueResult, error)`:
  - validation (`validation.Error`, per field): track analysed (`duration_ms` set, else 409 `track_not_analysed`); `0 ≤ cue_in_ms`; `cue_out_ms` null or `cue_in_ms < cue_out_ms ≤ duration_ms` (`cue_out_ms`: `cue_order`, `cue_beyond_end`); air length ≥ 5000 ms (`air_too_short`); `fade_in_ms`, `fade_out_ms` 0–5000; `fade_in_ms + fade_out_ms ≤` air length (`fades_too_long`);
  - one transaction: `SELECT ... FOR UPDATE` of the track; refuse `track_on_air` when a `scheduled_items` row of the track is `queued` or the now-playing track is this one (injected `OnAir` interface implemented by `playout`, small and consumer-defined); update the four columns; read the hour range of its `planned` items;
  - the result names the range of the plan hours that hold a planned item when cue in or out changed; after the commit the handler starts `rotation.RebuildRange` on it, as the clock and grid handlers start theirs (no `PlanRebuilder` in `library`), and its ID goes into the response.
- HTTP `PUT /api/tracks/{id}/cue` (`set-track-cue`): body `{ cue_in_ms, cue_out_ms (nullable), fade_in_ms, fade_out_ms }`, all required → `200 { track, build_operation_id? }`; 404, 409 `track_on_air` / `track_not_analysed`, 422 per field. `PUT /api/tracks/{id}` does not take the fields and keeps them.
- `Track` DTO: `cue_in_ms`, `cue_out_ms` (absent = end of file), `fade_in_ms`, `fade_out_ms`, `air_duration_ms` (absent when not analysed).
- New codes in `errcodes`: `track_on_air`, `track_not_analysed`, `waveform_unavailable`, `cue_order`, `cue_beyond_end`, `air_too_short`, `fades_too_long`.

### 3.4 File changes keep or reset the cue

- `repository/ingest` `SaveAnalysis`: when the checksum differs and the stored cue is invalid for the new duration (`cue_out_ms > $duration` or air length < 5000), reset `cue_in_ms`, `cue_out_ms`, `fade_in_ms`, `fade_out_ms` in the same `UPDATE` and return that it did; the analysis and transcode results list the track under `cue_reset` (translated by the panel). A valid cue is kept.

### 3.5 Waveform

- `media.FFmpeg.Peaks(ctx, rel, bucketMS) ([]int8, durationMS, error)`: `ffmpeg -v error -i <file> -ac 1 -ar 22050 -f s16le -` read as a stream (no whole-file buffer); per bucket of 50 ms the min and max sample scaled to −127..127. A 5-minute track gives 6,000 buckets, 12 KB.
- `ingest.Service.Waveform(ctx, trackID)`: the stored row when its checksum is the track's; else compute, store (upsert), return. At most 2 ffmpeg runs at a time (a semaphore owned by the service, waited for with the request context; the 30 s route timeout bounds it). Not a media operation: it reads the file only and runs beside import or transcode (a transcode replaces the file by rename).
- HTTP `GET /api/tracks/{id}/waveform` (`get-track-waveform`): `200 { duration_ms, bucket_ms, peaks: int[] }` (flat `[min0, max0, min1, max1, ...]`); 404; 409 `waveform_unavailable` when the file is missing or unreadable; 503 `media_tools_unavailable` without ffmpeg. `Cache-Control: no-cache` with an `ETag` of the checksum, so the browser reuses it.

### 3.6 Usage marks

- `repository/library` list query: `next_planned_at` from `LEFT JOIN LATERAL (SELECT min(planned_start_utc) FROM scheduled_items WHERE track_id = t.id AND status IN ('planned','queued'))`, and `queued` from `EXISTS (... status = 'queued')`; uses the index `scheduled_items (track_id, planned_start_utc DESC)`.
- `library.Service.ListTracks`: `playout_state` = `on_air` when `OnAir` names the track, `queued` when the row says so, else absent.
- `TrackFilter.InPlan *bool` (`EXISTS` of a planned or queued item), `TrackFilter.InPlaylist *bool` (`EXISTS` of a `track_playlists` row); sort key `next_planned_at` (unplanned last in both orders, as phase 20). Query parameters `in_plan`, `in_playlist` as string enums `true` / `false`, as `reviewed` in phase 21.
- Measure on 30,000 tracks and a full 48 h plan (phase 20 rule: index only above 50 ms); results written here.
- `GET /api/tracks/{id}` carries the same two fields.

### 3.7 Contract and docs

- `make openapi`; descriptions for the new fields, endpoints, filters, and the meaning of `planned_in_ms`.
- `tech-design.md`: §4 (`tracks` cue columns, `play_log.planned_in_ms`, `track_waveforms`), §6 (cue endpoint, waveform, usage fields, filters), §7 (on-air length in the fit, rebuild after a cue change), §8.1 and §8.5 (annotations and fades). Copy to the frontend and deploy repositories.

## 4. Files

| File | Change |
|------|--------|
| `internal/migrations/0012_track_cue.up.sql` | cue columns, `play_log.planned_in_ms`, `track_waveforms` |
| `internal/settings/definitions.go` | `liquidsoap.cue_fade_in_ms`, `liquidsoap.cue_fade_out_ms` |
| `internal/library/tracks.go`, `cue.go` | cue fields, `SetCue`, `OnAir` and `PlanRebuilder` interfaces, usage fields and filters |
| `internal/repository/library/tracks.go` | cue columns, `SetCue` transaction, usage subqueries, filters, sort key |
| `internal/rotation/build.go`, `reschedule.go`, `rotation.go` | in and out points from the cue |
| `internal/repository/rotation/library.go`, `plan.go` | on-air length and cue in in the snapshot; `planned_in_ms` written |
| `internal/playout/pusher.go`, `callbacks.go`, `playout.go` | annotations with cue and fades; emergency cue; `OnAir` |
| `internal/repository/playout/playout.go` | fades in `Items`; `End` length; `planned_in_ms` to `play_log` |
| `internal/repository/outage/outage.go` | `planned_in_ms` for missed items |
| `internal/airwindow/*`, `internal/repository/airwindow/airwindow.go` | lengths from in and out |
| `internal/media/ffmpeg.go` | `Peaks` |
| `internal/ingest/waveform.go` | new: cached waveform, semaphore |
| `internal/repository/ingest/ingest.go` | waveform rows; cue reset in `SaveAnalysis` |
| `internal/httpapi/handlers/tracks.go`, `dto/library.go`, `dto/playout.go`, `dto/rotation.go`, `dto/outage.go` | endpoints, fields, filters, `planned_in_ms` / `in_ms` |
| `internal/httpapi/errcodes/*` | new codes |
| `cmd/backend/serve.go` | wiring of `OnAir` (playout) into `library`; the library service is built after playout |
| `dev/liquidsoap/radio.liq` | `fade.in` on the hour queues; fades on the emergency source |
| `api/openapi.json` | `make openapi` |
| `docs/tech-design.md` | §4, §6, §7, §8.1, §8.5 |
| `CLAUDE.md` | Phase 22 in the required-reading list |

## 5. Tests

- **Unit:** cue validation (each code, null `cue_out_ms`, fades sum, exactly 5000 ms); `AirDurationMS`; build with a cue (in and out points, a crossing item cut from the cue in, fit by on-air length); `RequestURI` for: no cue, cue in only, cue out before the end, truncated with a cue, fades with and without truncation; peaks computation on a synthetic PCM stream (silence, full scale, bucket edges, last partial bucket).
- **Integration, repository:** migration up; `SetCue` refused while an item is `queued` and while `OnAir` names the track; accepted with planned items, returns the hour range; fades only → no range; `End` marks truncated by `out − in`; `planned_in_ms` reaches `play_log` for aired and missed items; `SaveAnalysis` keeps a valid cue and resets an invalid one on a new checksum, keeps everything on the same one; the reviewed mark survives `SetCue`; usage fields, `in_plan`, `in_playlist`, sort `next_planned_at` with `total`; waveform row reused for the same checksum and replaced for a new one.
- **Integration, rotation:** a track with a 30 s cut fills a 30 s element; after `SetCue` with planned items the `clock_build` operation rebuilds those hours and leaves `queued` items as they were.
- **HTTP:** `set-track-cue` (200 with and without `build_operation_id`, 404, 409 both codes, 422 per field); `get-track-waveform` (200, ETag 304, 409, 503 with the ffmpeg fake); filters and sort; the OpenAPI route list test.
- **Dev stack (manual):** the check of §3.2.

## 6. Order of work

1. 3.1 data with `make migrate` on the dev database, and 3.5 waveform (no playout change; the panel's editor can start on real data).
2. 3.2 playout with the dev check, then 3.3 and 3.4.
3. 3.6 usage marks.
4. 3.7, then the frontend F20 with `npm run api:sync`; deploy's Liquidsoap script with the release.

## 7. Done when

- A live recording with a cue out at the last chord and a 2 s fade out ends on air there, without the applause; the track plan shows its shorter length and the clock fits it by that length.
- Saving the cue of a track planned for tomorrow returns a `build_operation_id`, and the rebuilt hours use the new cut; saving while it is queued or on air returns 409 `track_on_air`.
- `GET /api/tracks/{id}/waveform` answers within 2 s for a 5-minute MP3 the first time and from the cache after.
- `GET /api/tracks?in_plan=true` and `?in_playlist=false` list the right tracks with the right `total`; `playout_state` shows the track on air.
- Migration `0012` is applied on the dev database (`make migrate`) and runs in the deploy's `migrate` service before the backend starts.
- `make lint`, `make test`, `make test-integration`, `make openapi-lint` pass.

## 8. Known limits

- Items already queued, and items pushed between a save and the end of its rebuild, keep the old cut (decision 5).
- The waveform is a mono mix at 50 ms resolution; finer placement uses the number fields (1 ms).
- Fades are linear (Liquidsoap's default curve).

## 9. Out of scope

- Cutting or rewriting audio files; editing in the middle of a track.
- Automatic cue suggestions (ffmpeg `silencedetect`) and applause detection.
- Cue points in bulk.
- Precomputing waveforms at import.

## 10. Implementation notes (2026-10-16)

Where the work differs from the draft above, or fills a gap in it:

- **Names.** The constraints follow the naming rule: `ck_tracks_cue`, `ck_tracks_fades`, `ck_play_log_planned_in_ms`, `pk_track_waveforms`, `fk_track_waveforms_track_id`. `track_waveforms` also stores `duration_ms`, which the response serves.
- **The cue rule** (`library.Cue.Check`): the 5 s minimum applies to a cut (cue in > 0 or a cue out); a file shorter than 5 s plays whole (a jingle). `air_too_short` names `cue_out_ms` when it is set, else `cue_in_ms`; `fades_too_long` names `fade_out_ms`; a cue in at or past the end is `cue_in_ms` / `cue_beyond_end`.
- **A new duration by hand.** `PUT /api/tracks/{id}` can change `duration_ms` (entered, or read from the file). It applies decision 7: a cue that still fits is kept, one that does not is reset in the same statement. The reset rule is one SQL condition in `repository/library` and `repository/ingest` (copied), the same as `Cue.Check`; a track without a cue is never reported reset. The rotation snapshot also leaves out a track whose stored cue does not fit its duration.
- **Rebuild.** The handler of `set-track-cue` starts the rebuild, like the clock and grid handlers; the library returns the hour range (`PlannedHours`: the built hours that hold a planned item of the track, from the first to the end of the last). A planned item in an hour already on air keeps its old cut (hours that have started are never rebuilt).
- **Now playing.** `out_ms` stays the out point in the file and `in_ms` is new, for plan items and emergency tracks alike (an emergency track: its cue out, else its duration), so the length is always `out_ms − in_ms`. The air window has no out point in its rows; its end times use `out − in` internally.
- **`cue_reset`.** The analysis and transcode summaries end with `; cue_reset N: [paths]` (at most five paths, as the failed names); each reset is a WARN with the track ID.
- **Waveform scale.** After a loudness fix a compressed master can peak well below full scale (−8.6 dBTP at −16 LUFS on the dev stack), so its waveform looked small; it was the new file's. The response carries `max_peak` (0 to 127), and the panel draws the waveform scaled to it (decided 2026-10-16). The dB scale of the axis is drawn by the panel: dBFS = 20 × log10(|v| / 127).
- **Usage marks** are one `LEFT JOIN` of the plan aggregated per track (hundreds of rows) instead of a subquery per track, for the columns, the `in_plan` filter, and the sort key; `in_playlist` is an `EXISTS` on `pk_track_playlists`.

**Measurements** (PostgreSQL 17.11 in Docker Desktop on an Apple M1 Pro, wall time of the list call): 30,000 tracks with 32-character titles, 2,000 artists, a 48-hour plan of 720 items. First page 21 ms; `in_plan=true` 11 ms; `in_playlist=false` 14 ms; sort `next_planned_at` first page 18 ms, desc last page 80 ms: the plan join is 8 ms of it (HashAggregate of the plan, Hash Left Join), the rest is the title tie-break, as in the title order's last page on the same table (72 ms). No index: the key is an aggregate. The waveform of a five-minute 320 kbps MP3 (6,000 buckets): 330 ms.

**Dev check** (§3.2, `savonet/liquidsoap:v2.4.6`, the stream recorded from Icecast and its envelope measured every 250 ms):

- *Found:* with the script as it was (Phase 9), plan items aired as **silence**: `fade.out` with the default `track_sensitive=false` starts one fade on the first frame of the source and never resets it; with `duration=0.` the gain is 0 from then on. A plain 20 s item and a truncated 5 s item recorded silent, the safety tone before and after. The production script in `radio-atlantida-deploy/config/` has the same line and must take the fix.
- *Found:* `fade.in` reads `liq_fade_in` in `on_metadata` but starts in `on_track`; for a track that starts after an empty queue the metadata comes second and the fade is lost (the tone came in at once). The script uses its own fade in (`cue_fade_in`), which takes the duration from the metadata `on_track` passes, and `fade.out(..., track_sensitive=true)`.
- *Result:* a 60 s tone with clicks at 0 s and 50 s, pushed with `liq_cue_in="5."`, `liq_cue_out="45."`, `liq_fade_in="1."`, `liq_fade_out="2."`: on air for 40 s, rising over 1 s from the start, falling over 2 s to the end; no click (no peak above the tone in any window). Back to back: cue only (10 s, hard edges), cue with fade in, cue with fade out, fades without a cut (whole 20 s): each as annotated.
- *Pending:* the emergency source with the same annotations end to end (the dev backend on `:8090` was the old build during the check). The emergency source uses the same two fade operators.
