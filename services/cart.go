package services

import (
	"context"
	"encoding/json"
	"fmt"

	"food-telegram/db"
)

type CartItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Price    int64  `json:"price"`
	Qty      int    `json:"qty"`
	Category string `json:"category"`
}

type Cart struct {
	Items      []CartItem `json:"items"`
	ItemsTotal int64      `json:"items_total"`
}

func GetCart(ctx context.Context, userID int64) (*Cart, error) {
	var itemsJSON []byte
	var itemsTotal int64
	err := db.Pool.QueryRow(ctx, `
		SELECT items, items_total FROM carts WHERE user_id = $1`,
		userID,
	).Scan(&itemsJSON, &itemsTotal)
	if err != nil {
		// Cart doesn't exist, return empty cart
		return &Cart{Items: []CartItem{}, ItemsTotal: 0}, nil
	}

	var items []CartItem
	if len(itemsJSON) > 0 {
		if err := json.Unmarshal(itemsJSON, &items); err != nil {
			return nil, fmt.Errorf("failed to unmarshal cart items: %w", err)
		}
	}
	return &Cart{Items: items, ItemsTotal: itemsTotal}, nil
}

func SaveCart(ctx context.Context, userID int64, cart *Cart) error {
	itemsJSON, err := json.Marshal(cart.Items)
	if err != nil {
		return fmt.Errorf("failed to marshal cart items: %w", err)
	}

	_, err = db.Pool.Exec(ctx, `
		INSERT INTO carts (user_id, items, items_total, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (user_id) DO UPDATE SET
			items = $2,
			items_total = $3,
			updated_at = now()`,
		userID, itemsJSON, cart.ItemsTotal,
	)
	return err
}

func DeleteCart(ctx context.Context, userID int64) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM carts WHERE user_id = $1`, userID)
	return err
}

type Checkout struct {
	CartItems  []CartItem `json:"cart_items"`
	ItemsTotal int64      `json:"items_total"`
	Phone      string     `json:"phone"`
}

func GetCheckout(ctx context.Context, userID int64) (*Checkout, error) {
	var itemsJSON []byte
	var itemsTotal int64
	var phone string
	err := db.Pool.QueryRow(ctx, `
		SELECT cart_items, items_total, phone FROM checkouts WHERE user_id = $1`,
		userID,
	).Scan(&itemsJSON, &itemsTotal, &phone)
	if err != nil {
		return nil, err // Checkout doesn't exist
	}

	var items []CartItem
	if len(itemsJSON) > 0 {
		if err := json.Unmarshal(itemsJSON, &items); err != nil {
			return nil, fmt.Errorf("failed to unmarshal checkout items: %w", err)
		}
	}
	return &Checkout{CartItems: items, ItemsTotal: itemsTotal, Phone: phone}, nil
}

func SaveCheckout(ctx context.Context, userID int64, checkout *Checkout) error {
	itemsJSON, err := json.Marshal(checkout.CartItems)
	if err != nil {
		return fmt.Errorf("failed to marshal checkout items: %w", err)
	}

	_, err = db.Pool.Exec(ctx, `
		INSERT INTO checkouts (user_id, cart_items, items_total, phone, created_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (user_id) DO UPDATE SET
			cart_items = $2,
			items_total = $3,
			phone = $4,
			created_at = now()`,
		userID, itemsJSON, checkout.ItemsTotal, checkout.Phone,
	)
	return err
}

func DeleteCheckout(ctx context.Context, userID int64) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM checkouts WHERE user_id = $1`, userID)
	return err
}
