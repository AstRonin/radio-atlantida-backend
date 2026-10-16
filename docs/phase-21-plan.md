# Phase 21 Plan — On-Air Titles, Reviewed Mark, Bulk Rename (radio-atlantida-backend)

Status: Implemented
Based on: `tech-design.md` (§6 track list, §8.1 requests), `docs/phase-9-plan.md` (Liquidsoap requests), `docs/phase-11-plan.md` (analysis, transcode), `docs/phase-18-plan.md` (import in place), `docs/phase-19-plan.md` (bulk delete), `docs/phase-20-plan.md` (sort), frontend `docs/f19-track-titles-review-weekdays-plan.md`
Last updated: 2026-10-16

## 1. Goal

Imported files often carry broken titles (wrong tag encoding, `01_track_final(2)`, `Title [www.site.com]`). The operator corrects them in the panel, but today the correction never reaches listeners: the requests pushed to Liquidsoap carry only `item_id` (or `track_id` for an emergency track) and the path (`internal/playout/pusher.go` `RequestURI`, `internal/playout/callbacks.go`), so Liquidsoap sends Icecast the **tags of the file**.

This phase:

1. sends the library title and artist with every request, so the stream metadata is what the operator entered;
2. adds a "reviewed" mark to tracks, set and cleared in bulk, so after a bulk import the operator sees which tracks are checked; any change of the track's content clears it;
3. adds a bulk find-and-replace on titles, with a preview.

The weekday "All" checkbox of the frontend plan needs no backend change.

## 2. Decisions (2026-10-16)

1. **Metadata from the library, never from the file.** Each request is annotated with `title` and `artist` (separate fields, as Liquidsoap and Icecast expect; Icecast shows "Artist - Title" for MP3 streams, the title alone without an artist, as for jingles). `artist` is always sent, empty for a track without an artist: without it the file's artist tag reaches the stream (dev check, §3.1). Applies to plan items and emergency tracks.
2. **Quoting.** One helper in `internal/liquidsoap` builds an annotation pair: the value in double quotes, `\` and `"` escaped, control characters removed. All annotations (`item_id`, `track_id`, `liq_cue_*`, `title`, `artist`) go through it, so a title cannot break the request.
3. **The annotation must win over the file tag.** Verified on the dev stack with the pinned Liquidsoap image (§3.1): it does, so the Liquidsoap scripts do not change.
4. **Already queued items keep the old title.** A save affects requests pushed after it; items already in `hour_0`/`hour_1` (the lookahead and the pre-pushed next hour) are not re-pushed. Documented in the API description of `PUT /api/tracks/{id}`.
5. **Title rule** (create, update, bulk rename, import): trimmed; runs of whitespace collapsed to one space; control characters refused on the API (field `title`, code `invalid_characters`) and removed on import; 1–500 characters after cleaning.
6. **Reviewed mark is a timestamp**, `tracks.reviewed_at timestamptz NULL`: the panel shows when the track was checked. One operator, no "by whom". It is separate from `quality_ok` (the analysis result with its manual override).
7. **Set and cleared in bulk only:** `POST /api/tracks/review` (`review-tracks`), up to 1,000 tracks, like bulk delete. `PUT /api/tracks/{id}` does not take the field. `reviewed: true` sets `now()` on tracks not reviewed yet (a repeated mark keeps the first date); `false` clears it.
8. **Any content change clears the mark** (operator's answer to Q2), in the same statement as the change:
   - `PUT /api/tracks/{id}` when `title`, `artist_id`, or `file_path` differs from the stored value (playlists, type, notes, quality override, and play rules do not clear it);
   - bulk rename, for each track whose title changes;
   - Fix loudness / Optimize (transcode, both output modes) and any analysis that saves a **different checksum** (a file replaced on disk, re-read by import in place or by the analyser);
   - an analysis of an unchanged file keeps it.
9. **Bulk rename is literal find-and-replace**, no regular expressions: `find` (1–200 chars), `replace` (0–200), `match_case` (default false). Every occurrence in the title is replaced, then the title rule (decision 5) is applied, so removing `[www.site.com]` leaves no double space. Two steps with the same endpoint: `dry_run: true` returns the preview and writes nothing; `dry_run: false` applies it in one transaction. A track whose new title would be empty or over 500 characters is refused, not cut; a track whose title does not change is listed as unchanged.
10. **Filters and sort.** `GET /api/tracks` gains `reviewed=true|false` and the sort key `reviewed` (by `reviewed_at`; not reviewed last in both orders, as the other keys of phase 20).
11. **No new WebSocket event.** The panel reloads after its own requests; the reviewed mark is not a broadcast fact.

## 3. Work

### 3.1 Titles on air

- `internal/liquidsoap/annotate.go`: `Annotate(uri string, pairs ...Pair) string`, with tests for `"`, `\`, newline, tab, Cyrillic, emoji.
- `internal/playout/playout.go` `Item`: `Title string`, `Artist string`.
- `internal/repository/playout/playout.go` `Items`: select `t.title` and `a.name` (`LEFT JOIN artists a ON a.id = t.artist_id`).
- `RequestURI`: `item_id`, the cue annotations, `title`, `artist` (empty when the track has none, decision 1), through the helper.
- `callbacks.go` emergency request: the same, from the emergency track row (`trackColumns` already has `t.title`, `artist_name`).
- **Dev check:** a file with ID3 title "WRONG" and library title "Right"; play it from the plan and as an emergency track; `curl http://localhost:8000/status-json.xsl` shows `title` "Right" (and the artist). Result written into this plan; script change if needed (decision 3).
- **Dev check result (2026-10-16, `savonet/liquidsoap:v2.4.6`, `libretime/icecast:2.4.4-alpine`).** A 40 s tone tagged title `WRONG`, artist `WRONGARTIST`, pushed over telnet into the hour queue on air:
  - `annotate:item_id="0",title="Right",artist="Right Artist":…` → Icecast `title` "Right Artist - Right": the annotation wins over the tags; no script change.
  - `title="Jingle only",artist=""` → "Jingle only": an empty artist hides the tag. Without an `artist` annotation the tag came through ("WRONGARTIST - …"), hence decision 1.
  - `title="Q \"x\" \\ b: c, d=e"` → `Q "x" \ b: c, d=e`: the escaping of the helper is what the annotate parser reads; `:`, `,` and `=` inside the quotes are plain text.
  - `title="Привет 🎵 café"` → Icecast shows `****** * café`. Liquidsoap sends the metadata correctly (`/admin/metadata?…&charset=UTF-8&song=%D0%9F…`, captured with a fake Icecast); Icecast 2.4 converts MP3 metadata to the mount's charset, ISO-8859-1 by default, and replaces what does not fit. Setting `encoding="UTF-8"` on `output.icecast` changes nothing (it is the default). See §7.
  - The emergency source resolves its request with `request.create` from the same text, so it reads the same annotations.
- `internal/library/tracks.go` `prepareTrack`: the title rule (decision 5); `internal/ingest/import.go`: the same cleaning (removal, not refusal) on the tag or file-name title.

### 3.2 Reviewed mark

- Migration `0011_track_reviewed.up.sql` (forward-only, like every migration: no `.down.sql`, §9.2 of the design): `ALTER TABLE tracks ADD COLUMN reviewed_at timestamptz;` Partial index only if the `reviewed=false` filter measures above 50 ms on 30,000 tracks (phase 20 rule). Measured (PostgreSQL 17 in Docker Desktop, Apple M1 Pro, 30,000 tracks, a third not reviewed): `reviewed=false` first page 7 ms, sort `reviewed` desc last page 31 ms, marking 1,000 tracks 15 ms. No index.
- `library.Track.ReviewedAt *time.Time`; DTO `Track.reviewed_at` (`format: date-time`, absent when not reviewed).
- `library.Service.ReviewTracks(ctx, ids []uint64, reviewed bool) (ReviewTracksResult, error)`: checks 1..1000 unique IDs (as `checkDeleteTracks`); one `UPDATE ... SET reviewed_at = CASE WHEN $2 THEN COALESCE(reviewed_at, now()) END WHERE id = ANY($1) RETURNING id`; IDs not returned are `not_found`.
- HTTP `POST /api/tracks/review`: body `{ track_ids: int64[1..1000, unique], reviewed: bool }` → `200 { updated: int64[], not_found: int64[] }` (request order).
- Clearing (decision 8):
  - `repository/library` `Tracks.Update`: `reviewed_at = CASE WHEN title IS DISTINCT FROM ? OR artist_id IS DISTINCT FROM ? OR file_path IS DISTINCT FROM ? THEN NULL ELSE reviewed_at END`, in the same `UPDATE` (no read-then-write race);
  - `repository/ingest` `SaveAnalysis`: `reviewed_at = CASE WHEN file_checksum IS DISTINCT FROM $3 THEN NULL ELSE reviewed_at END`. This covers transcode (it saves through `SaveAnalysis` with the new checksum) and a file replaced on disk.
- `TrackFilter.Reviewed *bool`, `SortReviewed`; the `WHERE` builder and the sort map of phase 20; `listTracksInput` `reviewed` (a string enum `true`/`false`, mapped to `*bool`: the API has no other optional boolean query parameter) and `sort` enum + `reviewed`.

### 3.3 Bulk rename

- `library.Service.RenameTracks(ctx, RenameRequest) (RenameResult, error)`:
  ```go
  type RenameRequest struct {
  	TrackIDs  []uint64 // 1..1000, unique
  	Find      string   // 1..200, not blank
  	Replace   string   // 0..200, no control characters
  	MatchCase bool
  	DryRun    bool
  }
  type RenameResult struct {
  	Changed   []TitleChange   // track_id, old_title, new_title (request order)
  	Unchanged []uint64
  	Refused   []RenameRefusal // track_id, code: track_not_found | title_empty | title_too_long
  }
  ```
- Replacement: `strings.ReplaceAll` when `MatchCase`, else a case-insensitive literal match (`regexp.QuoteMeta` with `(?i)`), then the title rule.
- Apply: read the titles `FOR UPDATE` in one transaction, compute, `UPDATE tracks SET title = v.title, reviewed_at = NULL FROM (VALUES ...)` for the changed ones. The applied result is computed again in the transaction (the titles may have changed since the preview), so the response always shows what was written.
- HTTP `POST /api/tracks/rename` (`rename-tracks`): body as above in snake case (`track_ids`, `find`, `replace`, `match_case`, `dry_run`), response `{ changed, unchanged, refused }`. Validation errors on `find`, `replace`, `track_ids`. `dry_run` is required, so a client that forgets it gets a 422 instead of a write; `replace` and `match_case` may be left out. `title_empty` and `title_too_long` are new codes in `errcodes` (published in `ProblemCode`, like the refusal codes of a bulk delete).

### 3.4 Contract and docs

- `make openapi`; descriptions: `PUT /api/tracks/{id}` (decision 4 and the mark clearing), `Track.reviewed_at`, `review-tracks`, `rename-tracks`, the `reviewed` filter and sort key.
- `tech-design.md`: §8.1 annotations include `title` and `artist`, stream metadata comes from the library; §6 track list: the reviewed mark, its clearing rule, the two bulk endpoints. Copy to the frontend and deploy repositories.

## 4. Files

| File | Change |
|------|--------|
| `internal/liquidsoap/annotate.go` | annotation quoting helper |
| `internal/playout/playout.go`, `pusher.go`, `callbacks.go` | `Item.Title/Artist`; annotations through the helper |
| `internal/repository/playout/playout.go` | title and artist in `Items` |
| `dev/liquidsoap/radio.liq` | unchanged: the annotations win (§3.1) |
| `internal/migrations/0011_track_reviewed.up.sql` | `reviewed_at` |
| `internal/library/tracks.go`, `titles.go` | `ReviewedAt`, `ReviewTracks`, filter and sort; title rule (`CleanTitle`) and `RenameTracks` |
| `internal/repository/library/tracks.go` | `reviewed_at` column, clearing in `Update`, review and rename statements, filter, sort key |
| `internal/repository/ingest/ingest.go` | clearing in `SaveAnalysis` |
| `internal/ingest/import.go` | title cleaning on import |
| `internal/httpapi/handlers/tracks.go`, `dto/library.go` | `review-tracks`, `rename-tracks`, `reviewed` filter and sort, `Track.reviewed_at` |
| `api/openapi.json` | `make openapi` |
| `docs/tech-design.md` | §6, §8.1 |
| `CLAUDE.md` | Phase 21 in the required-reading list |

## 5. Tests

- **Unit:** annotation quoting; `RequestURI` with title, artist, no artist, truncated item; title rule (trim, collapse, control characters → 422 `invalid_characters`, blank after cleaning → required); rename computation (case-insensitive, match case, several occurrences, result empty → `title_empty`, over 500 → `title_too_long`, no occurrence → unchanged, double spaces collapsed); review and rename ID checks (0 and 1,001 IDs, duplicates → 422).
- **Integration, repository:** migration up; `ReviewTracks` sets, keeps the first date, clears, reports not found; `Update` clears the mark on a title, artist, or path change and keeps it on a playlist, type, or notes change; `SaveAnalysis` clears it on a new checksum and keeps it on the same one; transcode of a reviewed track clears it; `RenameTracks` dry run writes nothing, apply writes the titles and clears the mark only on changed tracks, in one transaction; `reviewed` filter and sort key with `total` (phase 20 fixture).
- **Integration, playout:** `Items` returns title and artist; the pushed request text for an item and for an emergency track.
- **HTTP:** both endpoints, the filter, the sort key; the OpenAPI route list test.
- **Dev stack (manual):** the check of §3.1.

## 6. Done when

- A track renamed in the panel is heard with the new title and artist in VLC and on the Icecast status page, for items pushed after the save and for emergency tracks; a jingle without an artist shows its title alone.
- `POST /api/tracks/review` marks 1,000 tracks in one request; `GET /api/tracks?reviewed=false` lists only the others with the right `total`.
- Renaming a reviewed track, or running Fix loudness / Optimize on it, clears its mark; adding it to a playlist does not.
- `POST /api/tracks/rename` with `find: " [www.site.com]"`, `replace: ""`, `dry_run: true` shows the new titles and changes nothing; with `dry_run: false` writes them.
- `make lint`, `make test`, `make test-integration`, `make openapi-lint` pass.

## 7. Known limits

- Queued items keep the title they were pushed with (decision 4).
- **Non-Latin titles on an MP3 stream need the Icecast mount charset.** Icecast 2.4 stores and serves MP3 metadata in the mount's `<charset>`, ISO-8859-1 by default, so Cyrillic, Armenian, and emoji show as `*` (Latin accents survive). The backend and Liquidsoap send UTF-8; the fix is `<charset>UTF-8</charset>` in the `<mount>` of the Icecast configuration (deploy's `config/` for the test stack, and the station's production server). The dev Icecast runs the image's default configuration and keeps the limit.
- The file tags on disk are not rewritten; another player reading the files directly sees the old tags.

## 8. Out of scope

- Regular expressions or several rules in one rename.
- Renaming artists in bulk (artists are renamed on the artists page, which changes every track of the artist).
- Who reviewed, review notes, or review history.
- Rewriting the tags of the audio files.
