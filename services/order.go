package services

import (
	"context"
	"errors"
	"fmt"
	"math"

	"food-telegram/db"
	"food-telegram/models"
	"github.com/jackc/pgx/v5"
)

const (
	defaultBaseFee   = 5000 // start price (sum)
	defaultRatePerKm = 4000 // sum per km
)

// Order status enum: new (waiting) -> preparing -> ready -> ... -> completed; or new -> rejected
const (
	OrderStatusNew        = "new"
	OrderStatusRejected   = "rejected"
	OrderStatusPreparing  = "preparing"
	OrderStatusReady      = "ready"
	OrderStatusAssigned   = "assigned"
	OrderStatusPickedUp   = "picked_up"
	OrderStatusDelivering = "delivering"
	OrderStatusCompleted  = "completed"
)

// CalcDeliveryFee returns taxi-style fee: baseFee + (distance_km * ratePerKm).
func CalcDeliveryFee(distanceKm float64, baseFee, ratePerKm int64) int64 {
	if baseFee < 0 {
		baseFee = defaultBaseFee
	}
	if ratePerKm <= 0 {
		ratePerKm = defaultRatePerKm
	}
	rounded := math.Ceil(distanceKm*10) / 10
	perKmPart := int64(math.Round(rounded * float64(ratePerKm)))
	return baseFee + perKmPart
}

// ApplyDeliveryFeeRule rounds the calculated delivery fee to the nearest 1000 sum (e.g. 2400 -> 2000, 2600 -> 3000).
func ApplyDeliveryFeeRule(calculatedFee int64) int64 {
	if calculatedFee <= 0 {
		return 0
	}
	return (calculatedFee + 500) / 1000 * 1000
}

// FormatDeliveryFeeBreakdown returns a taxi-style breakdown string (e.g. "Boshlang'ich: 5 000 so'm\nMasofa: 2.5 km × 2 000 = 5 000 so'm\nYetkazib berish: 10 000 so'm").
func FormatDeliveryFeeBreakdown(distanceKm float64, baseFee, ratePerKm, totalDeliveryFee int64) string {
	if ratePerKm <= 0 {
		ratePerKm = defaultRatePerKm
	}
	if baseFee < 0 {
		baseFee = defaultBaseFee
	}
	rounded := math.Ceil(distanceKm*10) / 10
	perKmPart := int64(math.Round(rounded * float64(ratePerKm)))
	return fmt.Sprintf("Boshlang'ich: %d so'm\nMasofa: %.1f km × %d = %d so'm\nYetkazib berish: %d so'm",
		baseFee, rounded, ratePerKm, perKmPart, totalDeliveryFee)
}

func HaversineDistanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	const R = 6371
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return math.Round(R*c*100) / 100
}

func CreateOrder(ctx context.Context, input models.CreateOrderInput) (int64, error) {
	deliveryType := input.DeliveryType
	if deliveryType != "delivery" && deliveryType != "pickup" {
		deliveryType = "pickup"
	}
	deliveryFee := input.DeliveryFee
	if deliveryType == "pickup" {
		deliveryFee = 0
	}
	grandTotal := input.ItemsTotal + deliveryFee
	var id int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO orders (
			user_id, chat_id, phone, lat, lon, distance_km, rate_per_km,
			delivery_fee, items_total, grand_total, status, location_id, delivery_type
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id`,
		input.UserID, input.ChatID, input.Phone, input.Lat, input.Lon, input.DistanceKm,
		4000, deliveryFee, input.ItemsTotal, grandTotal, OrderStatusNew, input.LocationID, deliveryType,
	).Scan(&id)
	return id, err
}

// GetOrder loads an order by ID. Returns nil if not found.
func GetOrder(ctx context.Context, orderID int64) (*models.Order, error) {
	var o models.Order
	var deliveryType *string
	var driverID *string
	err := db.Pool.QueryRow(ctx, `
		SELECT id, COALESCE(location_id, 0), status, chat_id, items_total, grand_total,
		       COALESCE(delivery_fee, 0), COALESCE(distance_km, 0), delivery_type, driver_id
		FROM orders WHERE id = $1`,
		orderID,
	).Scan(&o.ID, &o.LocationID, &o.Status, &o.ChatID, &o.ItemsTotal, &o.GrandTotal, &o.DeliveryFee, &o.DistanceKm, &deliveryType, &driverID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	o.DeliveryType = deliveryType
	o.DriverID = driverID
	return &o, nil
}

// GetOrderCoordinates returns the delivery coordinates (lat, lon) for an order.
func GetOrderCoordinates(ctx context.Context, orderID int64) (lat, lon float64, err error) {
	err = db.Pool.QueryRow(ctx, `SELECT lat, lon FROM orders WHERE id = $1`, orderID).Scan(&lat, &lon)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	return lat, lon, nil
}

// CustomerOrderRow is a summary row for /orders list.
type CustomerOrderRow struct {
	ID         int64
	Status     string
	GrandTotal int64
	CreatedAt  string
}

// ListOrdersByUserID returns recent orders for the customer (user_id), newest first, limit 20.
func ListOrdersByUserID(ctx context.Context, userID int64, limit int) ([]CustomerOrderRow, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT id, status, grand_total, created_at::text
		FROM orders WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`,
		userID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []CustomerOrderRow
	for rows.Next() {
		var r CustomerOrderRow
		if err := rows.Scan(&r.ID, &r.Status, &r.GrandTotal, &r.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// TrySetOrderPushedAt sets orders.pushed_at = NOW() only if pushed_at IS NULL (one-shot claim for driver push).
// Returns true if we claimed (row updated), false if already set.
func TrySetOrderPushedAt(ctx context.Context, orderID int64) (claimed bool, err error) {
	var id int64
	err = db.Pool.QueryRow(ctx, `UPDATE orders SET pushed_at = now() WHERE id = $1 AND pushed_at IS NULL RETURNING id`, orderID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// OrderPushedWithinSeconds returns true if order has pushed_at set and within the last sec seconds.
func OrderPushedWithinSeconds(ctx context.Context, orderID int64, sec int) (bool, error) {
	if sec <= 0 {
		sec = 60
	}
	var within bool
	err := db.Pool.QueryRow(ctx, `SELECT pushed_at IS NOT NULL AND pushed_at >= now() - ($1::text || ' seconds')::interval FROM orders WHERE id = $2`, sec, orderID).Scan(&within)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return within, nil
}

// OrderAvailableForPush returns true if order is still status='ready' and driver_id IS NULL (not yet accepted).
func OrderAvailableForPush(ctx context.Context, orderID int64) (bool, error) {
	var status string
	var driverID *string
	err := db.Pool.QueryRow(ctx, `SELECT status, driver_id FROM orders WHERE id = $1`, orderID).Scan(&status, &driverID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return status == OrderStatusReady && driverID == nil, nil
}

// validStatusTransition defines allowed order status changes (no skip).
var validStatusTransition = map[string]string{
	OrderStatusNew:        OrderStatusPreparing,
	OrderStatusPreparing:  OrderStatusReady,
	OrderStatusReady:      OrderStatusAssigned,   // Driver accepts -> assigned
	OrderStatusAssigned:   OrderStatusPickedUp,   // Driver collects -> picked_up
	OrderStatusPickedUp:   OrderStatusDelivering, // Driver starts delivering -> delivering
	OrderStatusDelivering: OrderStatusCompleted,  // Driver completes -> completed
}

// ValidStatusTransition returns true if transitioning from 'from' to 'to' is allowed.
func ValidStatusTransition(from, to string) bool {
	// ready -> completed is only allowed in UpdateOrderStatus for pickup (validated there)
	if from == OrderStatusReady && to == OrderStatusCompleted {
		return false
	}
	// Driver can mark assigned -> completed (or via picked_up -> delivering -> completed)
	if from == OrderStatusAssigned && to == OrderStatusCompleted {
		return true
	}
	// Admin can reject a new order
	if from == OrderStatusNew && to == OrderStatusRejected {
		return true
	}
	next, ok := validStatusTransition[from]
	return ok && next == to
}

// UpdateOrderStatus updates order status in a transaction and records history. Validates transition and that order belongs to admin's restaurant.
// actorID is the Telegram user ID of the admin who performed the change.
func UpdateOrderStatus(ctx context.Context, orderID int64, newStatus string, adminLocationID int64, actorID int64) error {
	o, err := GetOrder(ctx, orderID)
	if err != nil {
		return err
	}
	if o == nil {
		return fmt.Errorf("order not found")
	}
	if o.LocationID != adminLocationID {
		return fmt.Errorf("order does not belong to your restaurant")
	}
	// Prevent admin from completing if driver is assigned
	if newStatus == OrderStatusCompleted {
		var driverID *string
		var deliveryType *string
		err := db.Pool.QueryRow(ctx, `SELECT driver_id, delivery_type FROM orders WHERE id = $1`, orderID).Scan(&driverID, &deliveryType)
		if err == nil && driverID != nil {
			return fmt.Errorf("bu buyurtma driverga biriktirilgan. Yakunlashni driver qiladi")
		}
		// For ready -> completed, only allow if delivery_type is 'pickup'
		if o.Status == OrderStatusReady {
			if deliveryType == nil || *deliveryType != "pickup" {
				return fmt.Errorf("bu buyurtma yetkazib berish uchun. Yakunlashni driver qiladi")
			}
		}
	}
	// Prevent admin from changing status once driver is assigned (except new->preparing->ready)
	var driverID *string
	err = db.Pool.QueryRow(ctx, `SELECT driver_id FROM orders WHERE id = $1`, orderID).Scan(&driverID)
	if err == nil && driverID != nil {
		// Admin can only change status if current status is new, preparing, or ready
		if o.Status != OrderStatusNew && o.Status != OrderStatusPreparing && o.Status != OrderStatusReady {
			return fmt.Errorf("bu buyurtma driverga biriktirilgan. Statusni driver o'zgartiradi")
		}
		// Prevent admin from setting completed if driver is assigned
		if newStatus == OrderStatusCompleted {
			return fmt.Errorf("bu buyurtma driverga biriktirilgan. Yakunlashni driver qiladi")
		}
	}
	// Allow ready -> completed only for pickup (already validated above when newStatus == completed)
	allowTransition := ValidStatusTransition(o.Status, newStatus) ||
		(o.Status == OrderStatusReady && newStatus == OrderStatusCompleted)
	if !allowTransition {
		return fmt.Errorf("invalid status transition from %q to %q", o.Status, newStatus)
	}
	fromStatus := o.Status
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `UPDATE orders SET status = $1, updated_at = now() WHERE id = $2`, newStatus, orderID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO order_status_history (order_id, from_status, to_status, actor_id)
		VALUES ($1, $2, $3, $4)`,
		orderID, fromStatus, newStatus, actorID,
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetDeliveryType sets the delivery type for an order (pickup or delivery).
func SetDeliveryType(ctx context.Context, orderID int64, deliveryType string, adminLocationID int64) error {
	if deliveryType != "pickup" && deliveryType != "delivery" {
		return fmt.Errorf("invalid delivery_type: %s", deliveryType)
	}
	o, err := GetOrder(ctx, orderID)
	if err != nil {
		return err
	}
	if o == nil {
		return fmt.Errorf("order not found")
	}
	if o.LocationID != adminLocationID {
		return fmt.Errorf("order does not belong to your restaurant")
	}
	if o.Status != OrderStatusReady {
		return fmt.Errorf("can only set delivery type when order is ready")
	}
	// For delivery orders, verify they have valid coordinates
	if deliveryType == "delivery" {
		var lat, lon float64
		err := db.Pool.QueryRow(ctx, `SELECT lat, lon FROM orders WHERE id = $1`, orderID).Scan(&lat, &lon)
		if err != nil {
			return fmt.Errorf("failed to get order coordinates: %v", err)
		}
		if lat == 0 && lon == 0 {
			return fmt.Errorf("delivery orders must have valid coordinates")
		}
	}
	_, err = db.Pool.Exec(ctx, `UPDATE orders SET delivery_type = $1 WHERE id = $2`, deliveryType, orderID)
	return err
}

// CustomerMessageForOrderStatus returns the Uzbek notification text for the customer (with order summary).
func CustomerMessageForOrderStatus(o *models.Order, newStatus string) string {
	summary := fmt.Sprintf("\n\nJami: %d UZS", o.GrandTotal)
	switch newStatus {
	case OrderStatusPreparing:
		return fmt.Sprintf("Sizning buyurtmangiz #%d tayyorlanmoqda. Tez orada yetkaziladi.%s\n\nKeyingi qadam: tayyor bo'lganda sizga xabar beramiz.", o.ID, summary)
	case OrderStatusReady:
		if o.DeliveryType != nil && *o.DeliveryType == "pickup" {
			return fmt.Sprintf("Sizning buyurtmangiz #%d tayyor va o'zingiz olib ketishingiz mumkin.%s", o.ID, summary)
		}
		return fmt.Sprintf("Sizning buyurtmangiz #%d hozir tayyor — yetkazib beruvchi olib ketishga tayyorlanmoqda.%s\n\nKeyingi qadam: yetkazib berish.", o.ID, summary)
	case OrderStatusAssigned:
		return fmt.Sprintf("🚚 Driver topildi, buyurtma yo'lga tayyorlanmoqda.%s", summary)
	case OrderStatusPickedUp:
		return fmt.Sprintf("📦 Buyurtmangiz restorandan olib ketildi.%s", summary)
	case OrderStatusDelivering:
		return fmt.Sprintf("🛵 Buyurtmangiz yo'lda.%s", summary)
	case OrderStatusCompleted:
		return fmt.Sprintf("✅ Buyurtmangiz yetkazildi. Rahmat!%s", summary)
	case OrderStatusRejected:
		return fmt.Sprintf("❌ Afsuski, buyurtmangiz #%d rad etildi. Savol bo'lsa, biz bilan bog'laning.", o.ID)
	default:
		return fmt.Sprintf("Buyurtma #%d — yangilandi: %s.%s", o.ID, newStatus, summary)
	}
}

func OverrideDeliveryFee(ctx context.Context, input models.OverrideDeliveryFeeInput) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE orders SET
			delivery_fee = $1,
			grand_total = items_total + $1,
			delivery_fee_overridden = true,
			delivery_fee_override_by = $2,
			delivery_fee_override_note = $3,
			delivery_fee_override_at = now(),
			updated_at = now()
		WHERE id = $4`,
		input.NewFee, input.OverrideBy, input.Note, input.OrderID,
	)
	return err
}

func GetDailyStats(ctx context.Context, date string) (*models.DailyStats, error) {
	var s models.DailyStats
	err := db.Pool.QueryRow(ctx, `
		SELECT
			COUNT(*)::int,
			COALESCE(SUM(items_total), 0)::bigint,
			COALESCE(SUM(delivery_fee), 0)::bigint,
			COALESCE(SUM(grand_total), 0)::bigint,
			COUNT(*) FILTER (WHERE delivery_fee_overridden)::int
		FROM orders
		WHERE created_at::date = $1::date`,
		date,
	).Scan(&s.OrdersCount, &s.ItemsRevenue, &s.DeliveryRevenue, &s.GrandRevenue, &s.OverridesCount)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SetAdminMessageID stores the admin chat ID and message ID for an order.
func SetAdminMessageID(ctx context.Context, orderID int64, adminChatID int64, adminMessageID int) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE orders SET admin_chat_id = $1, admin_message_id = $2 WHERE id = $3`,
		adminChatID, adminMessageID, orderID,
	)
	return err
}

// GetAdminMessageIDs retrieves admin chat ID and message ID for an order.
func GetAdminMessageIDs(ctx context.Context, orderID int64) (adminChatID *int64, adminMessageID *int, err error) {
	err = db.Pool.QueryRow(ctx, `
		SELECT admin_chat_id, admin_message_id FROM orders WHERE id = $1`,
		orderID,
	).Scan(&adminChatID, &adminMessageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	return adminChatID, adminMessageID, nil
}
