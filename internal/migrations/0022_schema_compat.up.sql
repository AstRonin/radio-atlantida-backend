-- 0022: downgrade guard (docs/deploy-prereqs-plan.md B1, design §9.2).
--
-- Expand-only: one new table with one row. min_app_version is the oldest
-- release that works with the schema. `backend serve` refuses to start below
-- it, `backend migrate check` reports it, and the deploy tool refuses to
-- install an older release below it. A contract migration (or any migration
-- an older release cannot work with) raises it as its last statement:
--   UPDATE schema_compat SET min_app_version = '<release>', set_by_migration = <n>, updated_at = now() WHERE id = 1;
-- and migrations.TargetMinAppVersion with it. A database without this table
-- counts as 1.0.0.
CREATE TABLE schema_compat (
    id               smallint    NOT NULL DEFAULT 1,
    min_app_version  text        NOT NULL,
    -- The migration that last set min_app_version.
    set_by_migration integer     NOT NULL,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_schema_compat PRIMARY KEY (id),
    CONSTRAINT ck_schema_compat_one_row CHECK (id = 1),
    CONSTRAINT ck_schema_compat_min_app_version CHECK (min_app_version ~ '^[0-9]+\.[0-9]+\.[0-9]+$')
);

INSERT INTO schema_compat (min_app_version, set_by_migration) VALUES ('1.0.0', 22);
