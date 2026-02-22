package services

import (
	"context"
	"fmt"
	"time"

	"food-telegram/db"
	"golang.org/x/crypto/bcrypt"
)

const (
	SubscriptionStatusActive  = "active"
	SubscriptionStatusExpired  = "expired"
	SubscriptionStatusPaused  = "paused"
	SubscriptionDenyMessage   = "❌ Abonement tugagan. To'lovdan keyin superadmin yangilaydi."
	SubscriptionWarningMessage = "⏳ Abonement tugashiga 3 kun qoldi."
)

// Subscription represents one row of subscriptions table.
type Subscription struct {
	ID              string
	TgUserID        int64
	Role            string
	Status          string
	StartAt         time.Time
	ExpiresAt       time.Time
	LastPaymentAt  *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// CreateSubscription creates a new subscription (on first approval). start_at=now(), expires_at=now()+days.
func CreateSubscription(ctx context.Context, tgUserID int64, role string, days int) error {
	if days <= 0 {
		days = 1
	}
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO subscriptions (tg_user_id, role, status, start_at, expires_at, updated_at)
		VALUES ($1, $2, 'active', now(), now() + ($3 * interval '1 day'), now())
		ON CONFLICT (tg_user_id, role) DO UPDATE SET
			status = 'active',
			expires_at = now() + ($3 * interval '1 day'),
			last_payment_at = now(),
			updated_at = now()`,
		tgUserID, role, days,
	)
	if err != nil {
		return err
	}
	return syncCredentialActiveFromSubscription(ctx, tgUserID, role)
}

// GetSubscription returns the subscription for the user and role, if any.
func GetSubscription(ctx context.Context, tgUserID int64, role string) (*Subscription, error) {
	var s Subscription
	err := db.Pool.QueryRow(ctx, `
		SELECT id::text, tg_user_id, role, status, start_at, expires_at, last_payment_at, created_at, updated_at
		FROM subscriptions WHERE tg_user_id = $1 AND role = $2`,
		tgUserID, role,
	).Scan(&s.ID, &s.TgUserID, &s.Role, &s.Status, &s.StartAt, &s.ExpiresAt, &s.LastPaymentAt, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// IsEffectiveExpired returns true if the subscription should be treated as expired (past expires_at or status expired/paused).
func (s *Subscription) IsEffectiveExpired() bool {
	if s.Status == SubscriptionStatusExpired || s.Status == SubscriptionStatusPaused {
		return true
	}
	return time.Now().After(s.ExpiresAt)
}

// ExpiresWithinDays returns true if expires_at is within the given days (and still active).
func (s *Subscription) ExpiresWithinDays(days int) bool {
	if s.IsEffectiveExpired() {
		return false
	}
	deadline := time.Now().AddDate(0, 0, days)
	return s.ExpiresAt.Before(deadline) || s.ExpiresAt.Equal(deadline)
}

// syncCredentialActiveFromSubscription sets user_credentials.is_active from subscription (active and not expired).
func syncCredentialActiveFromSubscription(ctx context.Context, tgUserID int64, role string) error {
	sub, err := GetSubscription(ctx, tgUserID, role)
	if err != nil {
		_, _ = db.Pool.Exec(ctx, `UPDATE user_credentials SET is_active = false, updated_at = now() WHERE tg_user_id = $1 AND role = $2`, tgUserID, role)
		return nil
	}
	active := !sub.IsEffectiveExpired()
	_, err = db.Pool.Exec(ctx, `UPDATE user_credentials SET is_active = $1, updated_at = now() WHERE tg_user_id = $2 AND role = $3`, active, tgUserID, role)
	return err
}

// MarkExpiredIfNeeded updates subscription status to expired when past expires_at and syncs credential.
func MarkExpiredIfNeeded(ctx context.Context, tgUserID int64, role string) {
	_, _ = db.Pool.Exec(ctx, `
		UPDATE subscriptions SET status = 'expired', updated_at = now()
		WHERE tg_user_id = $1 AND role = $2 AND status = 'active' AND expires_at < now()`,
		tgUserID, role,
	)
	_ = syncCredentialActiveFromSubscription(ctx, tgUserID, role)
}

// RequireActiveSubscription returns true if the user has an active, non-expired subscription. Otherwise sets credential inactive and returns false with message.
func RequireActiveSubscription(ctx context.Context, tgUserID int64, role string) (ok bool, msg string) {
	sub, err := GetSubscription(ctx, tgUserID, role)
	if err != nil {
		_, _ = db.Pool.Exec(ctx, `UPDATE user_credentials SET is_active = false, updated_at = now() WHERE tg_user_id = $1 AND role = $2`, tgUserID, role)
		return false, SubscriptionDenyMessage
	}
	if sub.IsEffectiveExpired() {
		_, _ = db.Pool.Exec(ctx, `UPDATE subscriptions SET status = 'expired', updated_at = now() WHERE tg_user_id = $1 AND role = $2 AND status = 'active'`, tgUserID, role)
		_ = syncCredentialActiveFromSubscription(ctx, tgUserID, role)
		return false, SubscriptionDenyMessage
	}
	return true, ""
}

// SubscriptionExpiresWithinDays returns true and warning message if subscription expires within 3 days (for display).
func SubscriptionExpiresWithinDays(ctx context.Context, tgUserID int64, role string, days int) (within bool, warningMsg string) {
	sub, err := GetSubscription(ctx, tgUserID, role)
	if err != nil || sub.IsEffectiveExpired() {
		return false, ""
	}
	if sub.ExpiresWithinDays(days) {
		return true, SubscriptionWarningMessage
	}
	return false, ""
}

// ExpiredSubscriptionRow is used for /subs_pending list.
type ExpiredSubscriptionRow struct {
	TgUserID  int64
	Role      string
	ExpiresAt time.Time
	FullName  string
}

// ListExpiredSubscriptions returns subscriptions that are expired or past expires_at (for superadmin).
func ListExpiredSubscriptions(ctx context.Context, limit int) ([]ExpiredSubscriptionRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT s.tg_user_id, s.role, s.expires_at
		FROM subscriptions s
		WHERE s.status IN ('active', 'expired') AND s.expires_at < now()
		ORDER BY s.expires_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []ExpiredSubscriptionRow
	for rows.Next() {
		var r ExpiredSubscriptionRow
		if err := rows.Scan(&r.TgUserID, &r.Role, &r.ExpiresAt); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	return list, rows.Err()
}

// RenewSubscription extends expiry by days, sets status=active; for restaurant_admin does NOT rotate password (only reactivates). For driver, rotates password and returns new plain password.
func RenewSubscription(ctx context.Context, tgUserID int64, role string, days int, approvedBy int64, amount *float64, note string) (newPlainPassword string, err error) {
	if days <= 0 {
		days = 1
	}
	var hash string
	if role != UserRoleRestaurantAdmin {
		newPlainPassword, err = GenerateSecurePassword()
		if err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		var h []byte
		h, err = bcrypt.GenerateFromPassword([]byte(newPlainPassword), bcrypt.DefaultCost)
		if err != nil {
			return "", fmt.Errorf("hash password: %w", err)
		}
		hash = string(h)
	}

	_, err = db.Pool.Exec(ctx, `
		INSERT INTO subscriptions (tg_user_id, role, status, start_at, expires_at, last_payment_at, updated_at)
		VALUES ($1, $2, 'active', now(), now() + ($3 * interval '1 day'), now(), now())
		ON CONFLICT (tg_user_id, role) DO UPDATE SET
			status = 'active',
			expires_at = GREATEST(subscriptions.expires_at, now()) + ($3 * interval '1 day'),
			last_payment_at = now(),
			updated_at = now()`,
		tgUserID, role, days,
	)
	if err != nil {
		return "", err
	}

	if role == "restaurant_admin" {
		// Reactivate only; do not change password (lives in branch_admins).
		_, err = db.Pool.Exec(ctx, `UPDATE user_credentials SET is_active = true, updated_at = now() WHERE tg_user_id = $1 AND role = $2`, tgUserID, role)
	} else {
		_, err = db.Pool.Exec(ctx, `UPDATE user_credentials SET password_hash = $1, is_active = true, updated_at = now() WHERE tg_user_id = $2 AND role = $3`, hash, tgUserID, role)
	}
	if err != nil {
		return "", fmt.Errorf("update credential: %w", err)
	}

	if amount != nil || note != "" {
		amt := 0.0
		if amount != nil {
			amt = *amount
		}
		_ = RecordPaymentReceipt(ctx, tgUserID, role, amt, approvedBy, note)
	}

	return newPlainPassword, nil
}

// RecordPaymentReceipt inserts a payment_receipts row.
func RecordPaymentReceipt(ctx context.Context, tgUserID int64, role string, amount float64, approvedBy int64, note string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO payment_receipts (tg_user_id, role, amount, currency, paid_at, note, approved_by, created_at)
		VALUES ($1, $2, $3, 'UZS', now(), NULLIF($4, ''), $5, now())`,
		tgUserID, role, amount, note, approvedBy,
	)
	return err
}

// PauseSubscription sets status=paused and credential is_active=false.
func PauseSubscription(ctx context.Context, tgUserID int64, role string) error {
	_, err := db.Pool.Exec(ctx, `UPDATE subscriptions SET status = 'paused', updated_at = now() WHERE tg_user_id = $1 AND role = $2`, tgUserID, role)
	if err != nil {
		return err
	}
	_, err = db.Pool.Exec(ctx, `UPDATE user_credentials SET is_active = false, updated_at = now() WHERE tg_user_id = $1 AND role = $2`, tgUserID, role)
	return err
}

// UnpauseSubscription sets status=active and is_active=true only if expires_at > now().
func UnpauseSubscription(ctx context.Context, tgUserID int64, role string) error {
	res, err := db.Pool.Exec(ctx, `
		UPDATE subscriptions SET status = 'active', updated_at = now() WHERE tg_user_id = $1 AND role = $2 AND expires_at > now()`,
		tgUserID, role,
	)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("abonement allaqachon tugagan; yangilash uchun /renew ishlating")
	}
	_, err = db.Pool.Exec(ctx, `UPDATE user_credentials SET is_active = true, updated_at = now() WHERE tg_user_id = $1 AND role = $2`, tgUserID, role)
	return err
}

// GetChatIDForSubscriber returns the most recent application chat_id for the user/role (for sending renewal password).
func GetChatIDForSubscriber(ctx context.Context, tgUserID int64, role string) (chatID int64, err error) {
	err = db.Pool.QueryRow(ctx, `SELECT chat_id FROM applications WHERE tg_user_id = $1 AND type = $2 ORDER BY created_at DESC LIMIT 1`, tgUserID, role).Scan(&chatID)
	return chatID, err
}

// ResetBranchAdminPassword generates a new 8-char password, updates branch_admins.password_hash for the branch, and returns the plain password and the primary admin's tg_user_id (for sending via Zayafka).
func ResetBranchAdminPassword(ctx context.Context, branchLocationID int64) (newPlainPassword string, primaryTgUserID int64, err error) {
	err = db.Pool.QueryRow(ctx, `SELECT admin_user_id FROM branch_admins WHERE branch_location_id = $1`, branchLocationID).Scan(&primaryTgUserID)
	if err != nil {
		return "", 0, fmt.Errorf("branch not found: %w", err)
	}
	newPlainPassword, err = GenerateSecurePassword()
	if err != nil {
		return "", 0, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPlainPassword), bcrypt.DefaultCost)
	if err != nil {
		return "", 0, err
	}
	_, err = db.Pool.Exec(ctx, `UPDATE branch_admins SET password_hash = $1 WHERE branch_location_id = $2`, string(hash), branchLocationID)
	if err != nil {
		return "", 0, err
	}
	return newPlainPassword, primaryTgUserID, nil
}
