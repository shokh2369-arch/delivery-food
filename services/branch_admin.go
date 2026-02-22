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
	if regclass == nil {
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
	}
	// Add order_lang column if missing (admin receives order cards in this language: uz or ru)
	_, _ = db.Pool.Exec(ctx, `ALTER TABLE branch_admins ADD COLUMN IF NOT EXISTS order_lang VARCHAR(2) NOT NULL DEFAULT 'uz'`)
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

// AuthenticateBranchAdmin checks plainPassword against any branch's password; on match records this tg_user_id as having access and logs the login. Multiple users can log in with the same restaurant password.
func AuthenticateBranchAdmin(ctx context.Context, userID int64, plainPassword string) (branchLocationID int64, ok bool, err error) {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return 0, false, err
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT branch_location_id, password_hash FROM branch_admins WHERE password_hash IS NOT NULL`,
	)
	if err != nil {
		return 0, false, fmt.Errorf("query branch admins: %w", err)
	}
	defer rows.Close()
	var matchedLocID int64
	for rows.Next() {
		var locID int64
		var hash string
		if err := rows.Scan(&locID, &hash); err != nil {
			return 0, false, err
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(plainPassword)) == nil {
			matchedLocID = locID
			break
		}
	}
	if matchedLocID == 0 {
		return 0, false, rows.Err()
	}
	// Record session and audit
	_, _ = db.Pool.Exec(ctx, `
		INSERT INTO branch_admin_access (tg_user_id, branch_location_id, logged_in_at)
		VALUES ($1, $2, now())
		ON CONFLICT (tg_user_id) DO UPDATE SET branch_location_id = $2, logged_in_at = now()`,
		userID, matchedLocID,
	)
	_, _ = db.Pool.Exec(ctx, `INSERT INTO admin_logins (tg_user_id, branch_location_id, logged_in_at) VALUES ($1, $2, now())`,
		userID, matchedLocID,
	)
	return matchedLocID, true, nil
}

// AddBranchAdmin adds a new admin for a specific branch with a unique password (hash).
// orderLang is the language in which this admin receives order cards: "uz" or "ru" (default "uz" if empty).
func AddBranchAdmin(ctx context.Context, branchLocationID int64, adminUserID int64, promotedBy int64, passwordHash string, orderLang string) error {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return err
	}
	if passwordHash == "" {
		return fmt.Errorf("password is required for branch admin")
	}
	if orderLang != "ru" {
		orderLang = "uz"
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

	_, err = db.Pool.Exec(ctx, `
		INSERT INTO branch_admins (branch_location_id, branch_name, admin_user_id, promoted_by, password_hash, order_lang)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (branch_location_id) DO UPDATE SET
			branch_name = EXCLUDED.branch_name,
			admin_user_id = EXCLUDED.admin_user_id,
			promoted_by = EXCLUDED.promoted_by,
			password_hash = EXCLUDED.password_hash,
			order_lang = EXCLUDED.order_lang,
			promoted_at = now()`,
		branchLocationID, branchName, adminUserID, promotedBy, passwordHash, orderLang,
	)
	if err != nil {
		return fmt.Errorf("failed to add branch admin (location_id=%d, admin_user_id=%d): %w", branchLocationID, adminUserID, err)
	}
	return nil
}

// GetAdminLocationID returns the location (restaurant) ID for which the user is acting as admin, or 0 if none.
// Checks branch_admin_access first (anyone who logged in with the password), then branch_admins (primary admin).
func GetAdminLocationID(ctx context.Context, adminUserID int64) (int64, error) {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return 0, err
	}
	var locID int64
	err := db.Pool.QueryRow(ctx, `SELECT branch_location_id FROM branch_admin_access WHERE tg_user_id = $1`, adminUserID).Scan(&locID)
	if err == nil && locID != 0 {
		return locID, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	err = db.Pool.QueryRow(ctx, `SELECT branch_location_id FROM branch_admins WHERE admin_user_id = $1 LIMIT 1`, adminUserID).Scan(&locID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return locID, nil
}

// GetBranchAdmins returns all admin user IDs for a specific branch (primary only from branch_admins).
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

// GetPrimaryAdminUserID returns the primary branch admin's tg_user_id for the branch (subscription holder). Returns 0 if not found.
func GetPrimaryAdminUserID(ctx context.Context, branchLocationID int64) (int64, error) {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return 0, err
	}
	var id int64
	err := db.Pool.QueryRow(ctx, `SELECT admin_user_id FROM branch_admins WHERE branch_location_id = $1`, branchLocationID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}

// MarkExpiredForBranch marks the primary admin's subscription as expired and sets their user_credentials.is_active = false (no password rotation).
func MarkExpiredForBranch(ctx context.Context, branchLocationID int64) {
	primaryID, err := GetPrimaryAdminUserID(ctx, branchLocationID)
	if err != nil || primaryID == 0 {
		return
	}
	MarkExpiredIfNeeded(ctx, primaryID, "restaurant_admin")
}

// BranchAdminWithLang is an admin user ID and the language they receive order cards in.
type BranchAdminWithLang struct {
	AdminUserID int64
	OrderLang   string // "uz" or "ru"
}

// GetBranchAdminsWithLang returns all admins for a branch with their order_lang: primary from branch_admins plus anyone in branch_admin_access for this branch (multi-user same password). Access users get primary's order_lang.
func GetBranchAdminsWithLang(ctx context.Context, branchLocationID int64) ([]BranchAdminWithLang, error) {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return nil, err
	}
	var primaryLang string
	err := db.Pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(TRIM(order_lang), ''), 'uz') FROM branch_admins WHERE branch_location_id = $1`, branchLocationID).Scan(&primaryLang)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("failed to get primary lang: %w", err)
	}
	if primaryLang != "ru" {
		primaryLang = "uz"
	}
	seen := make(map[int64]bool)
	var out []BranchAdminWithLang
	rows, err := db.Pool.Query(ctx, `
		SELECT admin_user_id FROM branch_admins WHERE branch_location_id = $1`,
		branchLocationID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get branch admins: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, BranchAdminWithLang{AdminUserID: id, OrderLang: primaryLang})
		}
	}
	rows.Close()
	rows2, err := db.Pool.Query(ctx, `
		SELECT tg_user_id FROM branch_admin_access WHERE branch_location_id = $1`,
		branchLocationID,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get branch admin access: %w", err)
	}
	for rows2.Next() {
		var id int64
		if err := rows2.Scan(&id); err != nil {
			rows2.Close()
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, BranchAdminWithLang{AdminUserID: id, OrderLang: primaryLang})
		}
	}
	rows2.Close()
	return out, rows2.Err()
}

// GetAdminOrderLang returns the order card language for an admin user ("uz" or "ru"). Checks branch_admins first, then branch_admin_access (uses branch's primary lang). Returns "uz" if not found.
func GetAdminOrderLang(ctx context.Context, adminUserID int64) (string, error) {
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return "uz", err
	}
	var orderLang string
	err := db.Pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(TRIM(order_lang), ''), 'uz') FROM branch_admins WHERE admin_user_id = $1 LIMIT 1`, adminUserID).Scan(&orderLang)
	if err == nil {
		if orderLang != "ru" {
			orderLang = "uz"
		}
		return orderLang, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "uz", err
	}
	// Access-only user: get branch and use that branch's order_lang
	err = db.Pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(TRIM(ba.order_lang), ''), 'uz') FROM branch_admin_access a JOIN branch_admins ba ON ba.branch_location_id = a.branch_location_id WHERE a.tg_user_id = $1 LIMIT 1`, adminUserID).Scan(&orderLang)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "uz", nil
		}
		return "uz", err
	}
	if orderLang != "ru" {
		orderLang = "uz"
	}
	return orderLang, nil
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
