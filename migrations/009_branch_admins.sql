-- Branch admins table for managing admins per branch
CREATE TABLE IF NOT EXISTS branch_admins (
    id BIGSERIAL PRIMARY KEY,
    branch_location_id BIGINT NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
    -- Snapshot of the branch name at time of promotion (for easier debugging/reporting).
    -- Source of truth remains locations.name.
    branch_name TEXT NOT NULL DEFAULT '',
    admin_user_id BIGINT NOT NULL,
    promoted_by BIGINT NOT NULL,
    promoted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(branch_location_id, admin_user_id)
);

-- Backwards/forwards compatible columns (in case table existed before we added them)
ALTER TABLE branch_admins ADD COLUMN IF NOT EXISTS branch_name TEXT NOT NULL DEFAULT '';
ALTER TABLE branch_admins ADD COLUMN IF NOT EXISTS promoted_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS idx_branch_admins_location ON branch_admins(branch_location_id);
CREATE INDEX IF NOT EXISTS idx_branch_admins_admin ON branch_admins(admin_user_id);
