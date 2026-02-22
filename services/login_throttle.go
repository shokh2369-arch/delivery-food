package services

import (
	"context"
	"math"
	"time"

	"food-telegram/db"
)

const (
	ThrottleRoleSuperadmin       = "superadmin"
	ThrottleRoleRestaurantAdmin  = "restaurant_admin"
	ThrottleRoleDriver           = "driver"
	ThrottleCooldownCapSeconds   = 30
)

// LoginThrottleWaitSeconds returns how many seconds the user must wait before trying again (0 if no cooldown).
func LoginThrottleWaitSeconds(ctx context.Context, tgUserID int64, role string) (int, error) {
	var cooldownUntil *time.Time
	err := db.Pool.QueryRow(ctx, `
		SELECT cooldown_until FROM login_throttle WHERE tg_user_id = $1 AND role = $2`,
		tgUserID, role,
	).Scan(&cooldownUntil)
	if err != nil {
		return 0, nil // no row = no throttle
	}
	if cooldownUntil == nil {
		return 0, nil
	}
	until := *cooldownUntil
	if time.Now().Before(until) {
		return int(time.Until(until).Seconds()) + 1, nil // round up
	}
	return 0, nil
}

// RecordLoginFailed increments fail_count and sets cooldown_until = now() + min(30, 2^fail_count) seconds.
func RecordLoginFailed(ctx context.Context, tgUserID int64, role string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO login_throttle (tg_user_id, role, fail_count, last_failed_at, cooldown_until, updated_at)
		VALUES ($1, $2, 1, now(), now() + (LEAST(30, POWER(2, 1)::int) || ' seconds')::interval, now())
		ON CONFLICT (tg_user_id, role) DO UPDATE SET
			fail_count = login_throttle.fail_count + 1,
			last_failed_at = now(),
			cooldown_until = now() + (LEAST(30, POWER(2, login_throttle.fail_count + 1)::int) || ' seconds')::interval,
			updated_at = now()`,
		tgUserID, role,
	)
	return err
}

// RecordLoginSuccess resets fail_count and cooldown_until for the user/role.
func RecordLoginSuccess(ctx context.Context, tgUserID int64, role string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO login_throttle (tg_user_id, role, fail_count, last_failed_at, cooldown_until, updated_at)
		VALUES ($1, $2, 0, NULL, NULL, now())
		ON CONFLICT (tg_user_id, role) DO UPDATE SET
			fail_count = 0,
			last_failed_at = NULL,
			cooldown_until = NULL,
			updated_at = now()`,
		tgUserID, role,
	)
	return err
}

// CooldownSecondsForFailCount returns min(30, 2^failCount) for tests.
func CooldownSecondsForFailCount(failCount int) int {
	s := int(math.Pow(2, float64(failCount)))
	if s > ThrottleCooldownCapSeconds {
		return ThrottleCooldownCapSeconds
	}
	return s
}
