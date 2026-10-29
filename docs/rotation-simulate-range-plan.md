# Track plan simulation for a date range (backend, measurement first)

## Goal

Let the operator see how a range of future days would be laid out, without
storing anything, for example 1 to 30 November. The first step is backend
only and measures how long a month takes to build, so the next decision
(whether to raise the 24 to 48 hour horizon, and how far) is based on numbers.

## What it is not

- It does not change the grid, the clocks, the plan hours, or scheduled
  items. Nothing is written. The result is returned and discarded.
- It is not the operator's plan: the real plan is built by the planner as
  time comes, with the real history and library at that time.
- It has no UI in this step.

## Behaviour

- Input: `from` and `to` (station local dates, inclusive), at most 31 days,
  `to` not before `from`. Dates may be any day the grid covers; the range is
  not limited to the plan horizon.
- For every grid hour in the range (`grid.Hours`, which already resolves the
  clock of each hour) build the hour with `rotation.Build`, as the planner
  does: the clock, the hour's one-time inserts (`Inserts`), the library, the
  history, and the settings. Use the seed the planner uses for the hour: the
  hour start in Unix seconds.
- History must include what the simulation has already built. Hours are
  built in time order; each built hour's items are added to the history of the
  later hours, so the track and artist intervals of the play rules are checked
  against the simulated hours too. The history before `from` comes from the
  play log and the stored plan hours (the same window the planner uses,
  `span = lib.maxRuleInterval()`). Load these once for the whole range, not
  once per hour.
- Output (JSON, not stored): for each hour its start, clock id, item count and
  warnings; and for the whole run the number of hours built, the number of
  items, the number of warnings by code, the elapsed time in milliseconds, and
  the number of database queries made. Do not return the items of every hour
  by default; add `items=true` to include them.
- Errors: the same refusals as the planner (for example an invalid range).
  A hour with no clock is skipped and counted, not an error.

## Where the code goes

- A new method in `internal/rotation` (next to `Sample` in `sample.go`, or in a
  new file `simulate.go`) that takes the grid hours and the stored history and
  builds them in order. It is pure except for reading the stores.
- An endpoint in `internal/httpapi/handlers/rotation.go` (huma operation,
  DTOs in `internal/httpapi/dto/rotation.go`), `GET /api/rotation/simulate`
  with `from`, `to`, `items`. Errors use the existing validation codes.
- No migration, no new settings, no change to the planner or the jobs.
- No timeout change for the planner. The simulation has its own timeout
  constant with a comment on why it is longer than `hourTimeout`, sized so
  that a 31-day run can finish; measure first and set it from the result.

## Measurement

- Run it against a dev database with a realistic library (the dev stack and
  `make media`, plus enough tracks for each playlist, or a fixture of similar
  size) and a grid that assigns a clock to every hour of the month.
- Report: elapsed time for 1 day, 7 days and 31 days; queries per run; the
  library size and history size used; memory use if it is easy to read.
- If the dev data is too small to be representative, say so in the report and
  give the numbers as a lower bound.

## Tests

- Unit tests in `internal/rotation` for the ordering rule: a track built in an
  earlier simulated hour is not allowed again inside its minimum interval in a
  later simulated hour.
- An integration test for the endpoint with a small grid (two days, one clock):
  the item counts match a direct `Build` call for the same hour.
- `make test` and `make lint` pass. Apply no migrations (none are needed).

## Done when

- `GET /api/rotation/simulate?from=2026-11-01&to=2026-11-30` returns the
  summary and the timing, and stores nothing (check the plan hours and
  scheduled items before and after the call).
- The report gives the measured elapsed time and query count for 1, 7 and 31
  days, with the data used.
