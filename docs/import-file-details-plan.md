# Import file details: backend plan

## Goal

The Import page shows what each file is before the manager imports it:

- Files that already have a track (`in_library`, `original`): the track's title, artist, duration, and technical facts, in the listing itself, from the database. No file is read.
- New files: the title and artist guessed from the file name, in the listing itself. No file is read.
- New files on request: one file is probed (ffprobe header read) when the manager expands its row. The response has the duration, the technical facts, the tags, and the title and artist the import would give it.

Loudness and true peak are out of scope: they need a full decode. The analysis that runs with the import measures them, as today.

## Current state

- `GET /api/import/files` (`internal/httpapi/handlers/ingest.go`, service `internal/ingest/folders.go` `Files()`) walks the folder with stat only (`internal/media/files.go` `Walk`, `media.File{Path, Size, ModTime}`), at most 2,000 files, and sets the status from `libraryIndex` (`TrackPaths`, `internal/repository/ingest/ingest.go`: path and id only).
- DTO `ImportFile` (`internal/httpapi/dto/ingest.go`): `path, size, modified_at, status, track_id?`.
- `libraryIndex` is shared with `Folders()` (whole-tree counts) and `runImport`; those must stay as cheap as today.
- Import naming (`internal/ingest/import.go`, `internal/ingest/names.go`): title = title tag (`library.CleanTitle`), else the file-name part; artist = form `artist_id`, else artist tag, else the file-name part. `nameParts`: exactly one hyphen (`-`, en dash, em dash) with text on both sides gives "Artist - Title", otherwise the whole name is the title and there is no artist.
- Probe: `internal/media/probe.go` (`Prober.Probe`, `parseProbe`): duration, format, codec, bitrate, sample rate, channels, title and artist tags; 10 s timeout.
- Quality check: `internal/ingest/quality.go` `Check` over `Measurements` against `Standard`.
- `docs/phase-18-plan.md` decision 4: the folder counts compare paths only, without reading files. This stays true. Decision 11 (one media operation at a time) does not apply: the probe is not an operation.

## Design

### 1. Listing: track summary for known files

- New repository query used only by `Files()`, e.g. `TrackSummaries(ctx, folder)`: the same path prefix range as `TrackPaths`, joined with `artists`, returning per track: `id, file_path, title, artist name, duration_ms, air_duration_ms, format, codec, bitrate, sample_rate, channels, quality_issues, reviewed_at`. `Folders()` and `runImport` keep `TrackPaths`.
- `FolderFile` gets an optional `Track *TrackSummary`, set for `in_library` and `original` (for `original` it is the transcoded copy's track, the one `track_id` points to).
- DTO: `ImportFile.track` (optional object `ImportFileTrack`) with the fields above (`artist` nullable, `quality_issues` an array, `reviewed_at` nullable). The description says that for `original` the facts describe the library copy, not this file.

### 2. Listing: file-name guess for new files

- For `status = new`, the service runs the import's file-name rule on the base name without extension and sets `name_title` and `name_artist` (nullable). Same function as the import uses (export or reuse `nameParts`, no copy of the rule).
- DTO: `ImportFile.name_title` (string, only for `new`) and `ImportFile.name_artist` (nullable). The description says the import prefers the tags and the form's artist; these are the fallback.

### 3. Probe one file: `GET /api/import/file-info?path=<relative path>`

- Path rules as for the listing and the import: relative to the media root, checked with the existing path validation and opened through the root (`os.Root`), no `..`, no absolute paths. Not a file / not found → 404 problem with a code; not an importable extension → 422 with `not_audio` code (reuse existing codes if they exist).
- Runs `Prober.Probe` (10 s timeout). Not a media operation; it does not block or wait for imports. Bounded concurrency (a small semaphore in `ingest.Service`, e.g. 2) so many expanded rows do not load the CPU next to playout; a waiting request respects its context.
- Response `ImportFileInfo`:
  - `path, size, modified_at`
  - `duration_ms, format, codec, bitrate, sample_rate, channels`
  - `tags: { title, artist }` (nullable each)
  - `title, title_source` (`tag | file_name`), `artist, artist_source` (`tag | file_name | none`) computed by the same code path as the import (one helper shared with `import.go`, not a copy). The form's `artist_id` is not known here; the description says it overrides `artist`.
  - `issues`: what `Check` can tell without loudness (format, sample rate, channels, low bitrate), with the same issue codes as track `quality_issues`. If `Check` cannot run without loudness, split out the non-loudness part rather than faking loudness values.
- Probe failure (ffprobe error, timeout, unreadable file) → a problem with a code (e.g. `probe_failed`, added to `errcodes`) and the detail logged; it does not raise an `error.alert`. Without ffprobe on the host (`make run` on macOS) → `media_tools_unavailable`, as import refuses today.
- The probe runs synchronously in the request (`r.Context()` with the probe's own 10 s timeout), so no goroutine is started per request; the semaphore only caps how many run at once. If the project prefers it, use a `workerpool` instead and say so in the report.
- No cache. The import still probes on its own.

## Steps

1. Repository: `TrackSummaries` query (sqlc or the project's style) and its test.
2. Service (`internal/ingest/folders.go`): fill `Track` for known files and `NameTitle` / `NameArtist` for new ones; extract the shared naming helper from `import.go` / `names.go` so the listing, `file-info`, and the import use one function. Tests: summary for `in_library` and `original`, guess for `new` (one hyphen, en dash, no hyphen, two hyphens), nothing for `not_audio`.
3. Service: `FileInfo(ctx, path)` with the semaphore, path checks, probe, effective names, and issues. Tests with a fake prober: tag wins over name, name fallback, no artist, issues, probe error, invalid path, not audio, not found.
4. Handlers and DTOs (`internal/httpapi/handlers/ingest.go`, `internal/httpapi/dto/ingest.go`): new fields and the new endpoint with OpenAPI descriptions; handler tests.
5. Docs: update `docs/tech-design.md` (import listing fields, the new endpoint) in the backend; note the change to phase-18 decision 5 (listing fields) if that plan is kept current.
6. No schema change (no migration). `make fmt`, `make lint`, `make test`, `make test-integration` (the new repository query), `make openapi-lint`.

## Report

- The DTO fields and the endpoint as implemented, error codes used, the semaphore size.
- How the naming helper is shared with the import.
- Command results.
