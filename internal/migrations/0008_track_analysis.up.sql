-- 0008: track analysis for import and transcoding (docs/phase-11-plan.md §2).
--
-- Expand-only: new nullable columns, one column with a default, one index.
-- tracks holds at most a few thousand rows, so the index needs no
-- CONCURRENTLY.

ALTER TABLE tracks
    -- Codec of the audio stream as ffprobe names it (mp3, aac, vorbis,
    -- flac, pcm_s16le, ...); format is the container.
    ADD COLUMN codec             text,
    ADD COLUMN channels          integer,
    -- EBU R128 measurements of the whole file (ffmpeg loudnorm, first pass).
    ADD COLUMN loudness_lufs     double precision,
    ADD COLUMN true_peak_dbtp    double precision,
    ADD COLUMN loudness_range_lu double precision,
    -- Bits per second of the lossy file this one was transcoded from: a
    -- transcode cannot restore what a low bitrate lost, so the low_bitrate
    -- issue stays with the new file (decision 5). Null for an original.
    ADD COLUMN source_bitrate    integer,
    -- Problems found by the last analysis (decision 5). Codes only: the
    -- panel words them from the code and the measurements.
    ADD COLUMN quality_issues    text[]      NOT NULL DEFAULT '{}',
    -- Null until the file is analysed.
    ADD COLUMN analyzed_at       timestamptz,
    ADD CONSTRAINT ck_tracks_channels_positive CHECK (channels > 0),
    ADD CONSTRAINT ck_tracks_source_bitrate_positive CHECK (source_bitrate > 0),
    ADD CONSTRAINT ck_tracks_quality_issues CHECK (
        quality_issues <@ ARRAY['format', 'sample_rate', 'channels', 'loudness', 'true_peak',
                                'low_bitrate', 'file_missing']::text[]
    );

-- Duplicate check of an import (decision 4): one lookup per file.
CREATE INDEX ix_tracks_file_checksum ON tracks (file_checksum);
