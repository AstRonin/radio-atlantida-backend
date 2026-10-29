-- 0016: the inputs of a built hour (docs/rotation-long-horizon-plan.md). The
-- full pass skips an hour whose stored fingerprint equals the one of its
-- current inputs: the clock version, the one-time inserts, the build settings,
-- and the library version. NULL: built before this migration, so the first
-- full pass builds it once. Expand-only: one nullable column.

ALTER TABLE plan_hours
    ADD COLUMN inputs_hash text,
    ADD CONSTRAINT ck_plan_hours_inputs_hash CHECK (inputs_hash IS NULL OR inputs_hash <> '');
