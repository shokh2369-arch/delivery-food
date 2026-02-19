package services

import (
	"context"
	"sort"

	"food-telegram/db"
	"food-telegram/models"
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

// DeleteLocation removes a location and its related data (menu items and user bindings).
func DeleteLocation(ctx context.Context, id int64) error {
	// Delete menu items for this location
	if _, err := db.Pool.Exec(ctx, `DELETE FROM menu_items WHERE location_id = $1`, id); err != nil {
		return err
	}
	// Delete user-location mappings
	if _, err := db.Pool.Exec(ctx, `DELETE FROM user_locations WHERE location_id = $1`, id); err != nil {
		return err
	}
	// Finally delete the location itself
	_, err := db.Pool.Exec(ctx, `DELETE FROM locations WHERE id = $1`, id)
	return err
}


