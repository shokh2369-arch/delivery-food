-- Soft login throttle (cooldown only, no hard block).
CREATE TABLE IF NOT EXISTS login_throttle (
  tg_user_id BIGINT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('superadmin', 'restaurant_admin', 'driver')),
  fail_count INT NOT NULL DEFAULT 0,
  last_failed_at TIMESTAMPTZ,
  cooldown_until TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tg_user_id, role)
);

CREATE INDEX IF NOT EXISTS idx_login_throttle_cooldown ON login_throttle(cooldown_until) WHERE cooldown_until IS NOT NULL;
