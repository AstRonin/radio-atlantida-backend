-- 0013: clocks of playlists and timed inserts, placement rules on playlists,
-- no filler, separation as a play rule (docs/phase-23-plan.md §3.1).
--
-- Not expand-only: it deletes the clocks, the grid, and the plan. That is
-- acceptable only because no release has aired (decision 8); the release
-- notes say so. The backend is stopped while migrations run.

-- Data reset (decision 8) --------------------------------------------------------

-- Their inserts are deleted below, so the missed items are open again.
UPDATE play_log SET resolution = 'open', resolved_item_id = NULL, resolved_at = NULL
WHERE status = 'missed' AND resolution = 'rescheduled';
DELETE FROM hour_inserts;
-- Planned, queued, replaced; aired and missed items live in play_log.
DELETE FROM scheduled_items;
DELETE FROM plan_hours;
DELETE FROM filler_requests;
UPDATE play_log SET clock_id = NULL, grid_slot_id = NULL
WHERE clock_id IS NOT NULL OR grid_slot_id IS NOT NULL;
DELETE FROM grid_slots;
-- Cascades the elements, their playlists, and the filler pools.
DELETE FROM clocks;

-- Separation moves into play rules (decisions 10, 12) -----------------------------

ALTER TABLE play_rules
    ADD COLUMN artist_min_interval_minutes integer,
    ADD CONSTRAINT ck_play_rules_artist_min_interval_minutes_positive CHECK (artist_min_interval_minutes > 0),
    DROP CONSTRAINT ck_play_rules_any_constraint,
    ADD CONSTRAINT ck_play_rules_any_constraint CHECK (
        window_from IS NOT NULL
        OR weekdays IS NOT NULL
        OR min_interval_minutes IS NOT NULL
        OR artist_min_interval_minutes IS NOT NULL
        OR max_per_hour IS NOT NULL
    );

-- The station keeps its rotation: one rule with the old values, attached to
-- every music playlist. An existing rule of that name is kept as it is.
INSERT INTO play_rules (name, min_interval_minutes, artist_min_interval_minutes, notes)
SELECT 'Music rotation',
       (SELECT 60 * max(min_interval_hours) FROM separation_rules WHERE scope = 'track'),
       (SELECT 60 * max(min_interval_hours) FROM separation_rules WHERE scope = 'artist'),
       'Created from the separation rules (phase 23).'
WHERE EXISTS (SELECT 1 FROM separation_rules)
  AND NOT EXISTS (SELECT 1 FROM play_rules WHERE name = 'Music rotation');
INSERT INTO playlist_play_rules (playlist_id, rule_id)
SELECT p.id, r.id
FROM playlists p
JOIN playlist_types t ON t.id = p.type_id AND t.code = 'music'
JOIN play_rules r ON r.name = 'Music rotation'
ON CONFLICT DO NOTHING;
DROP TABLE separation_rules;

-- Filler and elements removed (decision 6) ----------------------------------------

-- The columns go first: their foreign keys point at the tables dropped below.
ALTER TABLE scheduled_items
    DROP CONSTRAINT ck_scheduled_items_filler_pool,
    DROP COLUMN filler_pool_id,
    DROP COLUMN clock_element_id,
    DROP CONSTRAINT ck_scheduled_items_source;
ALTER TABLE plan_hours DROP COLUMN filler_request_id;
DROP TABLE clock_filler_pool, filler_requests, clock_element_playlists, clock_elements;
-- The filler checks and indexes of these columns go with them.
ALTER TABLE clocks
    DROP COLUMN status,
    DROP COLUMN default_filler_mode,
    DROP COLUMN default_filler_playlist_id,
    DROP COLUMN default_filler_artist_id;

-- Placement rules (decision 1) ------------------------------------------------------

ALTER TABLE playlists
    ADD COLUMN clock_rule  text     NOT NULL DEFAULT 'fill',
    -- spread only: how many tracks over the hour.
    ADD COLUMN clock_count smallint,
    ADD CONSTRAINT ck_playlists_clock_rule CHECK (
        clock_rule IN ('fill', 'spread', 'start', 'end', 'start_end', 'middle')
    ),
    ADD CONSTRAINT ck_playlists_clock_count CHECK (
        (clock_rule = 'spread') = (clock_count IS NOT NULL)
        AND (clock_count IS NULL OR clock_count BETWEEN 1 AND 12)
    );

-- The playlists of a clock. Whether every fill row has a weight or none
-- depends on the effective rule, which the service resolves.
CREATE TABLE clock_playlists (
    clock_id    bigint   NOT NULL,
    playlist_id bigint   NOT NULL,
    -- Order of the start and end rule tracks of several playlists.
    position    integer  NOT NULL,
    -- Null: the playlist's own rule.
    clock_rule  text,
    clock_count smallint,
    fill_weight smallint,
    CONSTRAINT pk_clock_playlists PRIMARY KEY (clock_id, playlist_id),
    CONSTRAINT uq_clock_playlists_clock_id_position UNIQUE (clock_id, position),
    CONSTRAINT fk_clock_playlists_clock_id FOREIGN KEY (clock_id)
        REFERENCES clocks (id) ON DELETE CASCADE,
    -- A playlist in a clock cannot be deleted (§4).
    CONSTRAINT fk_clock_playlists_playlist_id FOREIGN KEY (playlist_id)
        REFERENCES playlists (id) ON DELETE RESTRICT,
    CONSTRAINT ck_clock_playlists_position CHECK (position >= 0),
    CONSTRAINT ck_clock_playlists_clock_rule CHECK (
        clock_rule IN ('fill', 'spread', 'start', 'end', 'start_end', 'middle')
    ),
    -- A count only with a spread override; a spread override needs one.
    CONSTRAINT ck_clock_playlists_clock_count CHECK (
        (clock_rule IS NOT DISTINCT FROM 'spread') = (clock_count IS NOT NULL)
        AND (clock_count IS NULL OR clock_count BETWEEN 1 AND 12)
    ),
    CONSTRAINT ck_clock_playlists_fill_weight CHECK (fill_weight BETWEEN 1 AND 100)
);

-- Playlist in-use check.
CREATE INDEX ix_clock_playlists_playlist_id ON clock_playlists (playlist_id);

-- Timed inserts of a clock template: every hour of the clock.
CREATE TABLE clock_inserts (
    id           bigint GENERATED ALWAYS AS IDENTITY,
    clock_id     bigint  NOT NULL,
    offset_ms    integer NOT NULL,
    tolerance_ms integer NOT NULL DEFAULT 60000,
    track_id     bigint,
    playlist_id  bigint,
    CONSTRAINT pk_clock_inserts PRIMARY KEY (id),
    CONSTRAINT uq_clock_inserts_clock_id_offset_ms UNIQUE (clock_id, offset_ms),
    CONSTRAINT fk_clock_inserts_clock_id FOREIGN KEY (clock_id)
        REFERENCES clocks (id) ON DELETE CASCADE,
    CONSTRAINT fk_clock_inserts_track_id FOREIGN KEY (track_id)
        REFERENCES tracks (id) ON DELETE RESTRICT,
    CONSTRAINT fk_clock_inserts_playlist_id FOREIGN KEY (playlist_id)
        REFERENCES playlists (id) ON DELETE RESTRICT,
    CONSTRAINT ck_clock_inserts_offset_ms CHECK (offset_ms BETWEEN 0 AND 3599000 AND offset_ms % 1000 = 0),
    CONSTRAINT ck_clock_inserts_tolerance_ms CHECK (tolerance_ms BETWEEN 0 AND 600000),
    CONSTRAINT ck_clock_inserts_source CHECK ((track_id IS NULL) <> (playlist_id IS NULL))
);

-- Track and playlist in-use checks; most inserts have only one of them.
CREATE INDEX ix_clock_inserts_track_id ON clock_inserts (track_id) WHERE track_id IS NOT NULL;
CREATE INDEX ix_clock_inserts_playlist_id ON clock_inserts (playlist_id) WHERE playlist_id IS NOT NULL;

-- One-time inserts at a time with seconds ----------------------------------------------

ALTER TABLE hour_inserts
    DROP CONSTRAINT ck_hour_inserts_fixed_minute,
    DROP COLUMN fixed_minute,
    -- Whole seconds; set only for 'fixed'.
    ADD COLUMN offset_ms integer,
    ADD CONSTRAINT ck_hour_inserts_offset_ms CHECK (
        (placement = 'fixed') = (offset_ms IS NOT NULL)
        AND (offset_ms IS NULL OR (offset_ms BETWEEN 0 AND 3599000 AND offset_ms % 1000 = 0))
    );
ALTER TABLE hour_inserts RENAME COLUMN fixed_tolerance_ms TO tolerance_ms;
ALTER TABLE hour_inserts RENAME CONSTRAINT ck_hour_inserts_fixed_tolerance_ms TO ck_hour_inserts_tolerance_ms;

-- Plan items name the insert they play --------------------------------------------------

ALTER TABLE scheduled_items
    -- The template insert of an 'insert' item.
    ADD COLUMN clock_insert_id bigint,
    -- The one-time insert of an 'hour_insert' or 'reschedule' item.
    ADD COLUMN hour_insert_id  bigint,
    ADD CONSTRAINT fk_scheduled_items_clock_insert_id FOREIGN KEY (clock_insert_id)
        REFERENCES clock_inserts (id) ON DELETE SET NULL,
    ADD CONSTRAINT fk_scheduled_items_hour_insert_id FOREIGN KEY (hour_insert_id)
        REFERENCES hour_inserts (id) ON DELETE SET NULL,
    ADD CONSTRAINT ck_scheduled_items_source CHECK (
        source IN ('fill', 'rule', 'insert', 'hour_insert', 'reschedule', 'general')
    );

-- FK checks on insert deletes (clock saves, removing a one-time insert). The
-- plan is empty here, so no CONCURRENTLY is needed.
CREATE INDEX ix_scheduled_items_clock_insert_id
    ON scheduled_items (clock_insert_id) WHERE clock_insert_id IS NOT NULL;
CREATE INDEX ix_scheduled_items_hour_insert_id
    ON scheduled_items (hour_insert_id) WHERE hour_insert_id IS NOT NULL;

-- Settings ------------------------------------------------------------------------------

DELETE FROM settings WHERE key IN (
    'filler.fallback', 'filler.reminder_deadline_minutes',
    'rotation.default_duration_tolerance_ms', 'rotation.max_relaxed_tolerance_ms'
);
UPDATE settings SET key = 'rotation.min_gap_ms' WHERE key = 'rotation.min_filler_gap_ms';
