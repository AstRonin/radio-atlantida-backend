-- 0010: an index for the folder lookups of the import (docs/phase-18-plan.md
-- §5).
--
-- Expand-only: one index. uq_tracks_file_path compares in the database
-- collation, so it cannot serve "every track under music/"; the pattern
-- operator class compares bytes, which a prefix range needs. tracks holds
-- at most a few thousand rows, so the index needs no CONCURRENTLY.

CREATE INDEX ix_tracks_file_path ON tracks (file_path text_pattern_ops);
