-- Reject-in-progress: superadmin has clicked Reject and is about to send reason (restart-safe for Zayafka).
ALTER TABLE applications ADD COLUMN IF NOT EXISTS reject_in_progress_by BIGINT NULL;

CREATE INDEX IF NOT EXISTS idx_applications_reject_in_progress_by ON applications(reject_in_progress_by) WHERE reject_in_progress_by IS NOT NULL;
