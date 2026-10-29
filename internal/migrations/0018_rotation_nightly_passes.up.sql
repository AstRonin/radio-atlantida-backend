-- 0018: the claim of the nightly full pass of the track plan, one row per
-- station local date. The nightly job inserts the date before its pass; the
-- insert wins once per date, so a restart or a second process does not run
-- the same night again, and a failed pass is not retried that day. A button
-- pass takes no claim. Expand-only: one new table.

CREATE TABLE rotation_nightly_passes (
    station_date date        NOT NULL,
    claimed_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_rotation_nightly_passes PRIMARY KEY (station_date)
);
