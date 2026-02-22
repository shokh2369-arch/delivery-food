-- Applications (ariza) for restaurant admin and driver self-onboarding.
CREATE TABLE IF NOT EXISTS applications (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  type TEXT NOT NULL CHECK (type IN ('restaurant_admin','driver')),
  tg_user_id BIGINT NOT NULL,
  chat_id BIGINT NOT NULL,
  full_name TEXT,
  phone TEXT,
  language TEXT NOT NULL DEFAULT 'uz',
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  reviewed_by BIGINT,
  reviewed_at TIMESTAMPTZ,
  reject_reason TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_applications_pending_per_user_type
  ON applications (type, tg_user_id) WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_applications_status ON applications(status);
CREATE INDEX IF NOT EXISTS idx_applications_tg_user_id ON applications(tg_user_id);

-- Restaurant-specific application details.
CREATE TABLE IF NOT EXISTS application_restaurant_details (
  application_id UUID NOT NULL PRIMARY KEY REFERENCES applications(id) ON DELETE CASCADE,
  restaurant_name TEXT NOT NULL,
  lat DOUBLE PRECISION NOT NULL,
  lon DOUBLE PRECISION NOT NULL,
  address TEXT
);

-- Driver-specific application details.
CREATE TABLE IF NOT EXISTS application_driver_details (
  application_id UUID NOT NULL PRIMARY KEY REFERENCES applications(id) ON DELETE CASCADE,
  car_plate TEXT,
  car_model TEXT
);

-- Credentials for approved applicants (login with generated password).
CREATE TABLE IF NOT EXISTS user_credentials (
  tg_user_id BIGINT NOT NULL PRIMARY KEY,
  role TEXT NOT NULL CHECK (role IN ('restaurant_admin','driver')),
  password_hash TEXT NOT NULL,
  is_active BOOLEAN NOT NULL DEFAULT true,
  expires_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_user_credentials_role ON user_credentials(role);

-- Rate limit login attempts (per tg_user_id, 5 tries per 10 min).
CREATE TABLE IF NOT EXISTS login_attempts (
  tg_user_id BIGINT NOT NULL,
  attempted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  success BOOLEAN NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_login_attempts_user_time ON login_attempts(tg_user_id, attempted_at);
