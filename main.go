package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"food-telegram/bot"
	"food-telegram/config"
	"food-telegram/db"
	"food-telegram/services"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	// Check for migrate subcommand
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		runMigrate(cfg)
		return
	}

	// Check for reset-db subcommand (delete all data, keep schema)
	if len(os.Args) > 1 && os.Args[1] == "reset-db" {
		runResetDB(cfg)
		return
	}

	if cfg.Telegram.Token == "" {
		fmt.Fprintln(os.Stderr, "TOKEN not set")
		os.Exit(1)
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
	var adder *bot.AdderBot
	if cfg.Telegram.AdderToken != "" {
		var err error
		adder, err = bot.NewAdderBot(cfg, adminID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "adder bot:", err)
			os.Exit(1)
		}
		go adder.Start()
		fmt.Println("Qo'shuvchi bot ishga tushdi.")
	}

	// Start Zayafka bot (ZAYAFKA): application form only; new-application notification sent via adder so superadmin Approve/Reject in adder
	if cfg.Telegram.ZayafkaToken != "" {
		var adderAPI *tgbotapi.BotAPI
		if adder != nil {
			adderAPI = adder.GetAPI()
		}
		zayafka, err := bot.NewZayafkaBot(cfg, adminID, adderAPI)
		if err != nil {
			fmt.Fprintln(os.Stderr, "zayafka bot:", err)
			os.Exit(1)
		}
		if adder != nil {
			adder.SetZayafkaAPI(zayafka.GetAPI())
			zayafka.SetOnExpRenew(adder.HandleExpRenewFromZayafka)
		}
		go zayafka.Start()
		fmt.Println("Zayafka bot ishga tushdi.")
	}

	// Start driver bot (DRIVER_BOT_TOKEN)
	var driverBot *bot.DriverBot
	if cfg.Telegram.DriverToken != "" {
		var err error
		driverBot, err = bot.NewDriverBot(cfg, b.GetAPI(), b.GetMessageBot())
		if err != nil {
			fmt.Fprintln(os.Stderr, "driver bot:", err)
			os.Exit(1)
		}
		b.SetDriverBotAPI(driverBot.GetAPI())
		driverBot.SetOnOrderUpdated(func(orderID int64) {
			go b.RefreshOrderCards(context.Background(), orderID)
		})
		if adder != nil {
			driverBot.SetOnSubscriptionExpired(adder.SendExpiredNotificationToSuperadmin)
			driverBot.SetOnRenewalRequest(adder.SendRenewalRequestToSuperadmin)
		}
		go driverBot.Start()
		fmt.Println("Yetkazib beruvchi bot ishga tushdi.")
	}

	if adder != nil {
		adder.SetOnSubscriptionRenewed(clearExpiredNotified)
	}
	// Background: automatically notify when subscription expires (not only on password input)
	go runExpiredSubscriptionNotifier(adder, driverBot)

	fmt.Println("Bot started.")
	b.Start()
}

var expiredNotifiedMu sync.Mutex
var expiredNotified = make(map[string]bool)

func clearExpiredNotified(tgUserID int64, role string) {
	key := fmt.Sprintf("%d:%s", tgUserID, role)
	expiredNotifiedMu.Lock()
	delete(expiredNotified, key)
	expiredNotifiedMu.Unlock()
}

func runExpiredSubscriptionNotifier(adder *bot.AdderBot, driverBot *bot.DriverBot) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if adder == nil && driverBot == nil {
			continue
		}
		ctx := context.Background()
		list, err := services.ListExpiredSubscriptions(ctx, 50)
		if err != nil || len(list) == 0 {
			continue
		}
		for _, row := range list {
			key := fmt.Sprintf("%d:%s", row.TgUserID, row.Role)
			expiredNotifiedMu.Lock()
			already := expiredNotified[key]
			if !already {
				expiredNotified[key] = true
			}
			expiredNotifiedMu.Unlock()
			if already {
				continue
			}
			services.MarkExpiredIfNeeded(ctx, row.TgUserID, row.Role)
			chatID, _ := services.GetChatIDForSubscriber(ctx, row.TgUserID, row.Role)
			if row.Role == services.UserRoleRestaurantAdmin && adder != nil {
				adder.NotifyExpiredUser(chatID, row.TgUserID, row.Role)
				adder.SendExpiredNotificationToSuperadmin(row.TgUserID, row.Role)
			}
			// Drivers: no subscription; skip expired notification
		}
	}
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

func runResetDB(cfg *config.Config) {
	if err := db.Init(cfg.DB); err != nil {
		fmt.Fprintln(os.Stderr, "db:", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx := context.Background()
	// Truncate all tables (order handles FKs; CASCADE clears dependent rows)
	sql := `TRUNCATE TABLE
		payment_receipts,
		subscriptions,
		login_attempts,
		user_credentials,
		application_driver_details,
		application_restaurant_details,
		applications,
		order_message_pointers,
		order_status_history,
		messages,
		checkouts,
		carts,
		driver_locations,
		drivers,
		branch_admin_access,
		admin_logins,
		customer_users,
		branch_admins,
		user_delivery_coords,
		user_locations,
		menu_items,
		orders,
		locations
		RESTART IDENTITY CASCADE`
	if _, err := db.Pool.Exec(ctx, sql); err != nil {
		fmt.Fprintln(os.Stderr, "reset-db:", err)
		os.Exit(1)
	}
	fmt.Println("All database data deleted (tables truncated).")
}
