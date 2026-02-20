-- One evolving order card per audience: admin, customer, driver.
-- Pointers used to edit existing messages instead of sending new ones.
CREATE TABLE IF NOT EXISTS order_message_pointers (
  order_id BIGINT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  audience TEXT NOT NULL CHECK (audience IN ('admin','customer','driver')),
  chat_id BIGINT NOT NULL,
  message_id INT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (order_id, audience)
);

CREATE INDEX IF NOT EXISTS idx_order_message_pointers_order_id ON order_message_pointers(order_id);
