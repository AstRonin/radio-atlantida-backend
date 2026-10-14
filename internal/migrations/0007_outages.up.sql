-- 0007: outages and missed items (docs/phase-10-plan.md §2).
--
-- Expand-only: new tables and new indexes. play_log holds only a few days
-- of dev data in every release so far, so the indexes need no CONCURRENTLY.

-- When the backend was last alive (one row), written by the outage detector
-- every 30 s. After a restart it tells how long the backend was down
-- (decision 7).
CREATE TABLE backend_heartbeat (
    id       boolean     NOT NULL DEFAULT true,
    alive_at timestamptz NOT NULL,
    CONSTRAINT pk_backend_heartbeat PRIMARY KEY (id),
    CONSTRAINT ck_backend_heartbeat_one_row CHECK (id)
);

-- One-time elements of a single hour: a missed item rescheduled into it
-- (§7.1, §7.2, decision 10). Every build of the hour places them; the clock
-- template is not changed.
CREATE TABLE hour_inserts (
    id                 bigint GENERATED ALWAYS AS IDENTITY,
    hour_start_utc     timestamptz NOT NULL,
    track_id           bigint      NOT NULL,
    -- The missed play_log row this insert re-airs. Retention may delete it
    -- after it is resolved; the insert then only keeps its track.
    missed_id          bigint,
    placement          text        NOT NULL,
    fixed_minute       integer,
    fixed_tolerance_ms integer     NOT NULL DEFAULT 60000,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_hour_inserts PRIMARY KEY (id),
    CONSTRAINT fk_hour_inserts_track_id FOREIGN KEY (track_id)
        REFERENCES tracks (id) ON DELETE RESTRICT,
    CONSTRAINT fk_hour_inserts_missed_id FOREIGN KEY (missed_id)
        REFERENCES play_log (id) ON DELETE SET NULL,
    CONSTRAINT ck_hour_inserts_placement CHECK (placement IN ('start', 'end', 'fixed')),
    CONSTRAINT ck_hour_inserts_fixed_minute CHECK (
        (placement = 'fixed') = (fixed_minute IS NOT NULL)
        AND (fixed_minute IS NULL OR fixed_minute BETWEEN 0 AND 59)
    ),
    CONSTRAINT ck_hour_inserts_fixed_tolerance_ms CHECK (fixed_tolerance_ms >= 0)
);

-- The build of an hour reads its inserts.
CREATE INDEX ix_hour_inserts_hour_start_utc ON hour_inserts (hour_start_utc);
-- The track in-use check and the FK check on track deletes.
CREATE INDEX ix_hour_inserts_track_id ON hour_inserts (track_id);
-- Retention deletes play_log rows; each delete checks this FK.
CREATE INDEX ix_hour_inserts_missed_id ON hour_inserts (missed_id) WHERE missed_id IS NOT NULL;

-- Journal replay passes an emergency track's end again; its start time is
-- computed from the same journal line, so a repeat is a no-op (decision 6).
CREATE UNIQUE INDEX uq_play_log_emergency
    ON play_log (track_id, started_at) WHERE source = 'emergency';

-- The played-tracks list: newest first by the time the row aired or was
-- planned, keyset paginated (decision 8).
CREATE INDEX ix_play_log_sort_time
    ON play_log ((COALESCE(started_at, planned_start_utc)) DESC, id DESC);
