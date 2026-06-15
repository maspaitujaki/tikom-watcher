-- +goose Up
-- Separate cooldown tracking for the AVAILABLE -> SOLD_OUT alert direction.
-- last_notified_at remains the AVAILABLE (drop) cooldown timestamp.
ALTER TABLE event_state ADD COLUMN last_soldout_notified_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE event_state DROP COLUMN last_soldout_notified_at;
