-- Add delivery_type to orders table
-- 'pickup' = customer picks up, 'delivery' = driver delivers
ALTER TABLE orders ADD COLUMN IF NOT EXISTS delivery_type TEXT CHECK (delivery_type IN ('pickup', 'delivery'));

CREATE INDEX IF NOT EXISTS idx_orders_delivery_type ON orders(delivery_type);
