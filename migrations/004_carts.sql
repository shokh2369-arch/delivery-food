-- Carts table for storing user shopping carts
CREATE TABLE IF NOT EXISTS carts (
  user_id BIGINT PRIMARY KEY,
  items JSONB NOT NULL DEFAULT '[]'::jsonb,
  items_total BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_carts_updated_at ON carts(updated_at);
