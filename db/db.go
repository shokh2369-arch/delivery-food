package db

import (
	"context"
	"fmt"

	"food-telegram/config"

	"github.com/jackc/pgx/v5/pgxpool"
)

var Pool *pgxpool.Pool

func Init(cfg config.DBConfig) error {
	connStr := fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, cfg.Database,
	)
	var err error
	Pool, err = pgxpool.New(context.Background(), connStr)
	return err
}

func Close() {
	if Pool != nil {
		Pool.Close()
	}
}
