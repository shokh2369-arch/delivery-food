package main

import (
	"context"
	"fmt"
	"os"
	"strings"

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

	// Optional auto-migration (useful in production and for fresh DBs).
	// Set AUTO_MIGRATE=1 (or "true") to enable.
	if v := strings.TrimSpace(os.Getenv("AUTO_MIGRATE")); v == "1" || strings.EqualFold(v, "true") {
		if err := applyMigrations(context.Background(), false); err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			os.Exit(1)
		}
	}

	adminID := int64(0)
	if v := os.Getenv("ADMIN_ID"); v != "" {
		fmt.Sscanf(v, "%d", &adminID)
	}

	b, err := bot.New(cfg, adminID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bot:", err)
		os.Exit(1)
	}

	// Start adder bot (ADDER_TOKEN): big admin uses LOGIN; branch admins use their unique password
	if cfg.Telegram.AdderToken != "" {
		adder, err := bot.NewAdderBot(cfg, adminID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "adder bot:", err)
			os.Exit(1)
		}
		go adder.Start()
		fmt.Println("Qo'shuvchi bot ishga tushdi.")
	}

	// Start driver bot (DRIVER_BOT_TOKEN)
	if cfg.Telegram.DriverToken != "" {
		driverBot, err := bot.NewDriverBot(cfg, b.GetAPI(), b.GetMessageBot())
		if err != nil {
			fmt.Fprintln(os.Stderr, "driver bot:", err)
			os.Exit(1)
		}
		b.SetDriverBotAPI(driverBot.GetAPI())
		driverBot.SetOnOrderUpdated(func(orderID int64) {
			go b.RefreshOrderCards(context.Background(), orderID)
		})
		go driverBot.Start()
		fmt.Println("Yetkazib beruvchi bot ishga tushdi.")
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

	if err := applyMigrations(context.Background(), true); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}
