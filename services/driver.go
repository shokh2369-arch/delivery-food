package services

import (
	"context"
	"errors"
	"fmt"

	"food-telegram/db"
	"food-telegram/models"
	"github.com/jackc/pgx/v5"
)

const (
	DriverStatusOnline  = "online"
	DriverStatusOffline = "offline"
)

// Driver represents a delivery driver.
type Driver struct {
	ID       string
	TgUserID int64
	ChatID   int64
	Phone    string
	CarPlate string
	Status   string
}

// DriverLocation represents a driver's current location.
type DriverLocation struct {
	DriverID  string
	Lat       float64
	Lon       float64
	UpdatedAt string
}

// RegisterDriver creates or updates a driver record.
func RegisterDriver(ctx context.Context, tgUserID int64, chatID int64) (*Driver, error) {
	var id string
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO drivers (tg_user_id, chat_id, status)
		VALUES ($1, $2, $3)
		ON CONFLICT (tg_user_id) DO UPDATE SET chat_id = EXCLUDED.chat_id, updated_at = now()
		RETURNING id`,
		tgUserID, chatID, DriverStatusOffline,
	).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("register driver: %w", err)
	}
	return &Driver{ID: id, TgUserID: tgUserID, ChatID: chatID, Status: DriverStatusOffline}, nil
}

// GetDriverByTgUserID loads a driver by Telegram user ID.
func GetDriverByTgUserID(ctx context.Context, tgUserID int64) (*Driver, error) {
	var d Driver
	err := db.Pool.QueryRow(ctx, `
		SELECT id, tg_user_id, chat_id, COALESCE(phone, ''), COALESCE(car_plate, ''), status
		FROM drivers WHERE tg_user_id = $1`,
		tgUserID,
	).Scan(&d.ID, &d.TgUserID, &d.ChatID, &d.Phone, &d.CarPlate, &d.Status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

// GetDriverByID loads a driver by driver ID.
func GetDriverByID(ctx context.Context, driverID string) (*Driver, error) {
	var d Driver
	err := db.Pool.QueryRow(ctx, `
		SELECT id, tg_user_id, chat_id, COALESCE(phone, ''), COALESCE(car_plate, ''), status
		FROM drivers WHERE id = $1`,
		driverID,
	).Scan(&d.ID, &d.TgUserID, &d.ChatID, &d.Phone, &d.CarPlate, &d.Status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &d, nil
}

// UpdateDriverStatus updates driver status (online/offline).
func UpdateDriverStatus(ctx context.Context, driverID string, status string) error {
	if status != DriverStatusOnline && status != DriverStatusOffline {
		return fmt.Errorf("invalid driver status: %s", status)
	}
	_, err := db.Pool.Exec(ctx, `
		UPDATE drivers SET status = $1, updated_at = now() WHERE id = $2`,
		status, driverID,
	)
	return err
}

// UpdateDriverLocation updates or inserts driver location.
func UpdateDriverLocation(ctx context.Context, driverID string, lat, lon float64) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO driver_locations (driver_id, lat, lon, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (driver_id) DO UPDATE SET lat = EXCLUDED.lat, lon = EXCLUDED.lon, updated_at = now()`,
		driverID, lat, lon,
	)
	return err
}

// GetDriverLocation returns the driver's current location if recent (within 5 minutes).
func GetDriverLocation(ctx context.Context, driverID string) (*DriverLocation, error) {
	var loc DriverLocation
	err := db.Pool.QueryRow(ctx, `
		SELECT driver_id, lat, lon, updated_at::text
		FROM driver_locations
		WHERE driver_id = $1 AND updated_at > now() - interval '5 minutes'`,
		driverID,
	).Scan(&loc.DriverID, &loc.Lat, &loc.Lon, &loc.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &loc, nil
}

// GetDriverLocationAny returns the driver's location regardless of age (for debugging/immediate use).
func GetDriverLocationAny(ctx context.Context, driverID string) (*DriverLocation, error) {
	var loc DriverLocation
	err := db.Pool.QueryRow(ctx, `
		SELECT driver_id, lat, lon, updated_at::text
		FROM driver_locations
		WHERE driver_id = $1`,
		driverID,
	).Scan(&loc.DriverID, &loc.Lat, &loc.Lon, &loc.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &loc, nil
}

// ReadyOrderForDriver represents a ready order visible to drivers.
type ReadyOrderForDriver struct {
	ID         int64
	LocationID int64
	Lat        float64
	Lon        float64
	ItemsTotal int64
	GrandTotal int64
	DistanceKm float64
}

// CountReadyOrders returns count of READY orders (status='ready' AND driver_id IS NULL) for debugging.
func CountReadyOrders(ctx context.Context) (int, error) {
	var count int
	err := db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM orders WHERE status = $1 AND driver_id IS NULL AND lat IS NOT NULL AND lon IS NOT NULL`,
		OrderStatusReady,
	).Scan(&count)
	return count, err
}

// GetNearbyReadyOrders returns READY orders within radiusKm from driver location.
// Returns orders where:
// - status = 'ready' (exact string match)
// - driver_id IS NULL
// - lat IS NOT NULL AND lon IS NOT NULL
// - delivery_type = 'delivery' (only orders explicitly sent to delivery by admin)
// Distance calculated using Haversine formula (spherical law of cosines).
func GetNearbyReadyOrders(ctx context.Context, driverLat, driverLon float64, radiusKm float64, limit int) ([]ReadyOrderForDriver, error) {
	if limit <= 0 {
		limit = 10
	}
	// Haversine formula: distance = R * acos(cos(lat1) * cos(lat2) * cos(lon2 - lon1) + sin(lat1) * sin(lat2))
	// R = 6371 km (Earth radius)
	rows, err := db.Pool.Query(ctx, `
		SELECT id, COALESCE(location_id, 0), lat, lon, items_total, grand_total,
		       (6371 * acos(
		           cos(radians($1)) * cos(radians(lat)) *
		           cos(radians(lon) - radians($2)) +
		           sin(radians($1)) * sin(radians(lat))
		       )) AS distance_km
		FROM orders
		WHERE status = $3 
		  AND driver_id IS NULL 
		  AND lat IS NOT NULL 
		  AND lon IS NOT NULL
		  AND delivery_type = 'delivery'
		  AND (6371 * acos(
		      cos(radians($1)) * cos(radians(lat)) *
		      cos(radians(lon) - radians($2)) +
		      sin(radians($1)) * sin(radians(lat))
		  )) <= $4
		ORDER BY distance_km ASC
		LIMIT $5`,
		driverLat, driverLon, OrderStatusReady, radiusKm, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var orders []ReadyOrderForDriver
	for rows.Next() {
		var o ReadyOrderForDriver
		if err := rows.Scan(&o.ID, &o.LocationID, &o.Lat, &o.Lon, &o.ItemsTotal, &o.GrandTotal, &o.DistanceKm); err != nil {
			return nil, err
		}
		orders = append(orders, o)
	}
	return orders, rows.Err()
}

// AcceptOrder assigns a driver to a READY order and transitions status to 'assigned' (atomic, prevents double assign).
// Returns order details if successful, error if already assigned or invalid.
func AcceptOrder(ctx context.Context, orderID int64, driverID string, driverTgUserID int64) (*models.Order, error) {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var o models.Order
	err = tx.QueryRow(ctx, `
		UPDATE orders
		SET driver_id = $1, assigned_at = now(), status = $4, updated_at = now()
		WHERE id = $2 AND status = $3 AND driver_id IS NULL
		RETURNING id, COALESCE(location_id, 0), status, chat_id, items_total, grand_total`,
		driverID, orderID, OrderStatusReady, OrderStatusAssigned,
	).Scan(&o.ID, &o.LocationID, &o.Status, &o.ChatID, &o.ItemsTotal, &o.GrandTotal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Check if order exists but was already taken
			var existingDriverID *string
			checkErr := tx.QueryRow(ctx, `SELECT driver_id FROM orders WHERE id = $1`, orderID).Scan(&existingDriverID)
			if checkErr == nil && existingDriverID != nil {
				return nil, fmt.Errorf("bu buyurtma allaqachon olingan")
			}
			return nil, fmt.Errorf("buyurtma topilmadi yoki tayyor emas")
		}
		return nil, err
	}
	// Record status transition: ready -> assigned
	_, err = tx.Exec(ctx, `
		INSERT INTO order_status_history (order_id, from_status, to_status, actor_id)
		VALUES ($1, $2, $3, $4)`,
		orderID, OrderStatusReady, OrderStatusAssigned, driverTgUserID,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &o, nil
}

// GetDriverActiveOrder returns the active order assigned to a driver (status in assigned/picked_up/delivering).
func GetDriverActiveOrder(ctx context.Context, driverID string) (*models.Order, error) {
	var o models.Order
	err := db.Pool.QueryRow(ctx, `
		SELECT id, COALESCE(location_id, 0), status, chat_id, items_total, grand_total
		FROM orders
		WHERE driver_id = $1 AND status IN ($2, $3, $4)
		ORDER BY assigned_at DESC
		LIMIT 1`,
		driverID, OrderStatusAssigned, OrderStatusPickedUp, OrderStatusDelivering,
	).Scan(&o.ID, &o.LocationID, &o.Status, &o.ChatID, &o.ItemsTotal, &o.GrandTotal)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &o, nil
}

// UpdateDriverOrderStatus updates order status by the assigned driver (assigned -> picked_up -> delivering -> completed).
// Only the assigned driver can update status. Validates status transition.
func UpdateDriverOrderStatus(ctx context.Context, orderID int64, driverID string, driverTgUserID int64, newStatus string) error {
	// Validate new status is driver-controlled
	validDriverStatuses := map[string]bool{
		OrderStatusPickedUp:   true,
		OrderStatusDelivering: true,
		OrderStatusCompleted: true,
	}
	if !validDriverStatuses[newStatus] {
		return fmt.Errorf("invalid driver status: %s", newStatus)
	}

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Get current status and verify driver assignment
	var fromStatus string
	err = tx.QueryRow(ctx, `
		SELECT status FROM orders WHERE id = $1 AND driver_id = $2`,
		orderID, driverID,
	).Scan(&fromStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("order not found or not assigned to you")
		}
		return err
	}

	// Validate status transition
	if !ValidStatusTransition(fromStatus, newStatus) {
		return fmt.Errorf("invalid status transition from %q to %q", fromStatus, newStatus)
	}

	// Update status
	var rowsAffected int64
	err = tx.QueryRow(ctx, `
		UPDATE orders 
		SET status = $1, updated_at = now() 
		WHERE id = $2 AND driver_id = $3 AND status = $4
		RETURNING 1`,
		newStatus, orderID, driverID, fromStatus,
	).Scan(&rowsAffected)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("status mos emas yoki sizga tegishli emas")
		}
		return err
	}

	// Record status transition
	_, err = tx.Exec(ctx, `
		INSERT INTO order_status_history (order_id, from_status, to_status, actor_id)
		VALUES ($1, $2, $3, $4)`,
		orderID, fromStatus, newStatus, driverTgUserID,
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CompleteDeliveryByDriver marks an order as completed by the assigned driver (from delivering status).
func CompleteDeliveryByDriver(ctx context.Context, orderID int64, driverID string, driverTgUserID int64) error {
	return UpdateDriverOrderStatus(ctx, orderID, driverID, driverTgUserID, OrderStatusCompleted)
}
