-- 0009: broadcast state (docs/phase-16-plan.md §3).
--
-- Expand-only: one new table. One row, written only when the derived
-- broadcast state changes, so the panel's timer survives a restart of the
-- backend. The row is inserted by the first transition: an empty table
-- means the state is unknown. Unknown is never stored.
CREATE TABLE broadcast_state (
    id         smallint    NOT NULL,
    state      text        NOT NULL,
    -- The moment of the last state change (decision 1).
    since      timestamptz NOT NULL,
    -- The open outage's reason or component, for the panel's tooltip.
    reason     text,
    updated_at timestamptz NOT NULL,
    CONSTRAINT pk_broadcast_state PRIMARY KEY (id),
    CONSTRAINT ck_broadcast_state_one_row CHECK (id = 1),
    CONSTRAINT ck_broadcast_state_state CHECK (state IN ('connected', 'reconnecting', 'unreachable'))
);
