-- Order status: new (waiting) -> preparing -> ready -> completed
-- Link order to restaurant (location) for admin access control
ALTER TABLE orders ADD COLUMN IF NOT EXISTS location_id BIGINT REFERENCES locations(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_orders_location_id ON orders(location_id);

-- Use status values: new, preparing, ready, completed
ALTER TABLE orders ALTER COLUMN status SET DEFAULT 'new';
