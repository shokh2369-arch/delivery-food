package services

import (
	"context"
	"errors"

	"food-telegram/db"
	"github.com/jackc/pgx/v5"
)

// GetCustomerLanguage returns the stored language for the customer (uz or ru). Empty string and false if not set.
func GetCustomerLanguage(ctx context.Context, tgUserID int64) (language string, ok bool) {
	err := db.Pool.QueryRow(ctx, `SELECT language FROM customer_users WHERE tg_user_id = $1 AND language IS NOT NULL`, tgUserID).Scan(&language)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false
		}
		return "", false
	}
	return language, true
}

// SetCustomerLanguage sets (or updates) the customer's language and language_selected_at.
func SetCustomerLanguage(ctx context.Context, tgUserID int64, language string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO customer_users (tg_user_id, language, language_selected_at)
		VALUES ($1, $2, now())
		ON CONFLICT (tg_user_id) DO UPDATE SET language = EXCLUDED.language, language_selected_at = now()`,
		tgUserID, language,
	)
	return err
}
