-- Customer chooses delivery_type at checkout; default pickup for existing rows.
UPDATE orders SET delivery_type = 'pickup' WHERE delivery_type IS NULL;
ALTER TABLE orders ALTER COLUMN delivery_type SET DEFAULT 'pickup';
ALTER TABLE orders ALTER COLUMN delivery_type SET NOT NULL;
