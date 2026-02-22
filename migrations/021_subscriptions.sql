-- Monthly subscription (abonement) for restaurant_admin and driver.
CREATE TABLE IF NOT EXISTS subscriptions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tg_user_id BIGINT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('restaurant_admin', 'driver')),
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'expired', 'paused')),
  start_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  last_payment_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(tg_user_id, role)
);

CREATE INDEX IF NOT EXISTS idx_subscriptions_tg_user_role ON subscriptions(tg_user_id, role);
CREATE INDEX IF NOT EXISTS idx_subscriptions_expires_at ON subscriptions(expires_at);
CREATE INDEX IF NOT EXISTS idx_subscriptions_status ON subscriptions(status);

-- Manual payment receipts (MVP).
CREATE TABLE IF NOT EXISTS payment_receipts (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tg_user_id BIGINT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('restaurant_admin', 'driver')),
  amount NUMERIC NOT NULL,
  currency TEXT NOT NULL DEFAULT 'UZS',
  paid_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  note TEXT,
  approved_by BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_payment_receipts_tg_user ON payment_receipts(tg_user_id);
