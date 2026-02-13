package services

import (
	"context"
	"fmt"

	"food-telegram/db"
	"food-telegram/models"
)

// SetUserLocation stores the selected location for a user.
func SetUserLocation(ctx context.Context, userID int64, locationID int64) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO user_locations (user_id, location_id, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id) DO UPDATE SET
			location_id = EXCLUDED.location_id,
			updated_at = now()`,
		userID, locationID,
	)
	return err
}

// GetUserLocation returns the selected location for a user (if any).
func GetUserLocation(ctx context.Context, userID int64) (*models.Location, error) {
	var locationID int64
	err := db.Pool.QueryRow(ctx, `
		SELECT location_id FROM user_locations WHERE user_id = $1`,
		userID,
	).Scan(&locationID)
	if err != nil {
		return nil, err
	}

	var l models.Location
	err = db.Pool.QueryRow(ctx, `
		SELECT id, name, lat, lon
		FROM locations
		WHERE id = $1`,
		locationID,
	).Scan(&l.ID, &l.Name, &l.Lat, &l.Lon)
	if err != nil {
		return nil, fmt.Errorf("failed to load location: %w", err)
	}
	return &l, nil
}

