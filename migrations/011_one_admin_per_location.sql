-- Enforce one admin per location: keep one row per branch_location_id, then unique on branch_location_id.

-- Remove duplicates: keep the row with the latest promoted_at per location
DELETE FROM branch_admins a
USING branch_admins b
WHERE a.branch_location_id = b.branch_location_id AND a.id < b.id;

-- Drop old unique constraint (name from standard PostgreSQL naming)
ALTER TABLE branch_admins DROP CONSTRAINT IF EXISTS branch_admins_branch_location_id_admin_user_id_key;

-- One admin per location (idempotent: only add if not exists)
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'branch_admins_branch_location_id_key'
    ) THEN
        ALTER TABLE branch_admins ADD CONSTRAINT branch_admins_branch_location_id_key UNIQUE (branch_location_id);
    END IF;
END $$;
