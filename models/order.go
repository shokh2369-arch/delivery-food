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
