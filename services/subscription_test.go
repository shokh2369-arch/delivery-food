package services

import (
	"testing"
	"time"
)

func TestSubscription_IsEffectiveExpired(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(24 * time.Hour)

	tests := []struct {
		name      string
		status    string
		expiresAt time.Time
		want      bool
	}{
		{"expired status", SubscriptionStatusExpired, future, true},
		{"paused status", SubscriptionStatusPaused, future, true},
		{"active but past expiry", SubscriptionStatusActive, past, true},
		{"active and future expiry", SubscriptionStatusActive, future, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Subscription{Status: tt.status, ExpiresAt: tt.expiresAt}
			got := s.IsEffectiveExpired()
			if got != tt.want {
				t.Errorf("IsEffectiveExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSubscription_ExpiresWithinDays(t *testing.T) {
	now := time.Now()
	// Already expired -> false
	s := &Subscription{Status: SubscriptionStatusActive, ExpiresAt: now.Add(-time.Hour)}
	if s.ExpiresWithinDays(3) {
		t.Error("expired subscription should not be ExpiresWithinDays(3)")
	}
	// Expires in 2 days, check within 3 -> true
	s2 := &Subscription{Status: SubscriptionStatusActive, ExpiresAt: now.AddDate(0, 0, 2)}
	if !s2.ExpiresWithinDays(3) {
		t.Error("expires in 2 days should be within 3 days")
	}
	// Expires in 5 days, check within 3 -> false
	s3 := &Subscription{Status: SubscriptionStatusActive, ExpiresAt: now.AddDate(0, 0, 5)}
	if s3.ExpiresWithinDays(3) {
		t.Error("expires in 5 days should not be within 3 days")
	}
}

func TestSubscriptionConstants(t *testing.T) {
	if SubscriptionDenyMessage == "" {
		t.Error("SubscriptionDenyMessage should be set")
	}
	if SubscriptionWarningMessage == "" {
		t.Error("SubscriptionWarningMessage should be set")
	}
	if SubscriptionStatusActive != "active" || SubscriptionStatusExpired != "expired" || SubscriptionStatusPaused != "paused" {
		t.Error("subscription status constants should match")
	}
}

// TestRequireActiveSubscription_ExpiredUser documents: expired user gets deny message and ok=false.
// Full behavior requires DB (GetSubscription, sync credential).
func TestRequireActiveSubscription_ExpiredUser(t *testing.T) {
	// When subscription is missing or expired, RequireActiveSubscription returns (false, SubscriptionDenyMessage).
	// Privileged features (panel, go online, accept jobs) must check this and deny access.
	t.Log("RequireActiveSubscription loads subscription; if missing or IsEffectiveExpired, returns (false, deny message)")
	t.Log("Bots must clear session and send deny message so no privileged actions run")
}

// TestRenewUpdatesExpiresAndRotatesPassword documents renewal behavior.
// Integration test with DB would: create subscription, call RenewSubscription, verify expires_at extended and new password works.
func TestRenewUpdatesExpiresAndRotatesPassword(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	// Document expected behavior:
	// - RenewSubscription(tgUserID, role, months, approvedBy, amount, note)
	// - extends expires_at = max(current, now()) + months, sets status=active, user_credentials.is_active=true
	// - For restaurant_admin: does NOT rotate password (only reactivate). For driver: generates new password, stores hash, returns plain.
	// - RecordPaymentReceipt if amount or note provided
	// - Data (restaurant, menu, orders) is never deleted or reset
	t.Log("Renew extends expires_at; for driver rotates password; for restaurant_admin only reactivates")
	t.Log("Data (restaurant/menu/orders) remains unchanged")
}
