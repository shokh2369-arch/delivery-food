package services

import (
	"strings"
	"testing"

	"food-telegram/models"
)

func TestValidStatusTransition(t *testing.T) {
	tests := []struct {
		from, to string
		want     bool
	}{
		{OrderStatusNew, OrderStatusPreparing, true},
		{OrderStatusNew, OrderStatusReady, false},
		{OrderStatusNew, OrderStatusCompleted, false},
		{OrderStatusPreparing, OrderStatusReady, true},
		{OrderStatusPreparing, OrderStatusNew, false},
		{OrderStatusPreparing, OrderStatusCompleted, false},
		{OrderStatusReady, OrderStatusAssigned, true},
		{OrderStatusReady, OrderStatusCompleted, false},
		{OrderStatusAssigned, OrderStatusCompleted, true},
		{OrderStatusAssigned, OrderStatusReady, false},
		{OrderStatusReady, OrderStatusPreparing, false},
		{OrderStatusCompleted, OrderStatusNew, false},
		{"", OrderStatusNew, false},
		{OrderStatusNew, "", false},
	}
	for _, tt := range tests {
		got := ValidStatusTransition(tt.from, tt.to)
		if got != tt.want {
			t.Errorf("ValidStatusTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestCustomerMessageForOrderStatus(t *testing.T) {
	o := &models.Order{ID: 123, GrandTotal: 75000}
	m := CustomerMessageForOrderStatus(o, OrderStatusPreparing)
	if m == "" {
		t.Error("expected non-empty message for preparing")
	}
	if !strings.Contains(m, "123") || !strings.Contains(m, "75000") {
		t.Errorf("message should contain order id and total: %s", m)
	}
	m = CustomerMessageForOrderStatus(o, OrderStatusCompleted)
	if !strings.Contains(m, "yetkazildi") {
		t.Errorf("completed message should contain Uzbek 'yetkazildi': %s", m)
	}
}
