# Track plan up to six months: two recalculations, limits, logs

## Goal

The grid and the track plan cover up to six months ahead. Two ways keep the
plan current:

- **Short pass**: every 5 minutes, as today, but it covers the next 31 days
  instead of the next 48 hours.
- **Full pass**: rebuilds the whole range from now to the full horizon, at
  night every day, or on demand with a button. It runs in the background and
  reports progress and the result through WebSocket. Its duration, memory use
  and counts are written to the log so the operator can see its cost and tune
  it.

Both passes keep the rules that already hold: hours that have started keep
their plan, hours already pushed to Liquidsoap (the queue, 2 to 3 items) are not
changed, and hour inserts (`hour_inserts`) survive every rebuild.

## Decisions taken

1. The grid limit is raised to six months. The grid page switches by month,
   not by week, and does not show six months on one page.
2. The full pass runs every night at a configurable station-local time. The
   default is 03:00.
3. A rebuild that would change a queued hour skips that hour and warns.

## Limits and settings (backend)

- `rotation.grid_horizon_days`: range 1 to 183 (was 1 to 30); default stays 30.
- `rotation.plan_horizon_hours` is replaced by `rotation.plan_horizon_days`:
  the short pass covers today's station day up to this many days ahead. Range
  1 to 183, default 31. The old setting is read once for migration of values
  (a stored value of N hours becomes ceil(N / 24) days), then no longer used.
  This is an expand-and-contract migration: add the new key, copy, leave the old
  key unread (see `internal/settings`).
- `rotation.full_pass_time`: `HH:MM`, station local, default `03:00`. The pass
  starts at this time each day; a change applies at the next day.
- `rotation.full_pass_batch_hours`: hours per transaction in the full pass,
  range 24 to 744, default 168. A tuning knob for memory and lock time.
- `rotation.full_pass_enabled`: boolean, default true. Off stops the nightly
  pass; the button still works.
- `MaxPlanDays` (rebuild by range and simulation): 31 per request (was 7 for the
  rebuild). Range checks use the same value.
- Full pass range: from now to `grid_horizon_days` ahead, capped by the plan
  horizon limit of 183 days.

## Short pass (existing job, new horizon)

- `internal/jobs/track_plan.go` keeps its 5-minute interval. The horizon it
  passes to the planner is `plan_horizon_days`, not hours.
- No change to what it builds: missing and stale hours in the horizon.

## Full pass

- New operation kind `track_plan_full` in `operations.Kinds()`. Started by the
  nightly job and by a new endpoint `POST /api/rotation/rebuild-full`
  (returns `accepted` with the operation ID, like the other operations).
- Runs on the service context with its own timeout (sized from the measurement
  below; start at 15 minutes). Never uses the request context.
- Builds hours in time order in batches of `full_pass_batch_hours`, one
  transaction per batch. Each batch loads the stored plan and history it needs
  once for the batch, not per hour. The seed of each hour is its start, as now.
- Skips hours that have started, hours in the Liquidsoap queue, and hours whose
  plan has not changed (the same clock, the same inserts, the same settings and
  the same library version). Skipped hours are counted, not rebuilt.
- Changed hours are replaced in the same transaction as their
  `scheduled_items` rows; hour inserts are kept.
- Progress: an operation row with the count of hours done and total, updated
  per batch. Finish event `operation.completed` or `operation.failed` as today.
  New WebSocket event `rotation.plan_full` with `status`, `hours_built`,
  `hours_skipped`, `items`, `elapsed_ms`, sent at start and at the end, so the
  panel can show that the pass is running and what it did.
- Only one full pass runs at a time (the operation queue already serialises
  planner work; the button is disabled while a pass runs).

## Optimizations

- Library snapshot is limited to what the hours in the range can use: tracks
  of the playlists that the clocks assigned in the range contain (directly, or
  through an insert's playlist). The date range does not limit tracks, so this
  is the filter that matters. Only the columns the build needs are read. The
  snapshot is read once per run (per pass), not per batch and not per hour.
  The 5-minute pass uses the same filter, so it does not read the whole library
  every 5 minutes.
- Measure the snapshot size and its live heap on a fixture of 800 000 tracks
  (with realistic playlist membership, and with clocks using a subset of the
  playlists) before choosing the next optimization. Record the number of tracks
  in the snapshot, its live heap, and the time to read it.
- Inserts for a range: one query for the whole range (`hour_inserts` for the
  batch's dates), not one per hour. This is the main cost seen in the
  measurement (about 2.3 s of 3.3 s for a month).
- Build order and seeds stay as they are, so results match the planner.
- Optimizing `Build` itself (indexing tracks by playlist once per run) is a
  second step, done only if the measurement shows it is needed.

## Simulation (existing endpoint)

- The simulation reads the stored plan hours inside its range as history, as
  the planner would, so a range that already has a plan gives the same items.
  It keeps the 31-day limit and the 30-second timeout only until the measurement
  below says otherwise.

## Logs

Logged through the `rotation` component with `logging.For`:

- Start of every full pass and every short pass that builds at least one hour:
  trigger (`nightly`, `button`, `job`), range, hours to build, hours to skip.
- End of every run: `duration_ms`, `hours_built`, `hours_skipped`, `items`,
  `warnings_by_code`, `queries`, `batches`.
- Memory at start and end of each batch and of the run: `heap_alloc_mb`,
  `heap_sys_mb`, `total_alloc_mb`, `num_gc`, from `runtime.ReadMemStats`. Read
  it at batch boundaries only, not per hour (ReadMemStats stops the world).
- A warning line when a run takes longer than `rotation.full_pass_slow_ms`
  (default 60 000) or when heap in use exceeds `rotation.full_pass_heap_warn_mb`
  (default 512). Both are settings, so the operator can tune them.
- Logs go to the existing app log, so the Loki and Grafana setup of Phase 15
  shows them. Nothing about the content of tracks is logged.

## Frontend (`radio-atlantida-frontend`)

- **Grid page**: the table shows one month at a time. Navigation is by month
  (previous month, this month, next month), replacing the week navigation. A
  month shows its weeks as blocks stacked vertically, each with the same hour
  rows, so the table stays narrow. The date range for assigning a clock is
  limited to one month and to the window from today to six months ahead.
- **Track plan page**: the date pickers accept from today to six months ahead,
  and a range of at most one month. The text "Up to 7 days at once" becomes
  "Up to 31 days at once" (use the same constant from the backend DTO, not a
  second copy of the number).
- **Plan settings**: a control for the nightly time, the enable switch, the
  batch size, and the two log thresholds, using the existing settings page.
- **Full pass button** on the Track plan page: starts `rebuild-full`, disables
  itself while `rotation.plan_full` shows a run in progress, and shows the last
  result (hours built, skipped, items, duration).
- WebSocket: subscribe to `rotation.plan_full` with the existing hub client; a
  toast or inline status on the Track plan page when a run starts and ends.

## Migration

- One migration adds nothing to the schema: the new settings are rows in the
  settings table, read with their code defaults. The old setting key is read
  once for the value conversion, then unused. Apply it to the dev database
  before running tests (`make migrate`), as the backend rules require, if any
  schema change is made.

## Tests

- Backend: the full pass skips started hours, queued hours and unchanged hours
  (table test); a batch keeps hour inserts; the short pass horizon uses days;
  the rebuild range limit is 31; the settings defaults and ranges; a log line
  is written with duration and memory (captured logger).
- Integration: a full pass on a fixture of six months of grid and a library of
  2000 tracks completes, stores the plan, and reports the counts; a second pass
  with no changes builds nothing.
- Frontend: month navigation; date limits (from today, six months ahead, at
  most a month per range); the button disabled during a run; the WebSocket
  status update.

## Done when

- A six-month grid can be assigned; the grid page shows one month at a time.
- The 5-minute pass fills 31 days; the nightly pass rebuilds the range without
  touching started or queued hours; the button starts a background pass with
  live status.
- The logs show duration and memory for each run, and the thresholds can be
  changed in settings.
- The measurement for six months is recorded in the operation's log and in the
  report, and the timeout is set from it.
