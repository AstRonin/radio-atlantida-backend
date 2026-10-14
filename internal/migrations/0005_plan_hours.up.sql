-- 0005: the built hours of the track plan, and the links of planned items
-- (docs/phase-8-plan.md §2, items 8 and 5).
--
-- plan_hours records which hour was built from which clock version, with the
-- seed of the build, so the plan job can tell missing and stale hours apart
-- and a rebuild after a reminder answer is reproducible. Expand-only: new
-- table, new nullable columns, new indexes. The tables are empty in every
-- release so far, so the indexes need no CONCURRENTLY.

CREATE TABLE plan_hours (
    hour_start_utc    timestamptz NOT NULL,
    clock_id          bigint      NOT NULL,
    -- clocks.updated_at when the hour was built; a later change makes the
    -- hour stale.
    clock_updated_at  timestamptz NOT NULL,
    grid_slot_id      bigint,
    seed              bigint      NOT NULL,
    built_at          timestamptz NOT NULL,
    -- The reminder of this build, for the hour's waiting segments.
    filler_request_id bigint,
    CONSTRAINT pk_plan_hours PRIMARY KEY (hour_start_utc),
    -- A clock with planned items cannot be deleted (fk_scheduled_items_clock_id),
    -- so this only removes hours that planned nothing.
    CONSTRAINT fk_plan_hours_clock_id FOREIGN KEY (clock_id)
        REFERENCES clocks (id) ON DELETE CASCADE,
    CONSTRAINT fk_plan_hours_grid_slot_id FOREIGN KEY (grid_slot_id)
        REFERENCES grid_slots (id) ON DELETE SET NULL,
    CONSTRAINT fk_plan_hours_filler_request_id FOREIGN KEY (filler_request_id)
        REFERENCES filler_requests (id) ON DELETE SET NULL
);

-- FK checks on clock and slot deletes.
CREATE INDEX ix_plan_hours_clock_id ON plan_hours (clock_id);
CREATE INDEX ix_plan_hours_grid_slot_id ON plan_hours (grid_slot_id) WHERE grid_slot_id IS NOT NULL;

ALTER TABLE scheduled_items
    -- The element the item was planned for; null for filler.
    ADD COLUMN clock_element_id bigint,
    -- The pool row a filler_pool item took; a rebuild releases it.
    ADD COLUMN filler_pool_id bigint,
    ADD CONSTRAINT fk_scheduled_items_clock_element_id FOREIGN KEY (clock_element_id)
        REFERENCES clock_elements (id) ON DELETE SET NULL,
    ADD CONSTRAINT fk_scheduled_items_filler_pool_id FOREIGN KEY (filler_pool_id)
        REFERENCES clock_filler_pool (id) ON DELETE SET NULL,
    ADD CONSTRAINT ck_scheduled_items_filler_pool CHECK (filler_pool_id IS NULL OR source = 'filler_pool');

-- FK checks on element and pool row deletes (clock saves, pool edits).
CREATE INDEX ix_scheduled_items_clock_element_id
    ON scheduled_items (clock_element_id) WHERE clock_element_id IS NOT NULL;
CREATE INDEX ix_scheduled_items_filler_pool_id
    ON scheduled_items (filler_pool_id) WHERE filler_pool_id IS NOT NULL;

-- The history window of a build: aired plays around the hour (§7).
CREATE INDEX ix_play_log_started_at ON play_log (started_at) WHERE status IN ('aired', 'truncated');

-- Reminders whose deadline passed (the plan job).
CREATE INDEX ix_filler_requests_deadline_at_pending ON filler_requests (deadline_at) WHERE status = 'pending';
