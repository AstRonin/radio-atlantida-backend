# Grid: refuse a clock assignment whose weekdays do not occur in its dates

## Problem

The grid assigns a clock to chosen weekdays and hours for a range of dates
(`valid_from`, `valid_to`). A slot is matched to a date by the ISO weekday of
that date (`internal/grid/grid.go`, `ISOWeekday`). When the chosen weekday is
not among the dates of the range, the save succeeds, the slot is stored, but
it never plays and is invisible in the grid.

Example: weekday Monday, `valid_from = valid_to = 2026-10-29`. That date is a
Thursday, so the slot "Mon 01:00" never covers a day of the grid.

## Rule

An assignment is refused when none of its chosen weekdays falls on a date
from `valid_from` to `valid_to` (inclusive, station local). With `valid_to`
open (null), the range is open-ended and the rule is always met.

A partial match is allowed: weekdays Monday and Thursday over a range that
contains only a Thursday create the Thursday slots and skip Monday. The rule
refuses only the case where nothing at all would be created.

## Backend (`radio-atlantida-backend`)

- `internal/grid/grid.go`, `validate`: after the existing checks, when
  `ValidTo` is set and no weekday in `Weekdays` equals `ISOWeekday(d)` for any
  date `d` in `[ValidFrom, ValidTo]`, add
  `errs.Add("weekdays", validation.Invalid, "none of the chosen weekdays falls between valid_from and valid_to")`.
  Use `Date` and `AddDate` from the existing code. Stop at the first match; a
  range of up to the grid horizon is small enough to scan.
- Tests: a unit test in `internal/grid` for: one-day range on a non-matching
  weekday (refused); one-day range on the matching weekday (accepted); a
  range of a week (accepted); open-ended range (accepted); partial match
  (accepted). Keep the existing integration tests passing.
- Only the refusal changes. Existing stored slots are not touched.

## Frontend (`radio-atlantida-frontend`)

- Grid assign form (`src/app/features/grid/grid-page.component.ts`): show the
  same check before the request, on change of weekdays or dates, with the
  message from the backend in the weekdays field, and disable "Assign clock"
  while it fails. The backend stays the authority; the server error is still
  shown if the check on the page is missing.
- Text in `src/assets/i18n/en.json` for the message, and a help line in the
  grid help file (`src/assets/help/en/grid.json`) if that file exists.
- Tests: extend `grid.spec.ts` with the failing and passing cases above.

## Out of scope

- Changing how existing slots with a mismatched weekday are shown or cleaned
  up. The user deletes them manually with Delete.
