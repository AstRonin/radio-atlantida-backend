-- 0003: typed filler source columns with real foreign keys.
--
-- 0001 stored the filler source in one column (a playlist id or an artist id,
-- depending on the mode), which could not have a foreign key. Two typed
-- columns let the database refuse to delete a playlist or an artist that a
-- clock uses as filler, and name the constraint, like every other in-use
-- refusal. The API still exposes one filler_source_id field and maps it.
--
-- The old columns are dropped in the same migration. This is not a contract
-- step over released code: no release and no code has used them yet.

ALTER TABLE clock_elements
    DROP CONSTRAINT ck_clock_elements_filler_source,
    DROP COLUMN filler_source_id,
    ADD COLUMN filler_playlist_id bigint,
    ADD COLUMN filler_artist_id bigint,
    ADD CONSTRAINT fk_clock_elements_filler_playlist_id FOREIGN KEY (filler_playlist_id)
        REFERENCES playlists (id) ON DELETE RESTRICT,
    ADD CONSTRAINT fk_clock_elements_filler_artist_id FOREIGN KEY (filler_artist_id)
        REFERENCES artists (id) ON DELETE RESTRICT,
    ADD CONSTRAINT ck_clock_elements_filler_playlist CHECK (
        (filler_mode = 'playlist') = (filler_playlist_id IS NOT NULL)
    ),
    ADD CONSTRAINT ck_clock_elements_filler_artist CHECK (
        (filler_mode = 'artist') = (filler_artist_id IS NOT NULL)
    );

ALTER TABLE clocks
    DROP CONSTRAINT ck_clocks_default_filler_source,
    DROP COLUMN default_filler_source_id,
    ADD COLUMN default_filler_playlist_id bigint,
    ADD COLUMN default_filler_artist_id bigint,
    ADD CONSTRAINT fk_clocks_default_filler_playlist_id FOREIGN KEY (default_filler_playlist_id)
        REFERENCES playlists (id) ON DELETE RESTRICT,
    ADD CONSTRAINT fk_clocks_default_filler_artist_id FOREIGN KEY (default_filler_artist_id)
        REFERENCES artists (id) ON DELETE RESTRICT,
    -- default_filler_mode is nullable, hence COALESCE.
    ADD CONSTRAINT ck_clocks_default_filler_playlist CHECK (
        COALESCE(default_filler_mode = 'playlist', false) = (default_filler_playlist_id IS NOT NULL)
    ),
    ADD CONSTRAINT ck_clocks_default_filler_artist CHECK (
        COALESCE(default_filler_mode = 'artist', false) = (default_filler_artist_id IS NOT NULL)
    );

-- Playlist and artist in-use checks (and the FK checks on delete). Partial:
-- most rows have no filler source, and `col = $1` implies NOT NULL.
CREATE INDEX ix_clock_elements_filler_playlist_id
    ON clock_elements (filler_playlist_id) WHERE filler_playlist_id IS NOT NULL;
CREATE INDEX ix_clock_elements_filler_artist_id
    ON clock_elements (filler_artist_id) WHERE filler_artist_id IS NOT NULL;
CREATE INDEX ix_clocks_default_filler_playlist_id
    ON clocks (default_filler_playlist_id) WHERE default_filler_playlist_id IS NOT NULL;
CREATE INDEX ix_clocks_default_filler_artist_id
    ON clocks (default_filler_artist_id) WHERE default_filler_artist_id IS NOT NULL;
