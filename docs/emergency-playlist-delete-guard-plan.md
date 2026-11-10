# Plan — Refuse to Delete the Emergency Playlist (radio-atlantida-backend)

Status: Written for the operator's review (2026-11-10); not approved, not started
Based on: `docs/tech-design.md` (§8.6), frontend `docs/emergency-playlist-checkbox-plan.md`
Last updated: 2026-11-10

## 1. Goal

Today a playlist is refused deletion only when a clock uses it. The playlist chosen in `playout.emergency_playlist_id` can be deleted with no warning, the setting then points at nothing, and an hour with no plan airs the safety tone.

After this change, deleting the playlist that is the current emergency playlist is refused and the refusal says why.

## 2. Decisions

1. **The check is in the backend**, in the same place and form as the clock check, so every path to the delete is covered.
2. **The operator clears or replaces the setting first.** The delete does not reset the setting by itself.
3. **Other deletes are unchanged.**

## 3. Work

- `internal/library/playlists.go` (delete, about lines 192–199): before deleting, read the current value of `playout.emergency_playlist_id` through a small interface declared by `library` and implemented from `internal/settings`, wired in `cmd`. If it equals the playlist's ID, return the existing `*InUseError` with a new entry that names the emergency playlist.
- Follow how `used_by` entries are shaped today. If the new entry adds a value to a published enum or a code in `errcodes`, add it in the same change and regenerate `api/openapi.json` with `make openapi`.
- Check the order against a concurrent delete and setting save. If the settings `Guard` does not already refuse a playlist ID that does not exist, report it; do not add that check in this task unless it is one line in the existing guard.
- Migration: none expected. If one is needed, stop and report.

## 4. Tests

Unit (`library`, fake stores), table-driven:

- The emergency playlist: refused, the error names the emergency playlist, nothing is deleted.
- Another playlist with the setting set: deleted.
- The setting is empty: deleted.
- A playlist used by a clock: refused as today.
- Both a clock and the emergency setting: refused, and both reasons are named if the existing form allows it.
- Reading the setting fails: the delete fails with a wrapped error and nothing is deleted.

Handler test: the refusal is the same HTTP status and problem shape as the clock refusal.

## 5. Done when

- `make test`, `make test-integration`, `make lint`, `make openapi-lint` pass.
- No SQL outside `internal/repository`, and the architecture test passes.
