-- 0001_init: the full schema of §4 of docs/tech-design.md.
--
-- Greenfield (init plan §9.2): no categories or daypart_rules tables.
-- Conventions (init plan §1.2, §9.3): bigint identity keys, text + CHECK
-- instead of enum types, timestamptz for instants, named constraints.
-- Weekdays are ISO numbers in station local time: 1 = Monday ... 7 = Sunday.
--
-- ON DELETE rules: RESTRICT where the API must refuse the delete and name
-- what blocks it; CASCADE for pure link tables and rows owned by a parent;
-- SET NULL only where losing the link is harmless.

-- Library --------------------------------------------------------------------

CREATE TABLE playlist_types (
    id         bigint GENERATED ALWAYS AS IDENTITY,
    code       text        NOT NULL,
    name       text        NOT NULL,
    is_system  boolean     NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_playlist_types PRIMARY KEY (id),
    CONSTRAINT uq_playlist_types_code UNIQUE (code),
    CONSTRAINT uq_playlist_types_name UNIQUE (name),
    CONSTRAINT ck_playlist_types_code_slug CHECK (code ~ '^[a-z][a-z0-9_-]{0,62}$'),
    CONSTRAINT ck_playlist_types_name_not_blank CHECK (btrim(name) <> '')
);

-- System types: renamable, never deleted, code never changes (§4).
INSERT INTO playlist_types (code, name, is_system) VALUES
    ('music',   'Music',   true),
    ('jingle',  'Jingle',  true),
    ('news',    'News',    true),
    ('segment', 'Segment', true);

CREATE TABLE artists (
    id   bigint GENERATED ALWAYS AS IDENTITY,
    name text NOT NULL,
    CONSTRAINT pk_artists PRIMARY KEY (id),
    -- Artist separation counts plays per row, so a duplicate name would
    -- split one artist into two.
    CONSTRAINT uq_artists_name UNIQUE (name),
    CONSTRAINT ck_artists_name_not_blank CHECK (btrim(name) <> '')
);

CREATE TABLE playlists (
    id         bigint GENERATED ALWAYS AS IDENTITY,
    name       text        NOT NULL,
    type_id    bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_playlists PRIMARY KEY (id),
    CONSTRAINT uq_playlists_name UNIQUE (name),
    CONSTRAINT fk_playlists_type_id FOREIGN KEY (type_id)
        REFERENCES playlist_types (id) ON DELETE RESTRICT,
    CONSTRAINT ck_playlists_name_not_blank CHECK (btrim(name) <> '')
);

-- Type in-use check.
CREATE INDEX ix_playlists_type_id ON playlists (type_id);

CREATE TABLE tracks (
    id            bigint GENERATED ALWAYS AS IDENTITY,
    title         text        NOT NULL,
    -- Null for material without an artist (jingles, station IDs).
    artist_id     bigint,
    -- Null until the file is analysed; such a track cannot be planned.
    duration_ms   integer,
    -- Relative to MEDIA_ROOT, with forward slashes (§8.7).
    file_path     text        NOT NULL,
    format        text,
    -- Bits per second, as ffprobe reports it.
    bitrate       integer,
    sample_rate   integer,
    -- Null means "not analysed yet", which is not the same as "bad".
    quality_ok    boolean,
    quality_notes text        NOT NULL DEFAULT '',
    type_id       bigint      NOT NULL,
    file_checksum text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_tracks PRIMARY KEY (id),
    CONSTRAINT uq_tracks_file_path UNIQUE (file_path),
    CONSTRAINT fk_tracks_artist_id FOREIGN KEY (artist_id)
        REFERENCES artists (id) ON DELETE RESTRICT,
    CONSTRAINT fk_tracks_type_id FOREIGN KEY (type_id)
        REFERENCES playlist_types (id) ON DELETE RESTRICT,
    CONSTRAINT ck_tracks_title_not_blank CHECK (btrim(title) <> ''),
    CONSTRAINT ck_tracks_duration_ms_positive CHECK (duration_ms > 0),
    CONSTRAINT ck_tracks_bitrate_positive CHECK (bitrate > 0),
    CONSTRAINT ck_tracks_sample_rate_positive CHECK (sample_rate > 0),
    -- No absolute paths (Unix or Windows), no backslashes, no "..".
    CONSTRAINT ck_tracks_file_path_relative CHECK (
        file_path <> ''
        AND left(file_path, 1) <> '/'
        AND position('\' IN file_path) = 0
        AND file_path !~ '^[A-Za-z]:'
        AND file_path !~ '(^|/)\.\.(/|$)'
    )
);

-- Type and artist in-use checks.
CREATE INDEX ix_tracks_type_id ON tracks (type_id);
CREATE INDEX ix_tracks_artist_id ON tracks (artist_id);

-- A playlist is a pool: no position column (§4).
CREATE TABLE track_playlists (
    track_id    bigint NOT NULL,
    playlist_id bigint NOT NULL,
    -- The primary key also serves lookups by track_id.
    CONSTRAINT pk_track_playlists PRIMARY KEY (track_id, playlist_id),
    CONSTRAINT fk_track_playlists_track_id FOREIGN KEY (track_id)
        REFERENCES tracks (id) ON DELETE CASCADE,
    CONSTRAINT fk_track_playlists_playlist_id FOREIGN KEY (playlist_id)
        REFERENCES playlists (id) ON DELETE CASCADE
);

-- Candidate selection (§7).
CREATE INDEX ix_track_playlists_playlist_id ON track_playlists (playlist_id);

-- Play rules (§7.3) and separation ----------------------------------------------

CREATE TABLE play_rules (
    id                   bigint GENERATED ALWAYS AS IDENTITY,
    name                 text        NOT NULL,
    -- Station local time; a window may cross midnight (22:00 to 02:00).
    window_from          time,
    window_to            time,
    -- Null means every day. With a window, the day on which it starts.
    weekdays             smallint[],
    min_interval_minutes integer,
    max_per_hour         integer,
    notes                text        NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_play_rules PRIMARY KEY (id),
    CONSTRAINT uq_play_rules_name UNIQUE (name),
    CONSTRAINT ck_play_rules_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT ck_play_rules_any_constraint CHECK (
        window_from IS NOT NULL
        OR weekdays IS NOT NULL
        OR min_interval_minutes IS NOT NULL
        OR max_per_hour IS NOT NULL
    ),
    CONSTRAINT ck_play_rules_window_both CHECK ((window_from IS NULL) = (window_to IS NULL)),
    CONSTRAINT ck_play_rules_window_not_empty CHECK (window_from <> window_to),
    CONSTRAINT ck_play_rules_weekdays CHECK (
        array_ndims(weekdays) = 1
        AND cardinality(weekdays) BETWEEN 1 AND 7
        AND weekdays <@ '{1,2,3,4,5,6,7}'::smallint[]
    ),
    CONSTRAINT ck_play_rules_min_interval_minutes_positive CHECK (min_interval_minutes > 0),
    CONSTRAINT ck_play_rules_max_per_hour_positive CHECK (max_per_hour > 0)
);

CREATE TABLE track_play_rules (
    track_id bigint NOT NULL,
    rule_id  bigint NOT NULL,
    -- The primary key also serves lookups by track_id.
    CONSTRAINT pk_track_play_rules PRIMARY KEY (track_id, rule_id),
    CONSTRAINT fk_track_play_rules_track_id FOREIGN KEY (track_id)
        REFERENCES tracks (id) ON DELETE CASCADE,
    -- A rule in use cannot be deleted (§4).
    CONSTRAINT fk_track_play_rules_rule_id FOREIGN KEY (rule_id)
        REFERENCES play_rules (id) ON DELETE RESTRICT
);

-- Rule in-use check.
CREATE INDEX ix_track_play_rules_rule_id ON track_play_rules (rule_id);

CREATE TABLE playlist_play_rules (
    playlist_id bigint NOT NULL,
    rule_id     bigint NOT NULL,
    -- The primary key also serves lookups by playlist_id.
    CONSTRAINT pk_playlist_play_rules PRIMARY KEY (playlist_id, rule_id),
    CONSTRAINT fk_playlist_play_rules_playlist_id FOREIGN KEY (playlist_id)
        REFERENCES playlists (id) ON DELETE CASCADE,
    CONSTRAINT fk_playlist_play_rules_rule_id FOREIGN KEY (rule_id)
        REFERENCES play_rules (id) ON DELETE RESTRICT
);

-- Rule in-use check.
CREATE INDEX ix_playlist_play_rules_rule_id ON playlist_play_rules (rule_id);

CREATE TABLE separation_rules (
    id                 bigint GENERATED ALWAYS AS IDENTITY,
    scope              text    NOT NULL,
    min_interval_hours integer NOT NULL DEFAULT 72,
    notes              text    NOT NULL DEFAULT '',
    CONSTRAINT pk_separation_rules PRIMARY KEY (id),
    CONSTRAINT ck_separation_rules_scope CHECK (scope IN ('track', 'artist')),
    CONSTRAINT ck_separation_rules_min_interval_hours_positive CHECK (min_interval_hours > 0)
);

-- Placeholders until the values from the reference software are entered
-- (§4, init plan Phase 2).
INSERT INTO separation_rules (scope, min_interval_hours, notes) VALUES
    ('track',  72, 'Default: a track at most once per 72 hours. Replace with the reference software value.'),
    ('artist', 72, 'Default. Replace with the reference software value.');

-- Clocks (§4, §7.2) -------------------------------------------------------------

CREATE TABLE clocks (
    id                       bigint GENERATED ALWAYS AS IDENTITY,
    name                     text        NOT NULL,
    length_seconds           integer     NOT NULL DEFAULT 3600,
    status                   text        NOT NULL DEFAULT 'incomplete',
    -- Mode used when the pool is empty or a reminder expires; null falls
    -- back to the global setting (init plan §9.4, item 3).
    default_filler_mode      text,
    -- A playlist id for 'playlist', an artist id for 'artist'.
    default_filler_source_id bigint,
    updated_at               timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_clocks PRIMARY KEY (id),
    CONSTRAINT uq_clocks_name UNIQUE (name),
    CONSTRAINT ck_clocks_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT ck_clocks_length_seconds CHECK (length_seconds = 3600),
    CONSTRAINT ck_clocks_status CHECK (status IN ('complete', 'incomplete')),
    -- Only modes that resolve without the operator: 'ask' and 'pool' would
    -- loop back to the default.
    CONSTRAINT ck_clocks_default_filler_mode CHECK (
        default_filler_mode IN ('random', 'playlist', 'artist')
    ),
    CONSTRAINT ck_clocks_default_filler_source CHECK (
        COALESCE(default_filler_mode IN ('playlist', 'artist'), false)
        = (default_filler_source_id IS NOT NULL)
    )
);

CREATE TABLE clock_elements (
    id                    bigint GENERATED ALWAYS AS IDENTITY,
    clock_id              bigint      NOT NULL,
    position              integer     NOT NULL,
    placement             text        NOT NULL DEFAULT 'flow',
    fixed_minute          smallint,
    -- Applies to the start of a fixed element (±1 minute by default).
    fixed_tolerance_ms    integer     NOT NULL DEFAULT 60000,
    nominal_duration_ms   integer     NOT NULL,
    duration_tolerance_ms integer     NOT NULL DEFAULT 30000,
    filler_mode           text        NOT NULL DEFAULT 'none',
    -- A playlist id for 'playlist', an artist id for 'artist'. No FK,
    -- because the target table depends on the mode.
    filler_source_id      bigint,
    -- Optional per-element settings (§4).
    settings              jsonb,
    created_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_clock_elements PRIMARY KEY (id),
    -- Also the index clock_elements (clock_id, position) of §4.
    CONSTRAINT uq_clock_elements_clock_id_position UNIQUE (clock_id, position),
    CONSTRAINT fk_clock_elements_clock_id FOREIGN KEY (clock_id)
        REFERENCES clocks (id) ON DELETE CASCADE,
    CONSTRAINT ck_clock_elements_position CHECK (position >= 0),
    CONSTRAINT ck_clock_elements_placement CHECK (placement IN ('flow', 'start', 'end', 'fixed')),
    CONSTRAINT ck_clock_elements_fixed_minute_range CHECK (fixed_minute BETWEEN 0 AND 59),
    CONSTRAINT ck_clock_elements_fixed_minute CHECK ((placement = 'fixed') = (fixed_minute IS NOT NULL)),
    CONSTRAINT ck_clock_elements_fixed_tolerance_ms CHECK (fixed_tolerance_ms >= 0),
    CONSTRAINT ck_clock_elements_nominal_duration_ms CHECK (nominal_duration_ms BETWEEN 1 AND 3600000),
    CONSTRAINT ck_clock_elements_duration_tolerance_ms CHECK (duration_tolerance_ms >= 0),
    CONSTRAINT ck_clock_elements_filler_mode CHECK (
        filler_mode IN ('none', 'ask', 'random', 'playlist', 'artist', 'pool')
    ),
    CONSTRAINT ck_clock_elements_filler_source CHECK (
        (filler_mode IN ('playlist', 'artist')) = (filler_source_id IS NOT NULL)
    ),
    CONSTRAINT ck_clock_elements_settings_object CHECK (jsonb_typeof(settings) = 'object')
);

-- An element has at least one playlist; checked by the API (§6.2).
CREATE TABLE clock_element_playlists (
    clock_element_id bigint NOT NULL,
    playlist_id      bigint NOT NULL,
    CONSTRAINT pk_clock_element_playlists PRIMARY KEY (clock_element_id, playlist_id),
    CONSTRAINT fk_clock_element_playlists_clock_element_id FOREIGN KEY (clock_element_id)
        REFERENCES clock_elements (id) ON DELETE CASCADE,
    -- A playlist used by a clock element cannot be deleted (§4).
    CONSTRAINT fk_clock_element_playlists_playlist_id FOREIGN KEY (playlist_id)
        REFERENCES playlists (id) ON DELETE RESTRICT
);

-- Playlist in-use check.
CREATE INDEX ix_clock_element_playlists_playlist_id ON clock_element_playlists (playlist_id);

CREATE TABLE clock_filler_pool (
    id       bigint GENERATED ALWAYS AS IDENTITY,
    clock_id bigint  NOT NULL,
    track_id bigint  NOT NULL,
    position integer NOT NULL,
    -- Set when the track is taken for filler; rows are kept as history.
    used_at  timestamptz,
    CONSTRAINT pk_clock_filler_pool PRIMARY KEY (id),
    -- Also the index clock_filler_pool (clock_id, position) of §4.
    CONSTRAINT uq_clock_filler_pool_clock_id_position UNIQUE (clock_id, position),
    CONSTRAINT fk_clock_filler_pool_clock_id FOREIGN KEY (clock_id)
        REFERENCES clocks (id) ON DELETE CASCADE,
    CONSTRAINT fk_clock_filler_pool_track_id FOREIGN KEY (track_id)
        REFERENCES tracks (id) ON DELETE RESTRICT,
    CONSTRAINT ck_clock_filler_pool_position CHECK (position >= 0)
);

-- Next unused pool track: WHERE clock_id = $1 AND used_at IS NULL ORDER BY
-- position LIMIT 1. §4 lists (clock_id); position is added so the order
-- comes from the index.
CREATE INDEX ix_clock_filler_pool_clock_id_position_unused
    ON clock_filler_pool (clock_id, position) WHERE used_at IS NULL;
-- Track in-use check.
CREATE INDEX ix_clock_filler_pool_track_id ON clock_filler_pool (track_id);

-- Grid (§4, §10.2) ---------------------------------------------------------------

CREATE TABLE grid_slots (
    id         bigint GENERATED ALWAYS AS IDENTITY,
    -- Station local time.
    weekday    smallint NOT NULL,
    hour       smallint NOT NULL,
    clock_id   bigint   NOT NULL,
    -- Station local dates; valid_to null means open-ended.
    valid_from date     NOT NULL,
    valid_to   date,
    CONSTRAINT pk_grid_slots PRIMARY KEY (id),
    -- A clock in the grid cannot be deleted.
    CONSTRAINT fk_grid_slots_clock_id FOREIGN KEY (clock_id)
        REFERENCES clocks (id) ON DELETE RESTRICT,
    CONSTRAINT ck_grid_slots_weekday CHECK (weekday BETWEEN 1 AND 7),
    CONSTRAINT ck_grid_slots_hour CHECK (hour BETWEEN 0 AND 23),
    CONSTRAINT ck_grid_slots_valid_range CHECK (valid_to >= valid_from)
);

CREATE INDEX ix_grid_slots_weekday_hour ON grid_slots (weekday, hour);
-- Clock in-use check.
CREATE INDEX ix_grid_slots_clock_id ON grid_slots (clock_id);

-- Outages, play log, track plan, filler requests (§4, §7, §7.1) ------------------

CREATE TABLE broadcast_outages (
    id            bigint GENERATED ALWAYS AS IDENTITY,
    started_at    timestamptz NOT NULL,
    -- Null while the outage is open.
    ended_at      timestamptz,
    detected_by   text        NOT NULL,
    component     text        NOT NULL,
    reason        text,
    reconciled_at timestamptz,
    CONSTRAINT pk_broadcast_outages PRIMARY KEY (id),
    CONSTRAINT ck_broadcast_outages_detected_by CHECK (
        detected_by IN ('icecast', 'liquidsoap_callback', 'liquidsoap_control', 'backend')
    ),
    CONSTRAINT ck_broadcast_outages_component CHECK (
        component IN ('icecast', 'liquidsoap', 'backend', 'network')
    ),
    CONSTRAINT ck_broadcast_outages_ended_after_start CHECK (ended_at >= started_at)
);

CREATE INDEX ix_broadcast_outages_started_at ON broadcast_outages (started_at);
-- At most one open outage. NULLS NOT DISTINCT is required: in a plain unique
-- index two NULLs never collide.
CREATE UNIQUE INDEX uq_broadcast_outages_open
    ON broadcast_outages (ended_at) NULLS NOT DISTINCT WHERE ended_at IS NULL;

-- What aired (aired, truncated) and what did not (missed). Aired rows are
-- kept 10 days; missed rows until resolved (§4.1).
CREATE TABLE play_log (
    id                 bigint GENERATED ALWAYS AS IDENTITY,
    track_id           bigint      NOT NULL,
    -- Copied from the track when logged, for artist separation.
    artist_id          bigint,
    status             text        NOT NULL,
    -- Null only for tracks that were never planned (emergency playlist).
    planned_start_utc  timestamptz,
    -- Null for missed items.
    started_at         timestamptz,
    planned_out_ms     integer,
    actual_duration_ms integer,
    truncated          boolean     NOT NULL DEFAULT false,
    source             text        NOT NULL DEFAULT 'auto',
    clock_id           bigint,
    grid_slot_id       bigint,
    -- Trace ids of scheduled_items rows. No FK: those rows are deleted once
    -- they move here (§4.1).
    scheduled_item_id  bigint,
    -- Set for missed items.
    outage_id          bigint,
    -- Missed items only.
    resolution         text,
    resolved_at        timestamptz,
    resolved_item_id   bigint,
    CONSTRAINT pk_play_log PRIMARY KEY (id),
    CONSTRAINT fk_play_log_track_id FOREIGN KEY (track_id)
        REFERENCES tracks (id) ON DELETE RESTRICT,
    CONSTRAINT fk_play_log_artist_id FOREIGN KEY (artist_id)
        REFERENCES artists (id) ON DELETE RESTRICT,
    CONSTRAINT fk_play_log_clock_id FOREIGN KEY (clock_id)
        REFERENCES clocks (id) ON DELETE RESTRICT,
    -- Grid edits must not be blocked by history.
    CONSTRAINT fk_play_log_grid_slot_id FOREIGN KEY (grid_slot_id)
        REFERENCES grid_slots (id) ON DELETE SET NULL,
    CONSTRAINT fk_play_log_outage_id FOREIGN KEY (outage_id)
        REFERENCES broadcast_outages (id) ON DELETE RESTRICT,
    CONSTRAINT ck_play_log_status CHECK (status IN ('aired', 'truncated', 'missed')),
    -- 'emergency': played from the emergency playlist (§8.6, init plan §9.4).
    CONSTRAINT ck_play_log_source CHECK (source IN ('auto', 'live', 'emergency')),
    CONSTRAINT ck_play_log_resolution CHECK (resolution IN ('open', 'rescheduled', 'dismissed')),
    CONSTRAINT ck_play_log_truncated CHECK (truncated = (status = 'truncated')),
    CONSTRAINT ck_play_log_planned_out_ms CHECK (planned_out_ms > 0),
    CONSTRAINT ck_play_log_actual_duration_ms CHECK (actual_duration_ms >= 0),
    CONSTRAINT ck_play_log_planned CHECK (source = 'emergency' OR planned_start_utc IS NOT NULL),
    CONSTRAINT ck_play_log_aired_fields CHECK (
        status = 'missed'
        OR (started_at IS NOT NULL AND actual_duration_ms IS NOT NULL AND resolution IS NULL)
    ),
    CONSTRAINT ck_play_log_missed_fields CHECK (
        status <> 'missed'
        OR (outage_id IS NOT NULL AND resolution IS NOT NULL AND planned_start_utc IS NOT NULL)
    ),
    CONSTRAINT ck_play_log_resolved CHECK (
        resolution IS NULL OR (resolution = 'open') = (resolved_at IS NULL)
    ),
    CONSTRAINT ck_play_log_rescheduled CHECK (
        resolution IS DISTINCT FROM 'rescheduled' OR resolved_item_id IS NOT NULL
    )
);

-- Separation checks (§4); they filter on status. Not partial, because the
-- track and artist in-use checks must also see missed rows.
CREATE INDEX ix_play_log_track_id_started_at ON play_log (track_id, started_at DESC);
CREATE INDEX ix_play_log_artist_id_started_at ON play_log (artist_id, started_at DESC);
-- Played-tracks list and missed-item handling.
CREATE INDEX ix_play_log_status_planned_start_utc ON play_log (status, planned_start_utc);
CREATE INDEX ix_play_log_outage_id ON play_log (outage_id);
-- Clock in-use check.
CREATE INDEX ix_play_log_clock_id ON play_log (clock_id);

-- The rolling track plan (24 to 48 hours ahead).
CREATE TABLE scheduled_items (
    id                bigint GENERATED ALWAYS AS IDENTITY,
    grid_slot_id      bigint,
    clock_id          bigint      NOT NULL,
    track_id          bigint      NOT NULL,
    planned_start_utc timestamptz NOT NULL,
    planned_in_ms     integer     NOT NULL DEFAULT 0,
    -- Out point relative to the start of the track (§8.1).
    planned_out_ms    integer     NOT NULL,
    truncated         boolean     NOT NULL DEFAULT false,
    source            text        NOT NULL,
    status            text        NOT NULL DEFAULT 'planned',
    outage_id         bigint,
    missed_at         timestamptz,
    -- The missed item this one replaces. It lives in play_log, because
    -- missed items are moved there (§4.1).
    reschedule_of_id  bigint,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_scheduled_items PRIMARY KEY (id),
    CONSTRAINT fk_scheduled_items_grid_slot_id FOREIGN KEY (grid_slot_id)
        REFERENCES grid_slots (id) ON DELETE SET NULL,
    CONSTRAINT fk_scheduled_items_clock_id FOREIGN KEY (clock_id)
        REFERENCES clocks (id) ON DELETE RESTRICT,
    CONSTRAINT fk_scheduled_items_track_id FOREIGN KEY (track_id)
        REFERENCES tracks (id) ON DELETE RESTRICT,
    CONSTRAINT fk_scheduled_items_outage_id FOREIGN KEY (outage_id)
        REFERENCES broadcast_outages (id) ON DELETE RESTRICT,
    -- Retention must never be blocked by a plan row.
    CONSTRAINT fk_scheduled_items_reschedule_of_id FOREIGN KEY (reschedule_of_id)
        REFERENCES play_log (id) ON DELETE SET NULL,
    CONSTRAINT ck_scheduled_items_planned_in_ms CHECK (planned_in_ms >= 0),
    CONSTRAINT ck_scheduled_items_planned_out_ms CHECK (planned_out_ms > planned_in_ms),
    CONSTRAINT ck_scheduled_items_source CHECK (
        source IN ('clock', 'filler_pool', 'filler_random', 'filler_answer', 'reschedule')
    ),
    CONSTRAINT ck_scheduled_items_status CHECK (
        status IN ('planned', 'queued', 'aired', 'missed', 'replaced')
    ),
    CONSTRAINT ck_scheduled_items_reschedule_of CHECK (reschedule_of_id IS NULL OR source = 'reschedule'),
    CONSTRAINT ck_scheduled_items_missed_fields CHECK (
        status <> 'missed' OR (outage_id IS NOT NULL AND missed_at IS NOT NULL)
    )
);

CREATE INDEX ix_scheduled_items_planned_start_utc ON scheduled_items (planned_start_utc);
-- Separation checks against planned items.
CREATE INDEX ix_scheduled_items_track_id_planned_start_utc
    ON scheduled_items (track_id, planned_start_utc DESC);
-- Outage reconciliation.
CREATE INDEX ix_scheduled_items_status_outage_id ON scheduled_items (status, outage_id);
-- Retention deletes play_log rows; each delete checks this FK.
CREATE INDEX ix_scheduled_items_reschedule_of_id
    ON scheduled_items (reschedule_of_id) WHERE reschedule_of_id IS NOT NULL;

CREATE TABLE filler_requests (
    id              bigint GENERATED ALWAYS AS IDENTITY,
    clock_id        bigint      NOT NULL,
    hour_start_utc  timestamptz NOT NULL,
    reason          text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Counted from air time, not from creation (§7).
    deadline_at     timestamptz NOT NULL,
    mode            text        NOT NULL,
    answer_track_id bigint,
    status          text        NOT NULL DEFAULT 'pending',
    CONSTRAINT pk_filler_requests PRIMARY KEY (id),
    CONSTRAINT fk_filler_requests_clock_id FOREIGN KEY (clock_id)
        REFERENCES clocks (id) ON DELETE CASCADE,
    CONSTRAINT fk_filler_requests_answer_track_id FOREIGN KEY (answer_track_id)
        REFERENCES tracks (id) ON DELETE SET NULL,
    CONSTRAINT ck_filler_requests_reason CHECK (reason IN ('short_clock', 'pool_empty')),
    CONSTRAINT ck_filler_requests_mode CHECK (
        mode IN ('none', 'ask', 'random', 'playlist', 'artist', 'pool')
    ),
    CONSTRAINT ck_filler_requests_status CHECK (
        status IN ('pending', 'answered', 'defaulted', 'expired')
    )
);

-- At most one pending request per clock hour (§4).
CREATE UNIQUE INDEX uq_filler_requests_clock_id_hour_start_utc_pending
    ON filler_requests (clock_id, hour_start_utc) WHERE status = 'pending';

-- Settings and operations (§6.3, §6.5) --------------------------------------------

-- Only saved values; defaults live in code (init plan Phase 4).
CREATE TABLE settings (
    key        text        NOT NULL,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_settings PRIMARY KEY (key),
    CONSTRAINT ck_settings_key CHECK (key ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$')
);

CREATE TABLE operations (
    id             bigint GENERATED ALWAYS AS IDENTITY,
    kind           text        NOT NULL,
    status         text        NOT NULL DEFAULT 'running',
    started_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    error_message  text,
    result_summary text,
    CONSTRAINT pk_operations PRIMARY KEY (id),
    CONSTRAINT ck_operations_kind_not_blank CHECK (btrim(kind) <> ''),
    CONSTRAINT ck_operations_status CHECK (status IN ('running', 'completed', 'failed')),
    CONSTRAINT ck_operations_finished CHECK ((status = 'running') = (finished_at IS NULL)),
    CONSTRAINT ck_operations_failed_message CHECK (status <> 'failed' OR error_message IS NOT NULL)
);
