-- Orders table with delivery fields (single source of truth)
-- Rule: grand_total = items_total + delivery_fee
CREATE TABLE IF NOT EXISTS orders (
  id BIGSERIAL PRIMARY KEY,
  user_id BIGINT NOT NULL,
  chat_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',

  lat DOUBLE PRECISION NOT NULL,
  lon DOUBLE PRECISION NOT NULL,
  distance_km DOUBLE PRECISION NOT NULL,
  rate_per_km BIGINT NOT NULL DEFAULT 2000,

  delivery_fee BIGINT NOT NULL,
  items_total BIGINT NOT NULL,
  grand_total BIGINT NOT NULL,

  delivery_fee_overridden BOOLEAN NOT NULL DEFAULT false,
  delivery_fee_override_by BIGINT NOT NULL DEFAULT 0,
  delivery_fee_override_note TEXT NOT NULL DEFAULT '',
  delivery_fee_override_at TIMESTAMPTZ NULL,

  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_orders_created_at ON orders(created_at);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE INDEX IF NOT EXISTS idx_orders_delivery_overridden ON orders(delivery_fee_overridden) WHERE delivery_fee_overridden = true;
