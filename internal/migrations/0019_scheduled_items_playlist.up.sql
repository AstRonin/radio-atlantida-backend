-- 0019: the playlist a planned item was drawn from. A fill item is drawn from a
-- fill playlist and a rule item from its rule playlist; the track plan shows the
-- playlist and its type. Set when the hour is stored (every build, rebuild,
-- reschedule and hour insert stores its items the same way); null for general
-- items and for the items built before this migration, until their hour is
-- rebuilt. ON DELETE SET NULL keeps the items when a playlist is removed.
-- Expand-only: one nullable column, its foreign key and a partial index.

ALTER TABLE scheduled_items
    ADD COLUMN playlist_id bigint,
    ADD CONSTRAINT fk_scheduled_items_playlist_id FOREIGN KEY (playlist_id)
        REFERENCES playlists (id) ON DELETE SET NULL;

CREATE INDEX ix_scheduled_items_playlist_id ON scheduled_items (playlist_id)
    WHERE playlist_id IS NOT NULL;
