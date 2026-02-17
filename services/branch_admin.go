package services

import (
	"context"
	"fmt"

	"food-telegram/db"
)

// EnsureBranchAdminsTable makes sure the `branch_admins` table exists.
// This is a safety net for cases where migrations were not applied.
func EnsureBranchAdminsTable(ctx context.Context) error {
	var regclass *string
	if err := db.Pool.QueryRow(ctx, `SELECT to_regclass('public.branch_admins')`).Scan(&regclass); err != nil {
		return fmt.Errorf("check branch_admins table existence: %w", err)
	}
	if regclass != nil {
		return nil
	}

	// Create table + indexes (idempotent enough to run once at startup).
	_, err := db.Pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS branch_admins (
			id BIGSERIAL PRIMARY KEY,
			branch_location_id BIGINT NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
			branch_name TEXT NOT NULL DEFAULT '',
			admin_user_id BIGINT NOT NULL,
			promoted_by BIGINT NOT NULL,
			promoted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE(branch_location_id, admin_user_id)
		);

		CREATE INDEX IF NOT EXISTS idx_branch_admins_location ON branch_admins(branch_location_id);
		CREATE INDEX IF NOT EXISTS idx_branch_admins_admin ON branch_admins(admin_user_id);
	`)
	if err != nil {
		return fmt.Errorf("create branch_admins table: %w", err)
	}
	return nil
}

// AddBranchAdmin adds a new admin for a specific branch
func AddBranchAdmin(ctx context.Context, branchLocationID int64, adminUserID int64, promotedBy int64) error {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return err
	}

	// Validate that the branch location exists
	var exists bool
	err := db.Pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM locations WHERE id = $1)`,
		branchLocationID,
	).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check branch location: %w", err)
	}
	if !exists {
		return fmt.Errorf("branch location with ID %d does not exist", branchLocationID)
	}

	// Read branch name for audit/debug
	var branchName string
	if err := db.Pool.QueryRow(ctx, `SELECT name FROM locations WHERE id = $1`, branchLocationID).Scan(&branchName); err != nil {
		return fmt.Errorf("failed to read branch name (location_id=%d): %w", branchLocationID, err)
	}

	// Insert branch admin (ON CONFLICT will handle duplicates gracefully)
	_, err = db.Pool.Exec(ctx, `
		INSERT INTO branch_admins (branch_location_id, branch_name, admin_user_id, promoted_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (branch_location_id, admin_user_id) DO NOTHING`,
		branchLocationID, branchName, adminUserID, promotedBy,
	)
	if err != nil {
		return fmt.Errorf("failed to add branch admin (location_id=%d, admin_user_id=%d): %w", branchLocationID, adminUserID, err)
	}
	return nil
}

// GetBranchAdmins returns all admin user IDs for a specific branch
func GetBranchAdmins(ctx context.Context, branchLocationID int64) ([]int64, error) {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return nil, err
	}

	rows, err := db.Pool.Query(ctx, `
		SELECT admin_user_id FROM branch_admins WHERE branch_location_id = $1`,
		branchLocationID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get branch admins: %w", err)
	}
	defer rows.Close()

	var adminIDs []int64
	for rows.Next() {
		var adminID int64
		if err := rows.Scan(&adminID); err != nil {
			return nil, fmt.Errorf("failed to scan admin ID: %w", err)
		}
		adminIDs = append(adminIDs, adminID)
	}
	return adminIDs, rows.Err()
}

// RemoveBranchAdmin removes an admin from a branch
func RemoveBranchAdmin(ctx context.Context, branchLocationID int64, adminUserID int64) error {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return err
	}

	result, err := db.Pool.Exec(ctx, `
		DELETE FROM branch_admins 
		WHERE branch_location_id = $1 AND admin_user_id = $2`,
		branchLocationID, adminUserID,
	)
	if err != nil {
		return fmt.Errorf("failed to remove branch admin: %w", err)
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("branch admin not found")
	}
	return nil
}

// ListBranchAdmins returns all admins for a branch with their promotion info
type BranchAdminInfo struct {
	AdminUserID int64
	PromotedBy  int64
	PromotedAt  string
}

func ListBranchAdmins(ctx context.Context, branchLocationID int64) ([]BranchAdminInfo, error) {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return nil, err
	}

	rows, err := db.Pool.Query(ctx, `
		SELECT admin_user_id, promoted_by, promoted_at
		FROM branch_admins 
		WHERE branch_location_id = $1
		ORDER BY promoted_at DESC`,
		branchLocationID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list branch admins: %w", err)
	}
	defer rows.Close()

	var admins []BranchAdminInfo
	for rows.Next() {
		var admin BranchAdminInfo
		if err := rows.Scan(&admin.AdminUserID, &admin.PromotedBy, &admin.PromotedAt); err != nil {
			return nil, fmt.Errorf("failed to scan admin info: %w", err)
		}
		admins = append(admins, admin)
	}
	return admins, rows.Err()
}

// IsBranchAdmin checks if a user is an admin for a specific branch
func IsBranchAdmin(ctx context.Context, branchLocationID int64, userID int64) (bool, error) {
	var exists bool
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return false, err
	}
	err := db.Pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM branch_admins 
			WHERE branch_location_id = $1 AND admin_user_id = $2
		)`,
		branchLocationID, userID,
	).Scan(&exists)
	return exists, err
}
