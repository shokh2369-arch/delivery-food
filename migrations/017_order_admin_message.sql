-- Add admin message tracking to orders table
ALTER TABLE orders ADD COLUMN IF NOT EXISTS admin_chat_id BIGINT;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS admin_message_id INT;

CREATE INDEX IF NOT EXISTS idx_orders_admin_message ON orders(admin_chat_id, admin_message_id);
