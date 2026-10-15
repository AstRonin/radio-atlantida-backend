# Phase 11 Plan — Import, Quality Analysis, and Transcoding

Status: Done

> **Superseded in part by `docs/phase-18-plan.md` (import in place).** The
> inbox and library folders (decisions 1, 2, the settings `import.inbox_dir`
> and `import.library_dir` of decision 11, and the inbox listing of decision
> 12) are replaced: an import registers the files of a chosen folder of the
> media root where they lie and moves nothing. `make inbox` is now
> `make import-samples`. Analysis, quality issues, and transcoding are
> unchanged.
Based on: `base.md` (Audio Quality Analysis, Audio Transcoding), `tech-design.md` (§3, §4, §6.2, §6.3, §6.5, §8.7, §10.1), `backend-init-plan.md` §8 item 4 and §9 item 11
Last updated: 2026-10-14

## 1. Goal

New audio files reach the library without typing metadata: the manager copies
them into an inbox folder on the host and starts an import. Every file is
analysed with ffprobe and ffmpeg (format, bitrate, sample rate, channels,
duration, loudness, true peak) and checked against the broadcast standard set
in the admin settings. A track that falls short shows its problems in the
panel, and the manager fixes them with one click: **Fix loudness** (level
only) or **Optimize track** (everything that a transcode can fix). The
manager chooses whether the fixed file replaces the original or is written as
an optimal copy next to it.

Phase 6 left the hooks: `tracks` has `format`, `bitrate`, `sample_rate`,
`quality_ok`, `quality_notes`, `file_checksum`; `internal/media` has the
ffprobe prober; the runtime image has ffmpeg.

## 2. Decisions (2026-10-14)

1. **Inbox scan, no upload** (decided 2026-10-14). The manager copies files
   into `MEDIA_ROOT/<import.inbox_dir>` (default `incoming`), subfolders
   allowed. `POST /api/import` scans it. There is no upload through the panel.
2. **Imported files move out of the inbox** into
   `MEDIA_ROOT/<import.library_dir>` (default `library`), keeping their path
   below the inbox; a name that is taken gets ` (2)`, ` (3)`, … before the
   extension. The inbox then shows only what is not imported yet. A file that
   fails stays in the inbox, and the result says why.
3. **The backend mounts `MEDIA_ROOT` read-write** (§8.7 allows it when files
   change through the panel). Liquidsoap keeps its read-only mount. All file
   access stays inside the root (`os.OpenRoot`/`os.OpenInRoot`, §8.7).
4. **Import of one file**, in order:
   1. Skip names that are hidden or temporary (`.`-prefixed, `~$`, `.part`,
      `.tmp`) and extensions that are not audio (`mp3`, `m4a`, `aac`, `ogg`,
      `oga`, `flac`, `wav`): reported as `skipped`.
   2. SHA-256 of the file (`file_checksum`). A track with the same checksum
      exists → `duplicate`, the file stays.
   3. Probe (format, codec, bitrate, sample rate, channels, duration, tags
      `title` and `artist`) and measure loudness (decision 6). Unreadable or
      no audio stream → `unreadable`, the file stays.
   4. Move the file (decision 2).
   5. One transaction: find the artist by name (case-insensitive) or create
      it, insert the track (title from the tag, else the file name without
      extension; type and playlists from the request), its playlists, its
      measurements and quality issues. If the transaction fails, the file is
      moved back.
   The request gives `type_id` (required) and `playlist_ids` for the whole
   batch, so jingles are imported as a separate batch. At most 5,000 files
   per run; the rest wait for the next run.
5. **Quality issues** are stored codes with a detail text, computed by a pure
   function from the measurements and the settings at analysis time.
   `quality_ok` is true when there is no issue (the manager can still
   override it on the track form; the next analysis sets it again).

   | Code | When | Fixed by |
   |------|------|----------|
   | `format` | container is not the target (`mp3` → `mp3`, `aac` → `m4a`, `vorbis` → `ogg`) | Optimize |
   | `sample_rate` | sample rate is not the target | Optimize |
   | `channels` | channel count is not the target | Optimize |
   | `loudness` | integrated loudness more than `import.loudness_tolerance_lu` from `stream.loudness_target_lufs` | Fix loudness, Optimize |
   | `true_peak` | true peak above `import.max_true_peak_dbtp` | Fix loudness, Optimize |
   | `low_bitrate` | lossy source below `import.min_bitrate_kbps` | nothing: transcoding cannot add lost detail; the panel says to replace the source file. A transcode carries the issue over to the new file |
   | `file_missing` | the file is not under `MEDIA_ROOT` (moved or deleted) | nothing |

   The track DTO lists the issues and the fixes that apply (`fix_loudness`,
   `optimize`), so the panel shows the buttons without its own rules.
6. **Loudness** is measured with the first pass of ffmpeg `loudnorm`
   (EBU R128: integrated loudness, true peak, loudness range), with
   `-threads 1`. The target is the existing `stream.loudness_target_lufs`
   (default −16): one station loudness, not a second number.
7. **Analysis of existing tracks** (`analyse` operation,
   `POST /api/tracks/analyse`). For the given tracks, or every track never
   analysed: open the file (missing → `file_missing`), hash it; when the
   checksum is unchanged and measurements exist, only the issues are
   recomputed (new thresholds apply without decoding), unless `force`;
   otherwise probe and measure again and update duration, format, codec,
   bitrate, sample rate, channels, loudness, checksum, `analyzed_at`.
8. **Transcoding is a manual action** (decided 2026-10-14;
   `transcode` operation, `POST /api/tracks/transcode`). Body: `track_ids`,
   `fix` (`loudness` or `optimize`), `output` (`replace` or `copy`).
   - `loudness`: two-pass `loudnorm` to the target, codec, sample rate and
     channels kept (lossless stays lossless; a lossy source is encoded at its
     own bitrate, rounded up to the next standard value).
   - `optimize`: `loudnorm` plus the target codec, bitrate, sample rate and
     channels (settings `import.target_*`). Tags are copied.
   - `copy` writes `<name>.optimal.<ext>` next to the original; the track
     then points at the copy, the original stays on disk and is not a track.
     A track that already points at an `.optimal` file is overwritten in
     place (as `replace`).
   - `replace` writes `<name>.<target ext>`; the original is deleted after the
     track points at the new file (only when the extension changed;
     otherwise the new file is renamed over it).
   - Output goes to a temporary file in the same folder, is synced, then
     renamed into place, so no reader ever sees a half-written file. The new
     file is analysed and the track row updated in one transaction.
   - A track is skipped (`busy`) while it is queued in Liquidsoap, on air,
     or planned to start within `playout.prepush_minutes` + 5 minutes: the
     pusher reads `file_path` when it pushes, so the path must not change
     under it. Skipped also: `nothing_to_fix` (no issue that this fix
     covers), `path_taken` (the target path belongs to another track),
     `file_missing`.
   - Future plan items keep their planned durations (an encode changes the
     length by milliseconds at most); later builds use the new duration.
9. **One media operation at a time.** `import`, `analyse`, and `transcode`
   touch the same files and are CPU-heavy on the playout machine. A second
   request while one runs is refused with 409 `media_operation_running`
   (one owner: a flag in the ingest service). Inside an operation files are
   processed by `workerpool.ForEach`: 2 workers for import and analysis,
   1 for transcoding (constants with the reason). Each ffmpeg call has its
   own deadline (probe 10 s, measure 5 min, encode 10 min). Progress events
   (`<kind>.progress`) are sent per file; the terminal event carries the
   counts per outcome. A failed file does not fail the operation; a failure
   that stops the batch (database, media root unreadable) does.
10. **ffmpeg missing** (a development host without it): the three endpoints
    refuse with 503 `media_tools_unavailable`.
11. **Settings** (new group `import`, all `immediate`):
    `import.inbox_dir` (`incoming`), `import.library_dir` (`library`) —
    clean relative paths, not inside each other; `import.min_bitrate_kbps`
    (128); `import.target_codec` (`mp3`; `mp3`, `aac`, `vorbis`);
    `import.target_bitrate_kbps` (320); `import.target_sample_rate_hz`
    (44100); `import.target_channels` (2); `import.loudness_tolerance_lu`
    (2, 0.5..6); `import.max_true_peak_dbtp` (−1, −6..0).
12. **Inbox listing** (`GET /api/import/inbox`): the files waiting in the
    inbox (relative path, size, modification time), at most 1,000, so the
    panel shows what an import would take.

## 3. Packages

| Package | Contents |
|---------|----------|
| `internal/media` | ffmpeg tools: probe with codec, channels and tags; loudness measure (`loudnorm` pass 1); encode (pass 2 with target parameters); SHA-256; inbox walk; move and atomic replace inside the root |
| `internal/ingest` | Import, analysis, transcode operations; quality rules (pure); busy check; the one-at-a-time flag |
| `internal/repository/ingest` | pgx: duplicate by checksum, artist find-or-create, track insert with playlists and measurements, analysis update, busy check, path lookup |
| `internal/library` | Track gains channels, codec, loudness, true peak, loudness range, issues, `analyzed_at`; list filter `quality` (`ok`, `issues`, `unanalysed`) |
| `internal/settings` | Group `import` |
| `docker-compose.dev.yml`, Makefile | Backend `MEDIA_ROOT` mount read-write; `make inbox` writes test files with known problems into `.dev/media/incoming` |
| CI | Installs ffmpeg for the integration tests |

Migration `0008_track_analysis.up.sql`: `tracks` columns `codec`, `channels`,
`loudness_lufs`, `true_peak_dbtp`, `loudness_range_lu`, `quality_issues`
(`text[] NOT NULL DEFAULT '{}'`, checked against the codes), `analyzed_at`;
index `ix_tracks_file_checksum`.

## 4. API

| Method and path | Purpose |
|-----------------|---------|
| `GET /api/import/inbox` | Files waiting in the inbox |
| `POST /api/import` | Body `type_id`, `playlist_ids`; `accepted` with an operation ID (`import`) |
| `POST /api/tracks/analyse` | Body `track_ids` (optional, at most 1,000), `force`; `accepted` (`analyse`) |
| `POST /api/tracks/transcode` | Body `track_ids` (1..200), `fix`, `output`; `accepted` (`transcode`) |
| `GET /api/tracks`, `GET /api/tracks/{id}` | Gain the analysis fields, `quality_issues`, `fixes`; list filter `quality` |

New problem codes: `media_operation_running`, `media_tools_unavailable`.
New operation kinds: `import`, `analyse`, `transcode` (their progress,
completed and failed events). No new dependency.

## 5. Known limits (for later phases)

- A crash between moving a file and committing its track leaves the file in
  the library folder without a track; register it by path or move it back.
- Replacing a file over a Windows bind mount (rename over an existing file)
  is to be checked on the Windows host before the first air date.
- The production compose (`radio-atlantida-deploy`) must mount `MEDIA_ROOT`
  read-write for the backend in the same release.
- A transcoded copy leaves the original on disk; deleting it is the
  manager's job.
- Changing a threshold does not re-check tracks by itself; the manager runs
  the analysis (cheap without `force`).
- The bitrate is the one ffprobe reports for the file (container size over
  duration). A VBR file of simple content (Vorbis, a test tone) can fall
  below `import.min_bitrate_kbps` and get `low_bitrate` although its quality
  setting is high.
- Cover art is not copied into a transcoded file (only the first audio
  stream and the tags).
- The import, analysis, and transcode decisions are not yet in the frontend;
  its TypeScript client gets them from the OpenAPI document.

## 6. Done when

Unit tests cover the quality rules (each code, boundaries, carried
`low_bitrate`), the ffprobe and loudnorm output parsing, target paths
(`.optimal`, collisions, extension change), the inbox walk filter, and the
import/analyse/transcode flows with fake stores and tools (outcomes, move
back on failure, busy skip, one-at-a-time). Integration tests (with ffmpeg)
measure and encode real generated files; repository tests run against
PostgreSQL; API tests cover the endpoints. On the dev stack, `make inbox`
then an import registers the files with their issues, and Optimize fixes
them. `make lint`, `make test-integration`, and `make openapi-lint` pass.

Checked on the dev stack 2026-10-14 (backend container, ffmpeg 7.1): `make
inbox` wrote six files; one import registered them in 2 s with the expected
issues (a mono 22 kHz 64 kbps file: `sample_rate`, `channels`, `loudness`,
`low_bitrate`; a WAV: `format`, `sample_rate`, `channels`, `loudness`).
Optimize into copies and Fix loudness in place brought every file to
−16 LUFS with a true peak below −15 dBTP; the 64 kbps source kept
`low_bitrate`, and a track already within tolerance was skipped as "nothing
to fix". The end-to-end API test ran in a Go container with ffmpeg; locally
without ffmpeg it checks the refusals.
