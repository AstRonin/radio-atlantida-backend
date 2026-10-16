-- 0012: cue points and fades of a track, the in point of a logged item, and
-- the cached waveform (docs/phase-22-plan.md §3.1).
--
-- Expand-only: columns with defaults or nullable, and a new table, so the
-- previous code works against the new schema (it plays the whole file, as
-- before). The checks against duration_ms are in the service: the duration
-- changes with the file.

ALTER TABLE tracks
    -- Start of the on-air part, from the start of the file.
    ADD COLUMN cue_in_ms   integer NOT NULL DEFAULT 0,
    -- End of the on-air part; null is the end of the file.
    ADD COLUMN cue_out_ms  integer,
    -- Fades at the cue points, applied by Liquidsoap; 0 is a hard cut.
    ADD COLUMN fade_in_ms  integer NOT NULL DEFAULT 0,
    ADD COLUMN fade_out_ms integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT ck_tracks_cue CHECK (cue_in_ms >= 0 AND (cue_out_ms IS NULL OR cue_out_ms > cue_in_ms)),
    ADD CONSTRAINT ck_tracks_fades CHECK (fade_in_ms BETWEEN 0 AND 5000 AND fade_out_ms BETWEEN 0 AND 5000);

-- The in point the item was planned with, so its length is
-- planned_out_ms - planned_in_ms after it moved here.
ALTER TABLE play_log ADD COLUMN planned_in_ms integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT ck_play_log_planned_in_ms CHECK (planned_in_ms >= 0);

-- The waveform of a track file for the cue editor, computed on first
-- request and valid while the track's file_checksum equals file_checksum.
CREATE TABLE track_waveforms (
    track_id      bigint      NOT NULL,
    file_checksum text        NOT NULL,
    bucket_ms     integer     NOT NULL,
    -- One signed byte each for the min and the max sample of every bucket,
    -- scaled to -127..127.
    peaks         bytea       NOT NULL,
    duration_ms   integer     NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pk_track_waveforms PRIMARY KEY (track_id),
    CONSTRAINT fk_track_waveforms_track_id FOREIGN KEY (track_id)
        REFERENCES tracks (id) ON DELETE CASCADE,
    CONSTRAINT ck_track_waveforms_bucket_ms CHECK (bucket_ms > 0),
    CONSTRAINT ck_track_waveforms_duration_ms CHECK (duration_ms >= 0)
);
