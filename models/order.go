package models

type CreateOrderInput struct {
	UserID      int64
	ChatID      string
	Phone       string
	Lat         float64
	Lon         float64
	DistanceKm  float64
	DeliveryFee int64
	ItemsTotal  int64
	LocationID  int64 // restaurant (branch) this order belongs to
}

// Order is a row from orders table (for status and location checks).
type Order struct {
	ID           int64
	LocationID   int64
	Status       string
	ChatID       string
	ItemsTotal   int64
	GrandTotal   int64
	DeliveryFee  int64   // taxi-style: base + per km
	DistanceKm   float64 // for breakdown display
	DeliveryType *string // 'pickup' or 'delivery', nil if not set
}

type OverrideDeliveryFeeInput struct {
	OrderID    int64
	NewFee     int64
	OverrideBy int64
	Note       string
}

type DailyStats struct {
	OrdersCount     int
	ItemsRevenue    int64
	DeliveryRevenue int64
	GrandRevenue    int64
	OverridesCount  int
}
