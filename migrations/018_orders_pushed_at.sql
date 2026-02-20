-- When delivery is selected, we set pushed_at to prevent duplicate push to drivers.
-- Push is only sent when pushed_at IS NULL; if already set within 60s we skip.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS pushed_at TIMESTAMPTZ;
