package services

import (
	"context"
	"errors"
	"time"

	"food-telegram/db"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	UserRoleRestaurantAdmin = "restaurant_admin"
	UserRoleDriver          = "driver"
	LoginRateLimitCount     = 5
	LoginRateLimitWindow    = 10 * time.Minute
)

// HasApprovedCredential returns true if user has an active credential for the role (approved applicant).
func HasApprovedCredential(ctx context.Context, tgUserID int64, role string) (bool, error) {
	var active bool
	err := db.Pool.QueryRow(ctx, `
		SELECT is_active FROM user_credentials WHERE tg_user_id = $1 AND role = $2`,
		tgUserID, role,
	).Scan(&active)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return active, nil
}

// CredentialExists returns true if user has any credential row for the role (active or not).
func CredentialExists(ctx context.Context, tgUserID int64, role string) (bool, error) {
	var ok int
	err := db.Pool.QueryRow(ctx, `
		SELECT 1 FROM user_credentials WHERE tg_user_id = $1 AND role = $2`,
		tgUserID, role,
	).Scan(&ok)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// PasswordCorrectButInactive returns true if the user has a credential for the role, password matches, but is_active is false.
func PasswordCorrectButInactive(ctx context.Context, tgUserID int64, role string, plainPassword string) (bool, error) {
	var hash string
	var isActive bool
	err := db.Pool.QueryRow(ctx, `
		SELECT password_hash, is_active FROM user_credentials WHERE tg_user_id = $1 AND role = $2`,
		tgUserID, role,
	).Scan(&hash, &isActive)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if isActive {
		return false, nil
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plainPassword)) == nil, nil
}

// VerifyCredential checks password against user_credentials for the role; returns true if valid and active.
func VerifyCredential(ctx context.Context, tgUserID int64, role string, plainPassword string) (bool, error) {
	var hash string
	var isActive bool
	err := db.Pool.QueryRow(ctx, `
		SELECT password_hash, is_active FROM user_credentials WHERE tg_user_id = $1 AND role = $2`,
		tgUserID, role,
	).Scan(&hash, &isActive)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if !isActive {
		return false, nil
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plainPassword)) == nil, nil
}

// RecordLoginAttempt records a login attempt for rate limiting (do not log password).
func RecordLoginAttempt(ctx context.Context, tgUserID int64, success bool) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO login_attempts (tg_user_id, success) VALUES ($1, $2)`,
		tgUserID, success,
	)
	return err
}

// CountRecentFailedAttempts returns number of failed login attempts in the last 10 minutes.
func CountRecentFailedAttempts(ctx context.Context, tgUserID int64) (int, error) {
	var n int
	err := db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM login_attempts
		WHERE tg_user_id = $1 AND success = false AND attempted_at > now() - interval '10 minutes'`,
		tgUserID,
	).Scan(&n)
	return n, err
}

// CleanupOldLoginAttempts removes attempts older than 24h (optional, call periodically).
func CleanupOldLoginAttempts(ctx context.Context) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM login_attempts WHERE attempted_at < now() - interval '24 hours'`)
	return err
}
