-- 0002: source values for filler taken from a playlist or an artist.
--
-- 0001 had no value for the `playlist` and `artist` filler modes, so such a
-- track could only be labelled filler_random. Widening a CHECK is an
-- expand-only change: existing rows and older code stay valid.

ALTER TABLE scheduled_items
    DROP CONSTRAINT ck_scheduled_items_source,
    ADD CONSTRAINT ck_scheduled_items_source CHECK (
        source IN ('clock', 'filler_pool', 'filler_random', 'filler_playlist',
                   'filler_artist', 'filler_answer', 'reschedule')
    );
