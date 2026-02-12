-- Checkouts table for storing checkout state (between cart and order)
CREATE TABLE IF NOT EXISTS checkouts (
  user_id BIGINT PRIMARY KEY,
  cart_items JSONB NOT NULL DEFAULT '[]'::jsonb,
  items_total BIGINT NOT NULL DEFAULT 0,
  phone TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_checkouts_created_at ON checkouts(created_at);
