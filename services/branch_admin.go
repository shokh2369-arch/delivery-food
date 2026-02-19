package services

import (
	"context"
	"errors"
	"fmt"

	"food-telegram/db"
	"golang.org/x/crypto/bcrypt"
	"github.com/jackc/pgx/v5"
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

	// Create table + indexes (idempotent enough to run once at startup). One admin per location.
	_, err := db.Pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS branch_admins (
			id BIGSERIAL PRIMARY KEY,
			branch_location_id BIGINT NOT NULL REFERENCES locations(id) ON DELETE CASCADE,
			branch_name TEXT NOT NULL DEFAULT '',
			admin_user_id BIGINT NOT NULL,
			promoted_by BIGINT NOT NULL,
			promoted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			password_hash TEXT,
			UNIQUE(branch_location_id)
		);

		CREATE INDEX IF NOT EXISTS idx_branch_admins_location ON branch_admins(branch_location_id);
		CREATE INDEX IF NOT EXISTS idx_branch_admins_admin ON branch_admins(admin_user_id);
	`)
	if err != nil {
		return fmt.Errorf("create branch_admins table: %w", err)
	}
	return nil
}

// HashBranchAdminPassword returns a bcrypt hash of the plain password for storing in DB.
func HashBranchAdminPassword(plain string) (string, error) {
	if plain == "" {
		return "", fmt.Errorf("password cannot be empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(hash), nil
}

// IsBranchAdminPasswordUnique returns true if no branch admin row has this password hash (ensures unique passwords).
// excludeLocationID: when > 0, ignore the row for this location (used when replacing that location's admin).
func IsBranchAdminPasswordUnique(ctx context.Context, passwordHash string, excludeLocationID int64) (bool, error) {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return false, err
	}
	var count int
	if excludeLocationID > 0 {
		err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM branch_admins WHERE password_hash = $1 AND branch_location_id != $2`, passwordHash, excludeLocationID).Scan(&count)
		if err != nil {
			return false, fmt.Errorf("check password uniqueness: %w", err)
		}
	} else {
		err := db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM branch_admins WHERE password_hash = $1`, passwordHash).Scan(&count)
		if err != nil {
			return false, fmt.Errorf("check password uniqueness: %w", err)
		}
	}
	return count == 0, nil
}

// AuthenticateBranchAdmin checks userID + plainPassword; returns the branch location ID if valid.
func AuthenticateBranchAdmin(ctx context.Context, userID int64, plainPassword string) (branchLocationID int64, ok bool, err error) {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return 0, false, err
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT branch_location_id, password_hash FROM branch_admins WHERE admin_user_id = $1 AND password_hash IS NOT NULL`,
		userID,
	)
	if err != nil {
		return 0, false, fmt.Errorf("query branch admins: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var locID int64
		var hash string
		if err := rows.Scan(&locID, &hash); err != nil {
			return 0, false, err
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(plainPassword)) == nil {
			return locID, true, nil
		}
	}
	return 0, false, rows.Err()
}

// AddBranchAdmin adds a new admin for a specific branch with a unique password (hash).
func AddBranchAdmin(ctx context.Context, branchLocationID int64, adminUserID int64, promotedBy int64, passwordHash string) error {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return err
	}
	if passwordHash == "" {
		return fmt.Errorf("password is required for branch admin")
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

	// Ensure password is unique (allow re-use when replacing this location's admin)
	unique, err := IsBranchAdminPasswordUnique(ctx, passwordHash, branchLocationID)
	if err != nil {
		return err
	}
	if !unique {
		return fmt.Errorf("this password is already used by another branch admin; choose a unique password")
	}

	// Read branch name for audit/debug
	var branchName string
	if err := db.Pool.QueryRow(ctx, `SELECT name FROM locations WHERE id = $1`, branchLocationID).Scan(&branchName); err != nil {
		return fmt.Errorf("failed to read branch name (location_id=%d): %w", branchLocationID, err)
	}

	// One admin per location: upsert by branch_location_id (replaces existing admin if any)
	_, err = db.Pool.Exec(ctx, `
		INSERT INTO branch_admins (branch_location_id, branch_name, admin_user_id, promoted_by, password_hash)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (branch_location_id) DO UPDATE SET
			branch_name = EXCLUDED.branch_name,
			admin_user_id = EXCLUDED.admin_user_id,
			promoted_by = EXCLUDED.promoted_by,
			password_hash = EXCLUDED.password_hash,
			promoted_at = now()`,
		branchLocationID, branchName, adminUserID, promotedBy, passwordHash,
	)
	if err != nil {
		return fmt.Errorf("failed to add branch admin (location_id=%d, admin_user_id=%d): %w", branchLocationID, adminUserID, err)
	}
	return nil
}

// GetAdminLocationID returns the location (restaurant) ID for which the user is the branch admin, or 0 if none.
// Used to validate that a restaurant admin only updates orders for their own restaurant.
func GetAdminLocationID(ctx context.Context, adminUserID int64) (int64, error) {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return 0, err
	}
	var locID int64
	err := db.Pool.QueryRow(ctx, `SELECT branch_location_id FROM branch_admins WHERE admin_user_id = $1 LIMIT 1`, adminUserID).Scan(&locID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return locID, nil
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

// RemoveAllBranchAdminsForLocation removes all branch admins for a location (e.g. before changing admin).
func RemoveAllBranchAdminsForLocation(ctx context.Context, branchLocationID int64) error {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return err
	}
	_, err := db.Pool.Exec(ctx, `DELETE FROM branch_admins WHERE branch_location_id = $1`, branchLocationID)
	if err != nil {
		return fmt.Errorf("failed to remove branch admins for location %d: %w", branchLocationID, err)
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
