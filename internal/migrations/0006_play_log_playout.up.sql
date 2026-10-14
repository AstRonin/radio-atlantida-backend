-- 0006: play_log rows written by playout (docs/phase-9-plan.md §2, item 6).
--
-- A plan item moves to play_log when Liquidsoap reports its start; its end
-- arrives later, sometimes twice (at the hour switch and again when the
-- queue is selected an hour later), and journal reconciliation (Phase 10)
-- replays the same events. ended_at marks the end as recorded, so the first
-- end wins; the unique index makes a repeated start a no-op.
--
-- Expand-only: a new nullable column and a new index. play_log is empty in
-- every release so far (nothing aired before this phase), so the index needs
-- no CONCURRENTLY.

ALTER TABLE play_log
    -- Null until the end of the track is recorded (and for missed items).
    ADD COLUMN ended_at timestamptz,
    ADD CONSTRAINT ck_play_log_ended_after_start CHECK (ended_at >= started_at);

CREATE UNIQUE INDEX uq_play_log_scheduled_item_id
    ON play_log (scheduled_item_id) WHERE scheduled_item_id IS NOT NULL;
