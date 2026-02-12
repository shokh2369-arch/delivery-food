package services

import (
	"context"
	"math"

	"food-telegram/db"
	"food-telegram/models"
)

const defaultRatePerKm = 2000

func CalcDeliveryFee(distanceKm float64, ratePerKm int64) int64 {
	if ratePerKm == 0 {
		ratePerKm = defaultRatePerKm
	}
	rounded := math.Ceil(distanceKm*10) / 10
	return int64(math.Round(rounded * float64(ratePerKm)))
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
	grandTotal := input.ItemsTotal + input.DeliveryFee
	var id int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO orders (
			user_id, chat_id, phone, lat, lon, distance_km, rate_per_km,
			delivery_fee, items_total, grand_total, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'confirmed')
		RETURNING id`,
		input.UserID, input.ChatID, input.Phone, input.Lat, input.Lon, input.DistanceKm,
		2000, input.DeliveryFee, input.ItemsTotal, grandTotal,
	).Scan(&id)
	return id, err
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
