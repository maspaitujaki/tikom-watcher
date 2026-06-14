-- +goose Up
CREATE TABLE subscribers (
    chat_id    BIGINT PRIMARY KEY,
    username   TEXT,
    first_name TEXT,
    is_active  BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE event_state (
    event_key        TEXT PRIMARY KEY,
    url              TEXT NOT NULL,
    last_state       TEXT NOT NULL DEFAULT 'UNKNOWN'
                       CHECK (last_state IN ('AVAILABLE', 'SOLD_OUT', 'UNKNOWN')),
    unknown_streak   INT  NOT NULL DEFAULT 0,
    last_checked_at  TIMESTAMPTZ,
    last_changed_at  TIMESTAMPTZ,
    last_notified_at TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE state_transitions (
    id          BIGSERIAL PRIMARY KEY,
    event_key   TEXT NOT NULL,
    from_state  TEXT,
    to_state    TEXT NOT NULL,
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_state_transitions_event ON state_transitions (event_key, observed_at);

-- +goose Down
DROP TABLE IF EXISTS state_transitions;
DROP TABLE IF EXISTS event_state;
DROP TABLE IF EXISTS subscribers;
