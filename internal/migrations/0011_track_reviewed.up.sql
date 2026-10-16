-- 0011: the reviewed mark of a track (docs/phase-21-plan.md §3.2).
--
-- Expand-only: one nullable column, so the previous code works against the
-- new schema. NULL is "not reviewed"; the time is when the manager marked
-- the track checked. No index: the reviewed filter and sort key run on the
-- sequential scan of the track list like the other filters (phase 20).

ALTER TABLE tracks ADD COLUMN reviewed_at timestamptz;
