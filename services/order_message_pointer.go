package services

import (
	"context"
	"errors"
	"strings"

	"food-telegram/db"
	"github.com/jackc/pgx/v5"
)

// EnsureOrderMessagePointersTable creates order_message_pointers if missing (safety net when migrate was not run).
func EnsureOrderMessagePointersTable(ctx context.Context) error {
	_, err := db.Pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS order_message_pointers (
			order_id BIGINT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
			audience TEXT NOT NULL CHECK (audience IN ('admin','customer','driver')),
			chat_id BIGINT NOT NULL,
			message_id INT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (order_id, audience)
		);
		CREATE INDEX IF NOT EXISTS idx_order_message_pointers_order_id ON order_message_pointers(order_id);
	`)
	return err
}

func isRelationNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), "order_message_pointers") && strings.Contains(err.Error(), "does not exist")
}

// GetOrderMessagePointer returns the chat_id and message_id for the order's card for the given audience.
// ok is false if no pointer exists.
func GetOrderMessagePointer(ctx context.Context, orderID int64, audience string) (chatID int64, messageID int, ok bool, err error) {
	err = db.Pool.QueryRow(ctx, `
		SELECT chat_id, message_id FROM order_message_pointers WHERE order_id = $1 AND audience = $2`,
		orderID, audience,
	).Scan(&chatID, &messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, false, nil
		}
		if isRelationNotExist(err) {
			if ensureErr := EnsureOrderMessagePointersTable(ctx); ensureErr != nil {
				return 0, 0, false, ensureErr
			}
			return GetOrderMessagePointer(ctx, orderID, audience)
		}
		return 0, 0, false, err
	}
	return chatID, messageID, true, nil
}

// UpsertOrderMessagePointer inserts or updates the message pointer for (order_id, audience).
func UpsertOrderMessagePointer(ctx context.Context, orderID int64, audience string, chatID int64, messageID int) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO order_message_pointers (order_id, audience, chat_id, message_id, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (order_id, audience) DO UPDATE SET chat_id = EXCLUDED.chat_id, message_id = EXCLUDED.message_id, updated_at = now()`,
		orderID, audience, chatID, messageID,
	)
	if err != nil && isRelationNotExist(err) {
		if ensureErr := EnsureOrderMessagePointersTable(ctx); ensureErr != nil {
			return ensureErr
		}
		return UpsertOrderMessagePointer(ctx, orderID, audience, chatID, messageID)
	}
	return err
}
