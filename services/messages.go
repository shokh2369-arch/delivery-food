package services

import (
	"context"
	"encoding/json"
	"fmt"

	"food-telegram/db"
)

const outboundRole = "system/outbound"

// SaveOutboundMessage persists an outbound system message (e.g. order status notify).
func SaveOutboundMessage(ctx context.Context, chatID int64, content string, meta map[string]interface{}) error {
	metaJSON := "{}"
	if len(meta) > 0 {
		b, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("marshal meta: %w", err)
		}
		metaJSON = string(b)
	}
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO messages (chat_id, role, content, meta)
		VALUES ($1, $2, $3, $4::jsonb)`,
		chatID, outboundRole, content, metaJSON,
	)
	return err
}

// SentOrderStatusNotifyWithin30s returns true if the same order_id and status was already sent in the last 30 seconds (de-dup).
func SentOrderStatusNotifyWithin30s(ctx context.Context, orderID int64, status string) (bool, error) {
	var count int
	err := db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM messages
		WHERE role = $1 AND meta->>'sent_via' = 'order_status_notify'
		  AND (meta->>'order_id') = $2 AND meta->>'status' = $3
		  AND created_at > now() - interval '30 seconds'`,
		outboundRole, fmt.Sprintf("%d", orderID), status,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
