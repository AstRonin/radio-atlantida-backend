-- 0020: the slot claim of a pinned rule track (docs/track-pin-rule-plan.md). A
-- pin of a rule row takes over one slot of its rule playlist in that hour, so
-- the hour keeps the same number of rule tracks. rule_origin_ms is the row's
-- offset when it was pinned and tells which slot. Both are null for every other
-- one-time insert. ON DELETE SET NULL keeps the pin when its playlist is
-- removed; the claim then claims nothing. Rows are planned offsets in
-- milliseconds, so the origin need not be whole seconds. No index: the columns
-- are not joined. Expand-only: two nullable columns and their constraints.

ALTER TABLE hour_inserts
    ADD COLUMN rule_playlist_id bigint,
    ADD COLUMN rule_origin_ms integer,
    ADD CONSTRAINT fk_hour_inserts_rule_playlist_id FOREIGN KEY (rule_playlist_id)
        REFERENCES playlists (id) ON DELETE SET NULL,
    ADD CONSTRAINT ck_hour_inserts_rule_origin_ms CHECK (
        rule_origin_ms IS NULL OR rule_origin_ms BETWEEN 0 AND 3599999
    ),
    ADD CONSTRAINT ck_hour_inserts_rule_fixed CHECK (
        rule_playlist_id IS NULL OR placement = 'fixed'
    );
