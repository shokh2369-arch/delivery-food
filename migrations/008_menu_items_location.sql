ALTER TABLE menu_items
    ADD COLUMN IF NOT EXISTS location_id BIGINT REFERENCES locations(id);

CREATE INDEX IF NOT EXISTS idx_menu_items_location ON menu_items(location_id);

