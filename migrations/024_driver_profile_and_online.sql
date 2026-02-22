-- Driver profile fields and is_online for simplified onboarding (no password).
ALTER TABLE drivers ADD COLUMN IF NOT EXISTS full_name TEXT;
ALTER TABLE drivers ADD COLUMN IF NOT EXISTS car_model TEXT;
ALTER TABLE drivers ADD COLUMN IF NOT EXISTS car_color TEXT;
ALTER TABLE drivers ADD COLUMN IF NOT EXISTS is_online BOOLEAN NOT NULL DEFAULT false;

-- Sync existing rows: is_online = (status = 'online')
UPDATE drivers SET is_online = (status = 'online');
