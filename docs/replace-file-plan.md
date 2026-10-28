# Replace track file: plan

## Goal

The file path and the length of a track are no longer edited by hand. A track's file is changed only through one explicit action, Replace file. The action points the track at another file under the media root, re-analyses it, and stores the result in one step.

Today `PUT /api/tracks/{id}` accepts `file_path` and `duration_ms`. A path change there does not recompute the checksum, the analysis or the waveform, so the quality fields go stale. This plan removes both fields from the update path.

Out of scope: permissions (there are none in the system) and a change log for tracks (there is none, and none is added here).

## Current state (verified in the backend, read-only)

- `internal/ingest/analyse.go:50` `Service.Analyse` is asynchronous. Per track it calls `analyseTrack(ctx, tr, std, force, t)` (`analyse.go:169`), which runs ffprobe and ffmpeg loudnorm, then `Check` (`analyse.go:215`), then `store.SaveAnalysis` (`analyse.go:219`).
- `analyseTrack` sets `SourceBitrate` to nil for an original file (`analyse.go:207-212`). A replaced file is an original, so the same rule applies.
- `internal/repository/ingest/ingest.go:14-44` `SaveAnalysis` is one UPDATE. It writes `file_path`, `file_checksum`, the measurements, the quality fields and `analyzed_at`. In the same statement it clears `reviewed_at` when the checksum changes and resets the cue if it no longer fits the duration.
- Transcode replace (`internal/ingest/transcode.go:40-68`) is the model: encode to a temp file, analyse it, `files.Commit`, `SaveAnalysis`, then remove the original. It also has the busy check (`Service.busy`, near `transcode.go:226`) and the path-taken check (`TrackByPath`, near `transcode.go:40-50`).
- Path validation: `media.ValidatePath` (`internal/media/media.go:40`) and `Library.Open` (`media/media.go:76`, uses `os.OpenInRoot`). The library service also validates with `media.ValidatePath` in `prepareTrack` (`internal/library/tracks.go:677-680`).
- Waveform is lazy (`GET /api/tracks/{id}/waveform`, `internal/ingest/waveform.go:62`). A stored waveform is valid only while its `file_checksum` equals `tracks.file_checksum` (`waveform.go:73-80`). A changed checksum invalidates it with no explicit delete.
- The PUT route is `internal/httpapi/handlers/tracks.go:184-201` (operation `update-track`), service `internal/library/tracks.go:426` `UpdateTrack`, repository `internal/repository/library/tracks.go:446` `Update`. `trackColumns` (`tracks.go:376`) writes `file_path`.

## Backend changes

### 1. New endpoint: replace the file of a track

`POST /api/tracks/{id}/replace-file`, body `{ "file_path": "music/new.mp3" }`, operation `replace-track-file`. The handler goes in `internal/httpapi/handlers/tracks.go`, next to the other track routes. Response: the updated track, as `GET /api/tracks/{id}` returns it.

Service method `ingest.Service.ReplaceFile(ctx, trackID, rel)`. It lives in the ingest service, not the library service, because it needs `analyseTrack`, the busy check and `Files` in the same way as transcode.

Steps, in order:

1. `media.ValidatePath(rel)`. Invalid path → validation error on `file_path`.
2. `s.busy` check. If an analysis, import or transcode run is in progress → `ErrRunning`, as the other ingest operations do.
3. The track exists (`TrackByID`). Otherwise → `ErrTrackNotFound`. The track is not in use for playout (see "Decided"). If it is → the in-use conflict from `*InUseError`.
4. The path is not used by another track (`TrackByPath`). If it is → conflict error, with the id of the other track.
5. The file opens and is a regular file (`Library.Open`). Missing → `file_missing` error, same as analysis uses for missing files.
6. If `rel` equals the stored `file_path` and the checksum of the file equals the stored `file_checksum`, the file is unchanged: return the track as it is, with no writes and the reviewed mark kept (no-op, see "Decided").
7. `analyseTrack(ctx, tr, std, force=true, t)` on the new file, with the path set to `rel`. This computes the checksum, the probe and loudness, the quality issues and `analyzed_at`.
8. `store.SaveAnalysis` with `FilePath: rel`. The old file is **not** removed. A track can point at a file that others also use, and deleting files is not part of this feature.

Failure behaviour: if step 6 fails, nothing is written, because `SaveAnalysis` is the only write and it runs last. If `SaveAnalysis` itself fails, the track keeps the old file and old data.

### 2. Clear the reviewed mark on a path change, not only on a checksum change

`SaveAnalysis` clears `reviewed_at` only when the checksum changes. The rule in `internal/repository/library/tracks.go:448-449` clears it on a path change too. Replace must follow the same rule: a new path always clears `reviewed_at`, even when the content is identical. Implement this by passing a flag from `ReplaceFile` to `SaveAnalysis`, or by setting `reviewed_at = NULL` in the same UPDATE when `file_path` differs from the stored value.

### 3. Remove `file_path` and `duration_ms` from PUT

- `internal/library/tracks.go` `UpdateTrack` and `prepareTrack`: do not accept `file_path` or `duration_ms` in the update input. `create-track` keeps them, because a new track needs a file.
- `internal/repository/library/tracks.go:446` `Update`: drop the `file_path` clause from the reviewed rule (`tracks.go:448-449`), keep title and artist. Drop `cueReset(in.DurationMS)` from `Update`, since duration no longer changes there.
- `internal/httpapi/handlers/tracks.go:184-201`: the request type for PUT loses the two fields. A request that sends them is rejected as an unknown field, if the decoder is strict; otherwise the fields are ignored. Check the decoder setting and state which one in the handler.

Result: after this change the only ways to change a track's file, length or quality fields are import, analyse, transcode and replace.

### 4. Waveform

No code change. The checksum changes with the file, so the stored waveform stops matching and is recomputed on next request. Add one integration test that proves this (see tests).

## Frontend changes (`radio-atlantida-frontend`, separate plan later)

- Edit track: File path and Length become read-only text. Length shows as `ms` and `m:ss` the same way it does now.
- Next to the File block, a "Replace file" button opens a dialog:
  - a text field for the path under the media root, validated the same way as the backend;
  - a warning that the reviewed mark will be cleared and the cue reset if it no longer fits;
  - on save, call `POST /api/tracks/{id}/replace-file` and show the new length, format and bitrate from the response.
- v1 uses a typed path only. A picker of files not yet in the library is a later step, because it needs a list endpoint for the media folder.

## Tests to add (backend)

Mirror the existing tests:

- `internal/ingest/ingest_test.go`: `TestReplaceFile` (happy path: new path, new checksum, new duration, cue reset when it no longer fits, reviewed mark cleared); `TestReplaceFileConflictsWithOtherTrack`; `TestReplaceFileMissingFile` (track unchanged); `TestReplaceFileWhileBusy` (`ErrRunning`); `TestReplaceFileSamePathClearsReviewed` (same path, same content, reviewed mark still cleared).
- `internal/ingest/fakes_test.go`: no new fakes expected; reuse `Files`, `Prober`, `FFmpeg`.
- `internal/repository/ingest/ingest_integration_test.go`: `TestSaveAnalysisClearsReviewedMarkOnPathChange`; a check that a stored waveform row is ignored after the checksum changes (extend `TestWaveformRows`).
- `internal/repository/library/` integration tests: `Update` no longer changes `file_path` or `duration_ms`.
- `internal/httpapi/library_integration_test.go`: PUT with `file_path` is rejected or ignored (as decided in step 3); POST replace-file returns the updated track; POST replace-file with a conflicting path returns the conflict error.

## Decided

- Replace with the same path and the same content is a no-op (step 6): nothing is analysed or written, and the reviewed mark stays. A file that was overwritten at the same path (new checksum) is analysed and clears the reviewed mark. This second case is deferred, see "Later".
- PUT with `file_path` or `duration_ms` is rejected with a validation error (not ignored), so a stale frontend build fails loudly.
- A track that is in use for playout cannot be replaced. "In use" is checked in step 3 of `ReplaceFile`, before anything is analysed. The check covers at least: a track referenced by a clock insert, a track in the rotation plan for hours not yet aired, a track queued to Liquidsoap or on air. "In use" means (a): tracks referenced by clock inserts (timed and one-time), by the rotation plan for hours not yet aired, and queued to Liquidsoap or on air. Playlist membership does not block a replace. The refusal returns a conflict that names what blocks the replace, as delete already does (`*InUseError` in the library package; reuse it, do not write a second check).

## Open questions

None.

## Later

- Replace with the same path but changed content (file overwritten on disk): re-analyse the file and clear the reviewed mark. The same `analyseTrack` and `SaveAnalysis` steps as a normal replace, with the old path kept. Implement after the first version is in use.
- Re-analysis of an unchanged file stays with the existing analyse endpoint.

## Order of work

1. Copy this plan to `radio-atlantida-backend/docs/replace-file-plan.md` (docs subagent).
2. Implement `ReplaceFile` and the handler, with the tests above.
3. Remove `file_path` and `duration_ms` from PUT, update the tests.
4. Frontend plan, after the backend change is merged.
