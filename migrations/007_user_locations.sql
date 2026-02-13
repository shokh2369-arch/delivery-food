CREATE TABLE IF NOT EXISTS user_locations (
    user_id BIGINT PRIMARY KEY,
    location_id BIGINT NOT NULL REFERENCES locations(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

