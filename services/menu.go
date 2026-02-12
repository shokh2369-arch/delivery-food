package services

import (
	"context"
	"fmt"
	"strconv"

	"food-telegram/db"
	"food-telegram/models"
)

func ListMenuByCategory(ctx context.Context, category string) ([]models.MenuItem, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, category, name, price FROM menu_items
		WHERE category = $1
		ORDER BY id`,
		category,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []models.MenuItem
	for rows.Next() {
		var id int64
		var cat, name string
		var price int64
		if err := rows.Scan(&id, &cat, &name, &price); err != nil {
			return nil, err
		}
		items = append(items, models.MenuItem{
			ID:       strconv.FormatInt(id, 10),
			Category: cat,
			Name:     name,
			Price:    price,
		})
	}
	return items, rows.Err()
}

func ListAllMenu(ctx context.Context) ([]models.MenuItem, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, category, name, price FROM menu_items
		ORDER BY category, id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []models.MenuItem
	for rows.Next() {
		var id int64
		var cat, name string
		var price int64
		if err := rows.Scan(&id, &cat, &name, &price); err != nil {
			return nil, err
		}
		items = append(items, models.MenuItem{
			ID:       strconv.FormatInt(id, 10),
			Category: cat,
			Name:     name,
			Price:    price,
		})
	}
	return items, rows.Err()
}

func AddMenuItem(ctx context.Context, category, name string, price int64) (int64, error) {
	if category != models.CategoryFood && category != models.CategoryDrink && category != models.CategoryDessert {
		return 0, fmt.Errorf("invalid category: %s", category)
	}
	if name == "" {
		return 0, fmt.Errorf("name is required")
	}
	if price < 0 {
		return 0, fmt.Errorf("price must be >= 0")
	}

	var id int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO menu_items (category, name, price) VALUES ($1, $2, $3)
		RETURNING id`,
		category, name, price,
	).Scan(&id)
	return id, err
}

func GetMenuItem(ctx context.Context, idStr string) (*models.MenuItem, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil, err
	}
	var category, name string
	var price int64
	err = db.Pool.QueryRow(ctx, `SELECT category, name, price FROM menu_items WHERE id = $1`, id).Scan(&category, &name, &price)
	if err != nil {
		return nil, err
	}
	return &models.MenuItem{ID: idStr, Category: category, Name: name, Price: price}, nil
}

func DeleteMenuItem(ctx context.Context, id int64) error {
	_, err := db.Pool.Exec(ctx, `DELETE FROM menu_items WHERE id = $1`, id)
	return err
}
