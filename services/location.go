package services

import (
	"context"
	"errors"
	"sort"

	"food-telegram/db"
	"food-telegram/models"

	"github.com/jackc/pgx/v5"
)

// AddLocation inserts a new fast food location (branch) with coordinates.
func AddLocation(ctx context.Context, name string, lat, lon float64) (int64, error) {
	var id int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO locations (name, lat, lon)
		VALUES ($1, $2, $3)
		RETURNING id`,
		name, lat, lon,
	).Scan(&id)
	return id, err
}

// GetLocationName returns the name of a location by ID, or empty string if not found.
func GetLocationName(ctx context.Context, id int64) (string, error) {
	var name string
	err := db.Pool.QueryRow(ctx, `SELECT name FROM locations WHERE id = $1`, id).Scan(&name)
	if err != nil {
		return "", err
	}
	return name, nil
}

// GetLocationByID returns a location by ID with coordinates, or nil if not found.
func GetLocationByID(ctx context.Context, id int64) (*models.Location, error) {
	var l models.Location
	err := db.Pool.QueryRow(ctx, `SELECT id, name, lat, lon FROM locations WHERE id = $1`, id).Scan(&l.ID, &l.Name, &l.Lat, &l.Lon)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// ListLocations returns all configured locations.
func ListLocations(ctx context.Context) ([]models.Location, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, name, lat, lon
		FROM locations
		ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []models.Location
	for rows.Next() {
		var l models.Location
		if err := rows.Scan(&l.ID, &l.Name, &l.Lat, &l.Lon); err != nil {
			return nil, err
		}
		res = append(res, l)
	}
	return res, rows.Err()
}

// ListLocationsForCustomer returns only locations that have at least one branch admin with an active (non-expired) subscription. Used by the TOKEN (customer) bot so expired branches are hidden until they subscribe again.
func ListLocationsForCustomer(ctx context.Context) ([]models.Location, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT DISTINCT l.id, l.name, l.lat, l.lon
		FROM locations l
		INNER JOIN branch_admins ba ON ba.branch_location_id = l.id
		INNER JOIN subscriptions s ON s.tg_user_id = ba.admin_user_id AND s.role = 'restaurant_admin'
		WHERE s.status = 'active' AND s.expires_at > now()
		ORDER BY l.id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []models.Location
	for rows.Next() {
		var l models.Location
		if err := rows.Scan(&l.ID, &l.Name, &l.Lat, &l.Lon); err != nil {
			return nil, err
		}
		res = append(res, l)
	}
	return res, rows.Err()
}

// LocationHasActiveSubscription returns true if the location has at least one branch admin with an active subscription.
func LocationHasActiveSubscription(ctx context.Context, locationID int64) (bool, error) {
	var n int
	err := db.Pool.QueryRow(ctx, `
		SELECT 1 FROM branch_admins ba
		INNER JOIN subscriptions s ON s.tg_user_id = ba.admin_user_id AND s.role = 'restaurant_admin'
		WHERE ba.branch_location_id = $1 AND s.status = 'active' AND s.expires_at > now()
		LIMIT 1`,
		locationID,
	).Scan(&n)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

type LocationWithDistance struct {
	Location models.Location
	Distance float64
}

// SortLocationsByDistance computes distance from user and returns sorted slice.
func SortLocationsByDistance(userLat, userLon float64, locs []models.Location) []LocationWithDistance {
	withDist := make([]LocationWithDistance, len(locs))
	for i, l := range locs {
		d := HaversineDistanceKm(userLat, userLon, l.Lat, l.Lon)
		withDist[i] = LocationWithDistance{
			Location: l,
			Distance: d,
		}
	}
	sort.Slice(withDist, func(i, j int) bool {
		return withDist[i].Distance < withDist[j].Distance
	})
	return withDist
}

// DeleteLocation removes a location and all related data. For each branch admin of this location it also removes their credential and subscription and marks their application rejected so they can request to add a branch again via Zayavka.
func DeleteLocation(ctx context.Context, id int64) error {
	// Get branch admin user IDs before we delete the location (branch_admins CASCADE when location is deleted)
	adminIDs, err := GetBranchAdmins(ctx, id)
	if err != nil {
		return err
	}
	for _, tgUserID := range adminIDs {
		_, _ = db.Pool.Exec(ctx, `DELETE FROM user_credentials WHERE tg_user_id = $1 AND role = 'restaurant_admin'`, tgUserID)
		_, _ = db.Pool.Exec(ctx, `DELETE FROM subscriptions WHERE tg_user_id = $1 AND role = 'restaurant_admin'`, tgUserID)
		_, _ = db.Pool.Exec(ctx, `UPDATE applications SET status = 'rejected', reject_reason = 'Filial o''chirilgan', updated_at = now() WHERE tg_user_id = $1 AND type = 'restaurant_admin'`, tgUserID)
	}
	// Delete menu items for this location
	if _, err := db.Pool.Exec(ctx, `DELETE FROM menu_items WHERE location_id = $1`, id); err != nil {
		return err
	}
	// Delete user-location mappings
	if _, err := db.Pool.Exec(ctx, `DELETE FROM user_locations WHERE location_id = $1`, id); err != nil {
		return err
	}
	// Finally delete the location itself (branch_admins CASCADE)
	_, err = db.Pool.Exec(ctx, `DELETE FROM locations WHERE id = $1`, id)
	return err
}


