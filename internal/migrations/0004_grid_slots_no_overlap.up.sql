-- 0004: at most one clock per weekday and hour on any date.
--
-- Two grid slots for the same weekday and hour must not have overlapping
-- validity ranges. Saving an overlapping slot replaces the old one for the
-- overlapping dates; the API does that in one transaction (Phase 6), and this
-- constraint guarantees that no code path leaves two clocks in one hour.
-- A violation is SQLSTATE 23P01 (exclusion_violation).
--
-- btree_gist provides the `=` operator class for smallint in a GiST index.
-- It is a trusted extension, so the database owner may create it.

CREATE EXTENSION IF NOT EXISTS btree_gist;

ALTER TABLE grid_slots
    -- Both ends inclusive; a null valid_to is an open end.
    ADD CONSTRAINT ex_grid_slots_weekday_hour_valid EXCLUDE USING gist (
        weekday WITH =,
        hour WITH =,
        daterange(valid_from, valid_to, '[]') WITH &&
    );
