package main

import (
	"context"
	"fmt"
	"os"

	"food-telegram/bot"
	"food-telegram/config"
	"food-telegram/db"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	if cfg.Telegram.Token == "" {
		fmt.Fprintln(os.Stderr, "TOKEN not set")
		os.Exit(1)
	}

	// Check for migrate subcommand
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		runMigrate(cfg)
		return
	}

	if err := db.Init(cfg.DB); err != nil {
		fmt.Fprintln(os.Stderr, "db:", err)
		os.Exit(1)
	}
	defer db.Close()

	adminID := int64(0)
	if v := os.Getenv("ADMIN_ID"); v != "" {
		fmt.Sscanf(v, "%d", &adminID)
	}

	b, err := bot.New(cfg, adminID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bot:", err)
		os.Exit(1)
	}

	// Start adder bot (ADDER_TOKEN + LOGIN) in background if configured
	if cfg.Telegram.AdderToken != "" {
		adder, err := bot.NewAdderBot(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "adder bot:", err)
			os.Exit(1)
		}
		go adder.Start()
		fmt.Println("Adder bot started (admin add menu).")
	}

	fmt.Println("Bot started.")
	b.Start()
}

func runMigrate(cfg *config.Config) {
	if err := db.Init(cfg.DB); err != nil {
		fmt.Fprintln(os.Stderr, "db:", err)
		os.Exit(1)
	}
	defer db.Close()

	for _, name := range []string{"migrations/001_orders_delivery.sql", "migrations/002_orders_phone.sql", "migrations/003_menu_items.sql", "migrations/004_carts.sql", "migrations/005_checkouts.sql"} {
		sql, err := os.ReadFile(name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read migration:", err)
			os.Exit(1)
		}
		_, err = db.Pool.Exec(context.Background(), string(sql))
		if err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			os.Exit(1)
		}
		fmt.Println("Migration", name, "applied.")
	}
}
