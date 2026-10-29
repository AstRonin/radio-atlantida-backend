-- 0014: the progress of a running operation, stored per batch by the
-- operations that report it (track_plan_full, docs/rotation-long-horizon-plan.md).
-- Expand-only: nullable columns; existing rows have no progress.

ALTER TABLE operations
    ADD COLUMN progress_done  integer,
    ADD COLUMN progress_total integer,
    ADD CONSTRAINT ck_operations_progress CHECK (
        (progress_done IS NULL) = (progress_total IS NULL)
        AND progress_done >= 0
        AND progress_done <= progress_total
    );
