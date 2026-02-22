-- Multi-user restaurant access: who is currently "logged in" as admin for which branch (same password).
CREATE TABLE IF NOT EXISTS branch_admin_access (
    tg_user_id BIGINT NOT NULL PRIMARY KEY,
    branch_location_id BIGINT NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
    logged_in_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_branch_admin_access_location ON branch_admin_access(branch_location_id);

-- Audit: log each restaurant admin login (any tg_user_id + which branch).
CREATE TABLE IF NOT EXISTS admin_logins (
    id BIGSERIAL PRIMARY KEY,
    tg_user_id BIGINT NOT NULL,
    branch_location_id BIGINT NOT NULL,
    logged_in_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_admin_logins_branch ON admin_logins(branch_location_id);
CREATE INDEX IF NOT EXISTS idx_admin_logins_time ON admin_logins(logged_in_at);
