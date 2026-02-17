package services

import (
	"context"
	"fmt"

	"food-telegram/db"
)

// CreateLocationWithAdmin atomically creates a new location and assigns a branch admin.
// If assigning the admin fails, the location will NOT be created.
func CreateLocationWithAdmin(ctx context.Context, name string, lat, lon float64, adminUserID int64, promotedBy int64) (int64, error) {
	if adminUserID <= 0 {
		return 0, fmt.Errorf("adminUserID must be positive")
	}
	if promotedBy <= 0 {
		return 0, fmt.Errorf("promotedBy must be positive")
	}

	// Safety net: ensure table exists before trying to insert.
	if err := EnsureBranchAdminsTable(ctx); err != nil {
		return 0, err
	}

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locationID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO locations (name, lat, lon)
		VALUES ($1, $2, $3)
		RETURNING id`,
		name, lat, lon,
	).Scan(&locationID)
	if err != nil {
		return 0, fmt.Errorf("insert location: %w", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO branch_admins (branch_location_id, branch_name, admin_user_id, promoted_by)
		VALUES ($1, $2, $3, $4)`,
		locationID, name, adminUserID, promotedBy,
	)
	if err != nil {
		return 0, fmt.Errorf("insert branch admin: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit tx: %w", err)
	}

	return locationID, nil
}
