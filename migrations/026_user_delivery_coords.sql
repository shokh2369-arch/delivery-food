-- Persist customer delivery coordinates when they share location (so fee calculation works across restarts/callbacks).
CREATE TABLE IF NOT EXISTS user_delivery_coords (
    user_id BIGINT PRIMARY KEY,
    lat DOUBLE PRECISION NOT NULL,
    lon DOUBLE PRECISION NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
