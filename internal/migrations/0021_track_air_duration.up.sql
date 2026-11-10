-- 0021: the on-air length of a track, stored by PostgreSQL
-- (docs/track-stats-plan.md decisions 1 and 2), for the sum of the library
-- statistics and the duration sort. A generated column: every write to
-- duration_ms and the cue columns is covered, and the value cannot drift from
-- its sources. NULL while duration_ms is NULL, even with cue_out_ms set, as
-- Track.AirDurationMS() is. Never written by an INSERT or an UPDATE: PostgreSQL
-- refuses an explicit value. No index: the statistics scan the filtered tracks,
-- as the count of the track list does. The ADD COLUMN rewrites the table once
-- under an exclusive lock; migrations run before the backend starts, and the
-- table is small enough for this.

ALTER TABLE tracks
    ADD COLUMN air_duration_ms integer
    GENERATED ALWAYS AS (
        CASE WHEN duration_ms IS NOT NULL
            THEN COALESCE(cue_out_ms, duration_ms) - cue_in_ms
        END
    ) STORED;
