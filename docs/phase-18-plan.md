# Phase 18 Plan — Import in Place (radio-atlantida-backend)

Status: Done
Based on: `tech-design.md` (§6, §8.7), `docs/phase-11-plan.md` (import, analysis, transcoding), frontend `docs/f16-import-in-place-plan.md`
Last updated: 2026-10-16

## 1. Goal

The manager keeps the station's audio in folders directly under the media root (for example `D:\radio\media\music`, `...\jingles`, `...\news`, `...\music\rock`) and does not want an inbox. The import registers the files where they lie: the manager chooses a folder of the media root, a type, and optionally playlists, and every audio file of that folder that is not a track yet becomes one. Nothing is moved. Importing the same folder again adds only the files that are new since the last run.

This replaces the inbox model of phase 11 (inbox folder, library folder, files moved from one to the other). Analysis, quality issues, and transcoding are unchanged.

## 2. Decisions (2026-10-16)

1. **No inbox, no library folder.** The settings `import.inbox_dir` and `import.library_dir` are removed. An import never moves, renames, or deletes a file. A track's `file_path` is the path where the file was found, relative to the media root.
2. **Folders of the media root.** `GET /api/import/folders` lists every folder under the media root, nested ones included, in path order, with counts (2.4). Hidden folders (a name starting with `.`) and symbolic links are skipped, as `media.Library.Walk` already does. At most 500 folders are listed; `more` reports that more exist.
3. **Folder paths are relative to the media root**, with forward slashes: `music`, `music/rock`, `jingles`. This is the same form as a track's `file_path`, so the folder of a track is the start of its path. An empty folder value means the media root itself (every folder).
4. **Counts per folder**, in the folder and its subfolders:
   - `audio_files`: files an import takes by name (`importable()`: audio extension, not hidden, not `.part`, `.tmp`, `~$`);
   - `new_files`: audio files that are not a track yet by path, and not the original of a transcoded copy (2.8).

   The root has the same two counts. The counts compare paths only, without reading the files, so they are cheap; a file that is new by path but has the checksum of an existing track is counted as new and reported as a duplicate by the import (2.7).
5. **Files of one folder.** `GET /api/import/files?folder=music` lists the files in that folder and its subfolders, in path order, at most 2,000 (`more`). Each file has `path` (relative to the media root), `size`, `modified_at`, and `status`:
   - `new`: an import would take it;
   - `in_library`: a track has this path, with `track_id`;
   - `original`: the original of a transcoded copy (2.8), with the `track_id` of the copy's track;
   - `not_audio`: not taken by an import.
6. **Import body.** `POST /api/import` takes `folder` (optional, absent or empty: the media root), `type_id` (required), `playlist_ids` (optional), and `artist_id` (optional, an existing artist; unknown ID: 422, field `artist_id`, rule `unknown_id`). The type, the playlists, and the artist apply to the new tracks of this run only; tracks that already exist are not changed (their type, playlists, and artist stay as the manager set them). A new artist is created on the artists page first; the import body takes an ID, not a name.
6a. **Title and artist of a new track.** The title is never a path and never has the extension; `file_path` holds the path. Both are decided per file, each on its own:
   - **Artist**: the `artist_id` of the body when given (it wins over everything); else the artist tag; else the artist part of the file name (below); else none.
   - **Title**: the title tag; else the title part of the file name (below).

   **File name rule.** The name is the file's base name without the extension (`jingles/DJ Ann - Morning.mp3` gives `DJ Ann - Morning`). The hyphens counted are `-` (U+002D), `–` (en dash, U+2013), and `—` (em dash, U+2014), because file names copied from the web use all three.
   - **Exactly one hyphen**, with text on both sides after trimming spaces: the artist is the part before it, the title the part after it (`DJ Ann - Morning` gives artist `DJ Ann`, title `Morning`; `Aira V - Ты нужен мне любой` gives artist `Aira V`). The order is artist first, title second, as music files are commonly named.
   - **No hyphen**: the title is the whole name, no artist from the name.
   - **More than one hyphen** (`Jay-Z - Song`, `Intro - part 1 - long`): the split is not obvious, so the name gives no artist and the title is the whole name.
   - **One hyphen with an empty side** (`-intro`, `Morning -`): no artist; the title is the whole name, trimmed.

   The rule is a pure function `nameParts(base string) (title, artist string)` with table tests. When the artist comes from the name or the tag, it is looked up by name ignoring case and created when missing (`EnsureArtist`, as in phase 11). Titles and artists are trimmed and cut to the form limits (500 and 200 characters) as now.
7. **One file in a run**, in this order:
   1. not `importable()`: `skipped`;
   2. a track has this path: `in_library` (no read, so re-importing a large folder is fast);
   3. the original of a transcoded copy: `in_library`;
   4. checksum; a track with the same checksum exists elsewhere (the file was copied or moved on disk), or a file earlier in this run has it: `duplicates`. The file is left alone and the log names the file and the existing track's ID and path;
   5. analysis fails: `unreadable`;
   6. otherwise the track is registered at this path, with the title and artist of 2.6a: `imported`.

   The summary counts `imported`, `in_library`, `duplicates`, `unreadable`, `skipped`, `failed`, and starts with the folder (`folder jingles: ...`, `whole media folder: ...` for the root).
8. **Originals of transcoded copies.** A transcode with output `copy` writes `<stem>.optimal.<ext>` next to the original and points the track at the copy; the original stays on disk without a track. An import must not register it again. A file `<dir>/<stem>.<any ext>` is an original when a track's `file_path` is `<dir>/<stem>.optimal.<any ext>` (the copy may have another extension, for example a FLAC original and an MP3 copy).
9. **Validation of the folder** (listing and import), a per-field refusal (`422`, field `folder`, rule `invalid`, no new code):
   - `media.ValidatePath` (no `..`, no absolute path, no backslash, clean form);
   - no hidden element;
   - the folder exists under the media root and is a directory: detail "no such folder in the media folder".
10. **Missing or moved files: only an error.** An import does not look for tracks whose file has gone, and does not follow a moved file. A missing file shows as it does today: the analysis gives `file_missing` (library filter "issues"), and the preview and playout fail with their usual errors. The manager fixes it by editing the track's file path or deleting the track. A moved file found by a later import is a `duplicate` of its old track (2.7.4), not a new track.
11. **One media operation at a time** stays: an import of any folder while another import, analysis, or transcode runs is refused with `media_operation_running`.
12. **Limits.** `maxImportFiles` (5,000 files walked per run) stays; files already in the library count toward it, so a very large folder may need several runs, and the summary says so as now.

## 3. API

```
GET /api/import/folders
200 { "root": { "audio_files": 120, "new_files": 5 },
      "folders": [ { "path": "jingles", "audio_files": 20, "new_files": 2 },
                   { "path": "music", "audio_files": 100, "new_files": 3 },
                   { "path": "music/rock", "audio_files": 40, "new_files": 0 } ],
      "more": false }

GET /api/import/files?folder=jingles
200 { "folder": "jingles",
      "items": [ { "path": "jingles/id1.mp3", "size": 1, "modified_at": "...", "status": "new" },
                 { "path": "jingles/id2.mp3", "size": 1, "modified_at": "...", "status": "in_library", "track_id": 17 } ],
      "more": false }

POST /api/import
{ "folder": "jingles", "type_id": 2, "playlist_ids": [5], "artist_id": 9 }   -> 202 { "operation_id": ... }
```

`GET /api/import/inbox` is removed (the panel is the only client and changes with this phase, frontend plan F16).

## 4. Settings

| Key | Change |
|-----|--------|
| `import.inbox_dir` | Removed |
| `import.library_dir` | Removed |

Before removing them, check how the settings service treats a stored value whose key is no longer in the registry. If it refuses or logs it on every read, migration `0010_drop_import_folders` deletes the two rows. The settings page renders the groups the backend sends, so the fields disappear there without a frontend change.

Checked 2026-10-16: `settings.Service.List` walks the registry and only looks up saved rows by key, and `Get`/`Set` refuse an unknown key as not found; a saved row of a removed key is never read or logged. No migration deletes the rows. Migration 0010 is instead `0010_tracks_file_path_prefix` (§5).

## 5. Files

| File | Change |
|------|--------|
| `internal/media/files.go` | Folder walk with counts (folders in path order, hidden and links skipped), keeping `Walk` as it is |
| `internal/ingest/import.go` | `ImportRequest.Folder`, `ImportRequest.ArtistID`; `checkImport` checks the artist; run of 2.7 without `Move`/`FreeName`; `register` with the title and artist of 2.6a; summary; no `overlap` check |
| `internal/ingest/names.go` | New: `nameParts` (2.6a) |
| `internal/ingest/folders.go` | New: `Folders(ctx)`, `Files(ctx, folder)`, folder validation, the original-of-a-copy rule (2.8) |
| `internal/ingest/ingest.go` | `Settings` without `InboxDir`, `LibraryDir`; store methods `TracksByPaths(ctx, paths)` (path to track ID) and `OptimalStems(ctx, dir)` or one query that covers both; `Files` interface without the methods an import no longer uses, if transcode does not use them either |
| `internal/repository/...` (ingest store) | The two queries, with `EXPLAIN ANALYZE` in a doc comment; an index on `tracks(file_path)` if the existing unique constraint does not serve the prefix lookup. Done as one query, `TrackPaths(ctx, folder)` (path → track ID under the folder; the copy rule of 2.8 runs on its result in Go, since a copy is always in the folder of its original), plus `ArtistExists`; `TrackByChecksum` also returns the path for the duplicate log |
| `internal/migrations/0010_tracks_file_path_prefix.up.sql` | `ix_tracks_file_path` with `text_pattern_ops`: `uq_tracks_file_path` compares in the database collation and cannot serve a prefix range |
| `internal/settings/definitions.go` | Remove the two keys |
| `internal/migrations/0010_drop_import_folders.up.sql` | Only if needed (§4) |
| `internal/httpapi/dto/ingest.go` | `ImportFolders`, `ImportFolder`, `ImportFiles`, `ImportFile`; `ImportBody.Folder`, `ImportBody.ArtistID` (doc: "An existing artist for every new track; it wins over the artist tag and the file name."); remove `Inbox`, `InboxFile` |
| `internal/httpapi/handlers/ingest.go` | `GET /api/import/folders`, `GET /api/import/files`; new description of `POST /api/import`; remove the inbox listing |
| `cmd/backend/serve.go` | Settings wiring without the two folders |
| `Makefile`, `dev/` | `make inbox` writes its files into a folder of the media root (for example `.dev/media/incoming-test`) and keeps its name or is renamed `make import-samples`. Done: `make import-samples` (`dev/scripts/gen-import-samples.sh`) writes into `.dev/media/import-samples` |
| `api/openapi.json` | `make openapi` |
| `docs/tech-design.md` | §6 endpoints, §6.3 settings, §8.7 import in place |
| `docs/phase-11-plan.md` | A note at the top: the inbox and library folders are replaced by phase 18 |
| `CLAUDE.md` | Phase 18 in the required-reading list; the `make inbox` line |

## 6. Tests

- **Unit, folder validation:** `..`, absolute, backslash, hidden element, not clean, missing folder, a file instead of a folder refused with field `folder`, rule `invalid`; empty means the root.
- **Unit, folders and files:** nested folders in path order; hidden folders and links skipped; `audio_files` and `new_files` per folder include subfolders; an empty folder is listed with zeros; the folder limit sets `more`; every file status of 2.5, with `track_id` for `in_library` and `original`.
- **Unit, original rule:** `a.flac` with a track at `a.optimal.mp3` in the same folder is an original; `a.flac` with `a.optimal.mp3` in another folder is not; `a.optimal.mp3` itself is `in_library`.
- **Unit, `nameParts` (table):** `DJ Ann - Morning` (title `Morning`, artist `DJ Ann`); `DJ Ann–Morning` and `DJ Ann — Morning` (en and em dash); `Morning` (no artist); `Jay-Z - Song` and `A - B - C` (whole name, no artist); `-intro`, `Morning -`, ` - ` (whole name trimmed, no artist); extra spaces trimmed.
- **Unit, title and artist precedence:** body `artist_id` wins over the tag and the name; the artist tag wins over the name; no tag and one hyphen gives the name's artist, created once for several files; the title tag wins over the name; the title never holds the folder or the extension.
- **Unit, import run (fake files and store):** files stay where they are and the tracks get those paths; a second run of the same folder imports nothing and counts `in_library` without reading the files; a copied file is a duplicate; type and playlists are set on new tracks only; the summary names the folder; the root imports every folder.
- **Integration (repository):** the path lookup and the copy lookup on a database with a few thousand tracks use an index.
- **HTTP, integration:** `GET /api/import/folders`, `GET /api/import/files?folder=x` with an unknown folder (422, field `folder`), `POST /api/import` with a folder; `GET /api/import/inbox` is gone (404); the settings list has no `import.inbox_dir`, `import.library_dir`.

## 7. Done when

- With `music` (3 songs) and `jingles` (2 jingles) under the media root, `GET /api/import/folders` shows both with `new_files` 3 and 2.
- `POST /api/import` with `folder: "jingles"` and the jingle type registers 2 tracks with paths `jingles/...`; the files have not moved; the folders show `new_files` 0 for `jingles`.
- A third jingle copied into `jingles` and a second import adds exactly that one.
- A file `music/DJ Ann - Morning.mp3` without tags becomes the track `Morning` by the artist `DJ Ann` (created if missing); `music/Jay-Z - Song.mp3` becomes `Jay-Z - Song` without an artist; with `artist_id` in the body every new track of the run gets that artist.
- `POST /api/import` with an unknown `artist_id` is a 422 with the field `artist_id`.
- A track whose file was deleted on disk gets `file_missing` on the next analysis; nothing else changes.
- `make lint`, `make test`, `make test-integration`, `make openapi-lint` pass.

## 8. Known limits

- A file that is still being copied under its final name (Windows Explorer copies that way) may be read half-written. Import a folder after the copy has finished. A half file that cannot be decoded is `unreadable` and stays new for the next run; one that decodes would become a short track, which the manager deletes and imports again.
- The counts walk the whole media tree on every folder listing. The panel loads it on open, on Refresh, and after an import, not on a timer.
- A moved file is not re-linked to its track automatically (decision 10).

## 9. Out of scope

- Following moved or renamed files, or removing tracks whose file is gone.
- Changing the type, playlists, or artist of tracks that already exist through an import.
- A preview of the title and artist each new file would get (it needs a tag read per file).
- Creating an artist by name in the import body.
- Another file name order (`Title - Artist`) or a setting to choose it.
- Several folders in one import, or a remembered type per folder.
- Creating, renaming, or removing folders.
- Upload through the panel.
