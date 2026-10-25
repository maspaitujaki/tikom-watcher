-- +goose Up
-- Health counters for the daily recap: reset each time a recap is sent.
ALTER TABLE event_state
    ADD COLUMN checks_since_recap  INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN unknown_since_recap INTEGER NOT NULL DEFAULT 0;

-- Single-row table (id is pinned to 1) recording when the last recap went out, so
-- a restart neither re-sends nor skips the day's recap.
CREATE TABLE recap_state (
    id            SMALLINT    PRIMARY KEY CHECK (id = 1),
    last_recap_at TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE recap_state;
ALTER TABLE event_state
    DROP COLUMN checks_since_recap,
    DROP COLUMN unknown_since_recap;
