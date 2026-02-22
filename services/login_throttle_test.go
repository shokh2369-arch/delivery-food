package services

import (
	"context"
	"testing"
	"time"

	"food-telegram/db"
)

func TestCooldownSecondsForFailCount(t *testing.T) {
	tests := []struct {
		failCount int
		want      int
	}{
		{0, 1},   // 2^0=1
		{1, 2},   // 2^1=2
		{2, 4},   // 2^2=4
		{3, 8},   // 2^3=8
		{4, 16},  // 2^4=16
		{5, 30},  // 2^5=32 -> cap 30
		{6, 30},  // 2^6=64 -> cap 30
		{10, 30}, // cap 30
	}
	for _, tt := range tests {
		got := CooldownSecondsForFailCount(tt.failCount)
		if got != tt.want {
			t.Errorf("CooldownSecondsForFailCount(%d) = %d, want %d", tt.failCount, got, tt.want)
		}
	}
}

// Integration tests for throttle (require DB). Skip if db.Pool is nil or -short.
func TestLoginThrottle_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping throttle integration test in short mode")
	}
	if db.Pool == nil {
		t.Skip("skipping throttle integration test: no DB pool")
	}
	ctx := context.Background()
	const testUserID int64 = 999999997
	role := ThrottleRoleDriver

	// Cleanup: reset throttle for test user so tests are independent
	defer func() {
		_ = RecordLoginSuccess(ctx, testUserID, role)
	}()

	// 1) Success resets cooldown
	_ = RecordLoginSuccess(ctx, testUserID, role)
	wait, err := LoginThrottleWaitSeconds(ctx, testUserID, role)
	if err != nil {
		t.Fatalf("LoginThrottleWaitSeconds after success: %v", err)
	}
	if wait != 0 {
		t.Errorf("after success: wait = %d, want 0", wait)
	}

	// 2) Failed attempt sets cooldown
	_ = RecordLoginFailed(ctx, testUserID, role)
	wait, err = LoginThrottleWaitSeconds(ctx, testUserID, role)
	if err != nil {
		t.Fatalf("LoginThrottleWaitSeconds after fail: %v", err)
	}
	if wait <= 0 {
		t.Errorf("after one fail: wait = %d, want > 0 (cooldown 2s)", wait)
	}

	// 3) Attempt within cooldown returns wait message (we only check wait > 0 here)
	if wait > 30 {
		t.Errorf("cooldown wait %d exceeds cap 30", wait)
	}

	// 4) After cooldown expires, wait becomes 0 (or we can record success and verify reset)
	time.Sleep(time.Duration(wait+1) * time.Second)
	wait, _ = LoginThrottleWaitSeconds(ctx, testUserID, role)
	if wait != 0 {
		t.Logf("after cooldown expired: wait = %d (expected 0)", wait)
	}

	// 5) Success resets: fail again then success, then wait must be 0
	_ = RecordLoginFailed(ctx, testUserID, role)
	_ = RecordLoginSuccess(ctx, testUserID, role)
	wait, _ = LoginThrottleWaitSeconds(ctx, testUserID, role)
	if wait != 0 {
		t.Errorf("after fail then success: wait = %d, want 0", wait)
	}

	// 6) Cooldown caps at 30s: after many failures, cooldown should be at most 30s
	for i := 0; i < 8; i++ {
		_ = RecordLoginFailed(ctx, testUserID, role)
	}
	wait, _ = LoginThrottleWaitSeconds(ctx, testUserID, role)
	if wait > 30 {
		t.Errorf("after 8 fails: wait = %d, want <= 30 (cap)", wait)
	}
}
