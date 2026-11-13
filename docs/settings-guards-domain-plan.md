# Plan — Settings Guards into the Domain (radio-atlantida-backend)

Status: Approved (2026-11-14).
Based on: `docs/backend-split-plan.md` §5 item 1, `docs/tech-design.md` §4.1, §6.3, Plan agent analysis (2026-11-13)
Last updated: 2026-11-14

## 1. Goal

Two business rules live in `cmd/backend/settings_guards.go`, where domain tests do not reach them:

- `guardRetention`: the play log retention (`settings.RetentionPlayLogDays`) must not be shorter than the longest play rule interval (§4.1). Registered with `settings.Guard`.
- `checkEncoderLimits` / `encoderLimitCheck` / `streamFormatKeys`: a `stream.*` combination must stay within the encoder limits of the pinned Liquidsoap (§6.3, `streamformat.Check`). Registered with `settings.Check` on the merged `View`.

Move them into domain packages with unit tests. `cmd` keeps one registration line per rule. Behaviour, rule codes and messages do not change.

## 2. Decisions (developer, 2026-11-14)

1. **Retention guard → `internal/playrules`.** playrules already owns the other half of the invariant (intervals ≤ retention, `MaxIntervalFunc`). `playrules` imports `settings`; no cycle (`settings` imports only `logging`). `settings` must never import `playrules`.
2. **Encoder check → `internal/settings`.** `settings` already defines the `stream.*` keys and `RuleEncoderLimit`; it imports the leaf `internal/streamformat`. `streamformat` stays dependency-free (used by `dev/liqsmoke`).
3. The `internal/httpapi/settings_test.go` hand-made copy of the encoder check (its message differs from production) is replaced by the real registration; its assertion changes to the production text.

## 3. Work

1. `internal/playrules/retention_guard.go`:
   ```go
   // GuardRetention registers the check that refuses a play log retention
   // shorter than the longest play rule interval (design §4.1).
   func (s *Service) GuardRetention(set *settings.Service)
   ```
   Body = the current `guardRetention` closure, using `s.LongestIntervalMinutes`. Same `settings.Refuse(..., settings.RuleIntervalBeyondRetention, ...)` text and rounding.
2. `internal/settings/stream.go`:
   ```go
   // CheckEncoderLimits registers the cross-key check of the stream format
   // against the encoder limits of the pinned Liquidsoap (design §6.3).
   func CheckEncoderLimits(svc *Service)
   ```
   plus unexported `encoderLimitCheck` and `streamFormatKeys`, bodies unchanged (drop the `settings.` qualifiers).
3. `cmd/backend/serve.go`: replace the two calls with `playRules.GuardRetention(settingsSvc)` and `settings.CheckEncoderLimits(settingsSvc)` at the same place (after `playrules.NewService`, before the HTTP server). `serve_test.go` helper calls `settings.CheckEncoderLimits`. Delete `cmd/backend/settings_guards.go`.
4. Tests:
   - Move `TestEncoderLimitCheck` (with `encoderService`, `refusals`, the key vars) from `cmd/backend/serve_test.go` to `internal/settings/stream_test.go`, using the in-package fake store (`settings_test.go` `fakeStore`), or a copy of `memStore`. `TestWakeOnStreamChange`, `TestStreamOutputDefaults` and `memStore` stay in cmd if still used there.
   - New `internal/playrules/retention_guard_test.go` (table-driven): longest == days×1440 passes; one minute over refused with `RuleIntervalBeyondRetention` and the rounded-up day count in the message; no rules (0) passes; a store error fails the save (error, not a refusal). Fake: a struct embedding `playrules.Store` that implements only `LongestInterval`; `NewService(nil, fake, nil)`.
   - `internal/httpapi/settings_test.go`: use `settings.CheckEncoderLimits` instead of the copy; update the expected message to the production text.
5. Docs in the same change: doc comment of `internal/streamformat/streamformat.go` (no longer "cmd/backend"), CLAUDE.md layout lines for `settings/`, `streamformat/`, `playrules/`.

## 4. Commits

1. Retention guard into playrules (+ test).
2. Encoder check into settings (+ moved tests, httpapi test fix).
3. CLAUDE.md and doc comments (may be folded into 1 and 2).
4. Plan status.

## 5. Done when

- `make fmt`, `go build ./...`, `go vet ./...`, `make test`, `make test-integration`, `make lint` pass.
- `cmd/backend/settings_guards.go` is gone; `cmd` holds no business rule of these two.
- No import of `playrules` from `settings`; `streamformat` imports nothing from `internal/`.
- Refusal codes and messages unchanged (except the httpapi test copy, decision 3).

## 6. Deviations

(Filled in during implementation.)
