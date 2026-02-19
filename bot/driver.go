package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"food-telegram/config"
	"food-telegram/services"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// DriverBot handles driver interactions (uses DRIVER_BOT_TOKEN).
type DriverBot struct {
	api        *tgbotapi.BotAPI
	mainBot    *tgbotapi.BotAPI // for sending customer notifications
	messageBot *tgbotapi.BotAPI // for sending admin notifications
	config     *config.Config
	stateMu    sync.RWMutex
}

// NewDriverBot creates a driver bot using DRIVER_BOT_TOKEN.
func NewDriverBot(cfg *config.Config, mainBotAPI *tgbotapi.BotAPI, messageBotAPI *tgbotapi.BotAPI) (*DriverBot, error) {
	if cfg.Telegram.DriverToken == "" {
		return nil, fmt.Errorf("DRIVER_BOT_TOKEN not set")
	}
	api, err := tgbotapi.NewBotAPI(cfg.Telegram.DriverToken)
	if err != nil {
		return nil, err
	}
	return &DriverBot{
		api:        api,
		mainBot:    mainBotAPI,
		messageBot: messageBotAPI,
		config:     cfg,
	}, nil
}

func (d *DriverBot) Start() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := d.api.GetUpdatesChan(u)

	for update := range updates {
		if update.CallbackQuery != nil {
			d.handleCallback(update.CallbackQuery)
			continue
		}
		if update.Message == nil {
			continue
		}
		msg := update.Message
		userID := msg.From.ID
		text := strings.TrimSpace(msg.Text)

		if text == "/start" {
			d.handleStart(msg.Chat.ID, userID)
			continue
		}


		// Handle location updates (for online drivers)
		if msg.Location != nil {
			d.handleLocation(msg.Chat.ID, userID, msg.Location.Latitude, msg.Location.Longitude)
			continue
		}

		d.send(msg.Chat.ID, "Iltimos, tugmalardan birini tanlang.")
	}
}

func (d *DriverBot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := d.api.Send(msg); err != nil {
		log.Printf("driver bot send error: %v", err)
	}
}

func (d *DriverBot) sendWithInline(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := d.api.Send(msg); err != nil {
		log.Printf("driver bot send error: %v", err)
	}
}

func (d *DriverBot) handleStart(chatID int64, userID int64) {
	ctx := context.Background()
	driver, err := services.RegisterDriver(ctx, userID, chatID)
	if err != nil {
		d.send(chatID, "Xatolik: "+err.Error())
		return
	}
	d.sendDriverPanel(chatID, driver)
}

func (d *DriverBot) sendDriverPanel(chatID int64, driver *services.Driver) {
	d.sendDriverPanelWithLocation(chatID, driver, nil)
}

func (d *DriverBot) sendDriverPanelWithLocation(chatID int64, driver *services.Driver, knownLocation *services.DriverLocation) {
	statusEmoji := "🟢"
	if driver.Status == services.DriverStatusOffline {
		statusEmoji = "🔴"
	}
	text := fmt.Sprintf("🚗 Yetkazib beruvchi paneli\n\nHolat: %s %s", statusEmoji, driver.Status)
	
	// Check if driver has recent location
	ctx := context.Background()
	hasLocation := false
	var loc *services.DriverLocation
	if knownLocation != nil {
		loc = knownLocation
		hasLocation = true
	} else if driver.Status == services.DriverStatusOnline {
		loc, _ = services.GetDriverLocation(ctx, driver.ID)
		if loc != nil {
			hasLocation = true
		}
	}
	if hasLocation && loc != nil {
		text += fmt.Sprintf("\n📍 Lokatsiya: (%.4f, %.4f)", loc.Lat, loc.Lon)
	} else if driver.Status == services.DriverStatusOnline {
		text += "\n⚠️ Lokatsiya topilmadi — \"Share Location\" tugmasini bosing."
	}
	
	kb := d.driverKeyboard(driver.Status, hasLocation)
	d.sendWithInline(chatID, text, kb)
}

func (d *DriverBot) driverKeyboard(status string, hasLocation bool) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🟢 Go Online", "driver:online"),
			tgbotapi.NewInlineKeyboardButtonData("🔴 Go Offline", "driver:offline"),
		},
	}
	// Only show Jobs/Active Order if online AND has location
	if status == services.DriverStatusOnline && hasLocation {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("📋 Jobs Near Me", "driver:jobs"),
			tgbotapi.NewInlineKeyboardButtonData("📦 My Active Order", "driver:active"),
		})
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (d *DriverBot) handleLocation(chatID int64, userID int64, lat, lon float64) {
	ctx := context.Background()
	driver, err := services.GetDriverByTgUserID(ctx, userID)
	if err != nil || driver == nil {
		d.send(chatID, "Iltimos, avval /start ni bosing.")
		return
	}
	if driver.Status != services.DriverStatusOnline {
		d.send(chatID, "❌ Avval \"Go Online\" tugmasini bosing.")
		return
	}
	if err := services.UpdateDriverLocation(ctx, driver.ID, lat, lon); err != nil {
		log.Printf("update driver location: %v", err)
		d.send(chatID, "❌ Lokatsiyani saqlashda xatolik.")
		return
	}
	log.Printf("driver location saved: driver_id=%s lat=%.6f lon=%.6f", driver.ID, lat, lon)
	// Refresh driver object to get updated status
	driver, _ = services.GetDriverByTgUserID(ctx, userID)
	if driver != nil {
		// Create location object from what we just saved
		loc := &services.DriverLocation{
			DriverID: driver.ID,
			Lat:      lat,
			Lon:      lon,
		}
		// Acknowledge and refresh panel to show Jobs/Active Order buttons
		// Keep location keyboard visible for future updates
		kb := tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButtonLocation("📍 Share Location"),
			),
		)
		kb.OneTimeKeyboard = false
		kb.ResizeKeyboard = true
		msg := tgbotapi.NewMessage(chatID, "✅ Lokatsiya yangilandi.")
		msg.ReplyMarkup = kb
		d.api.Send(msg)
		d.sendDriverPanelWithLocation(chatID, driver, loc)
	}
}

func (d *DriverBot) handleCallback(cq *tgbotapi.CallbackQuery) {
	chatID := cq.Message.Chat.ID
	userID := cq.From.ID
	data := cq.Data

	d.api.Request(tgbotapi.NewCallback(cq.ID, ""))

	ctx := context.Background()
	driver, err := services.GetDriverByTgUserID(ctx, userID)
	if err != nil || driver == nil {
		d.send(chatID, "Iltimos, avval /start ni bosing.")
		return
	}

	switch {
	case data == "driver:online":
		if err := services.UpdateDriverStatus(ctx, driver.ID, services.DriverStatusOnline); err != nil {
			d.send(chatID, "Xatolik: "+err.Error())
			return
		}
		kb := tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButtonLocation("📍 Share Location"),
			),
		)
		kb.OneTimeKeyboard = false
		kb.ResizeKeyboard = true
		msg := tgbotapi.NewMessage(chatID, "✅ Online holatga o'tdingiz.\n\n📍 Iltimos, \"Share Location\" tugmasini bosing va lokatsiyangizni ulashing. Shundan so'ng \"Jobs Near Me\" va \"My Active Order\" tugmalari ko'rinadi.")
		msg.ReplyMarkup = kb
		d.api.Send(msg)
		// Don't show panel yet - wait for location to be shared
	case data == "driver:offline":
		if err := services.UpdateDriverStatus(ctx, driver.ID, services.DriverStatusOffline); err != nil {
			d.send(chatID, "Xatolik: "+err.Error())
			return
		}
		removeKb := tgbotapi.NewMessage(chatID, "🔴 Offline holatga o'tdingiz.")
		removeKb.ReplyMarkup = tgbotapi.NewRemoveKeyboard(true)
		d.api.Send(removeKb)
		driver.Status = services.DriverStatusOffline
		d.sendDriverPanel(chatID, driver)
	case data == "driver:jobs":
		d.handleJobsNearMe(chatID, driver)
	case data == "driver:active":
		d.handleActiveOrder(chatID, driver)
	case strings.HasPrefix(data, "driver_accept:"):
		orderIDStr := strings.TrimPrefix(data, "driver_accept:")
		orderID, err := strconv.ParseInt(orderIDStr, 10, 64)
		if err != nil || orderID <= 0 {
			d.send(chatID, "Noto'g'ri buyurtma ID.")
			return
		}
		d.handleAcceptOrder(chatID, driver, orderID)
	case strings.HasPrefix(data, "driver_status:"):
		parts := strings.SplitN(data, ":", 3)
		if len(parts) != 3 {
			d.api.Request(tgbotapi.NewCallback(cq.ID, "Noto'g'ri callback."))
			return
		}
		orderID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || orderID <= 0 {
			d.api.Request(tgbotapi.NewCallback(cq.ID, "Noto'g'ri buyurtma ID."))
			return
		}
		newStatus := parts[2]
		if newStatus == services.OrderStatusCompleted {
			d.handleCompleteDelivery(chatID, driver, orderID, cq.Message.MessageID)
		} else {
			d.handleDriverStatusUpdate(chatID, driver, orderID, newStatus, cq.Message.MessageID)
		}
		d.api.Request(tgbotapi.NewCallback(cq.ID, "✅ Status yangilandi."))
	case strings.HasPrefix(data, "driver_done:"):
		orderIDStr := strings.TrimPrefix(data, "driver_done:")
		orderID, err := strconv.ParseInt(orderIDStr, 10, 64)
		if err != nil || orderID <= 0 {
			d.send(chatID, "Noto'g'ri buyurtma ID.")
			return
		}
		d.handleCompleteDelivery(chatID, driver, orderID, 0)
	case data == "driver:back":
		d.sendDriverPanel(chatID, driver)
	}
}

func (d *DriverBot) handleJobsNearMe(chatID int64, driver *services.Driver) {
	if driver.Status != services.DriverStatusOnline {
		d.send(chatID, "❌ Avval \"Go Online\" tugmasini bosing.")
		return
	}
	ctx := context.Background()
	
	// Debug: Log driver info
	log.Printf("driver jobs: driver tg_user_id=%d driver_db_id=%s driver_status=%s", driver.TgUserID, driver.ID, driver.Status)
	
	// First try recent location (within 5 min)
	loc, err := services.GetDriverLocation(ctx, driver.ID)
	if err != nil || loc == nil {
		log.Printf("driver jobs: get location error driver_id=%s: %v", driver.ID, err)
		// Try any location for debugging
		locAny, _ := services.GetDriverLocationAny(ctx, driver.ID)
		if locAny != nil {
			log.Printf("driver jobs: found old location lat=%.6f lon=%.6f updated_at=%s, rejecting (older than 5 min)", locAny.Lat, locAny.Lon, locAny.UpdatedAt)
			kb := tgbotapi.NewReplyKeyboard(
				tgbotapi.NewKeyboardButtonRow(
					tgbotapi.NewKeyboardButtonLocation("📍 Share Location"),
				),
			)
			kb.OneTimeKeyboard = false
			kb.ResizeKeyboard = true
			msg := tgbotapi.NewMessage(chatID, "❌ Lokatsiyangiz eskirgan yoki yo'q. Iltimos live location yuboring.")
			msg.ReplyMarkup = kb
			d.api.Send(msg)
			return
		}
		log.Printf("driver jobs: no location found at all for driver_id=%s", driver.ID)
		d.send(chatID, "❌ Lokatsiyangiz eskirgan yoki yo'q. Iltimos live location yuboring.")
		return
	}
	
	// Debug: Log driver location
	log.Printf("driver jobs: driver location lat=%.6f lon=%.6f updated_at=%s", loc.Lat, loc.Lon, loc.UpdatedAt)
	
	// Get radius from config (default 50km for debugging)
	radiusKm := d.config.Delivery.DriverJobsRadius
	log.Printf("driver jobs: radius_km=%.1f", radiusKm)
	
	// Count ready orders before distance filter
	readyCount, _ := services.CountReadyOrders(ctx)
	log.Printf("driver jobs: COUNT ready orders (status='ready' AND driver_id IS NULL): %d", readyCount)
	
	// Get nearby orders (fetch more for debugging, display top 5)
	orders, err := services.GetNearbyReadyOrders(ctx, loc.Lat, loc.Lon, radiusKm, 10)
	if err != nil {
		log.Printf("driver jobs: query error driver_id=%s: %v", driver.ID, err)
		d.send(chatID, "Xatolik: "+err.Error())
		return
	}
	
	log.Printf("driver jobs: COUNT after distance filter: %d", len(orders))
	
	// Debug: Log top 3 candidate orders
	for i, o := range orders {
		if i >= 3 {
			break
		}
		log.Printf("driver jobs: candidate order #%d: id=%d lat=%.6f lon=%.6f distance_km=%.2f", i+1, o.ID, o.Lat, o.Lon, o.DistanceKm)
	}
	
	if len(orders) == 0 {
		d.send(chatID, "📭 Yaqin atrofda buyurtma topilmadi.")
		return
	}
	
	// Display top 5 orders
	displayLimit := 5
	if len(orders) < displayLimit {
		displayLimit = len(orders)
	}
	text := "📋 Yaqin atrofda tayyor buyurtmalar:\n\n"
	var rows [][]tgbotapi.InlineKeyboardButton
	for i := 0; i < displayLimit; i++ {
		o := orders[i]
		text += fmt.Sprintf("Buyurtma #%d\n", o.ID)
		text += fmt.Sprintf("Masofa: %.1f km\n", o.DistanceKm)
		text += fmt.Sprintf("Jami: %d UZS\n\n", o.GrandTotal)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("✅ Accept Order #%d", o.ID), "driver_accept:"+strconv.FormatInt(o.ID, 10)),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("« Back", "driver:back"),
	))
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	d.sendWithInline(chatID, text, kb)
}

func (d *DriverBot) handleActiveOrder(chatID int64, driver *services.Driver) {
	if driver.Status != services.DriverStatusOnline {
		d.send(chatID, "❌ Avval \"Go Online\" tugmasini bosing.")
		return
	}
	ctx := context.Background()
	loc, err := services.GetDriverLocation(ctx, driver.ID)
	if err != nil || loc == nil {
		// Try any location
		locAny, _ := services.GetDriverLocationAny(ctx, driver.ID)
		if locAny != nil {
			log.Printf("driver active: using location updated_at=%s", locAny.UpdatedAt)
			loc = locAny
		} else {
			kb := tgbotapi.NewReplyKeyboard(
				tgbotapi.NewKeyboardButtonRow(
					tgbotapi.NewKeyboardButtonLocation("📍 Share Location"),
				),
			)
			kb.OneTimeKeyboard = false
			kb.ResizeKeyboard = true
			msg := tgbotapi.NewMessage(chatID, "❌ Lokatsiyangiz topilmadi. Iltimos, lokatsiyangizni ulashing.")
			msg.ReplyMarkup = kb
			d.api.Send(msg)
			return
		}
	}
	order, err := services.GetDriverActiveOrder(ctx, driver.ID)
	if err != nil {
		d.send(chatID, "Xatolik: "+err.Error())
		return
	}
	if order == nil {
		d.send(chatID, "📭 Sizda faol buyurtma yo'q.")
		return
	}
	// Build inline buttons based on current status
	var rows [][]tgbotapi.InlineKeyboardButton
	var statusText string
	
	switch order.Status {
	case services.OrderStatusAssigned:
		statusText = "Buyurtma qabul qilindi"
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📦 Mark Collected", fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusPickedUp)),
		))
	case services.OrderStatusPickedUp:
		statusText = "Buyurtma olindi"
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛵 Start Delivering", fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusDelivering)),
		))
	case services.OrderStatusDelivering:
		statusText = "Yetkazilmoqda"
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Order completed", fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusCompleted)),
		))
	case services.OrderStatusCompleted:
		statusText = "Yetkazildi"
		// No buttons for completed orders
	}
	
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("« Back", "driver:back"),
	))
	
	text := fmt.Sprintf("📦 Faol buyurtma:\n\nBuyurtma #%d\nJami: %d UZS\nHolat: %s", order.ID, order.GrandTotal, statusText)
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	d.sendWithInline(chatID, text, kb)
}

func (d *DriverBot) handleAcceptOrder(chatID int64, driver *services.Driver, orderID int64) {
	if driver.Status != services.DriverStatusOnline {
		d.send(chatID, "❌ Faqat online holatda buyurtma qabul qilishingiz mumkin.")
		return
	}
	ctx := context.Background()
	order, err := services.AcceptOrder(ctx, orderID, driver.ID, driver.TgUserID)
	if err != nil {
		if err.Error() == "bu buyurtma allaqachon olingan" {
			d.send(chatID, "❌ Bu buyurtma allaqachon olingan.")
		} else {
			d.send(chatID, "❌ "+err.Error())
		}
		return
	}
	// Send restaurant location to driver
	if order.LocationID > 0 {
		restaurantLoc, err := services.GetLocationByID(ctx, order.LocationID)
		if err == nil && restaurantLoc != nil {
			// Send text message first
			d.send(chatID, fmt.Sprintf("📍 Restoran lokatsiyasi: %s", restaurantLoc.Name))
			// Send location
			locationMsg := tgbotapi.NewLocation(chatID, restaurantLoc.Lat, restaurantLoc.Lon)
			if _, err := d.api.Send(locationMsg); err != nil {
				log.Printf("send restaurant location to driver: %v", err)
			}
		}
	}

	// Show active order card with buttons
	d.handleActiveOrder(chatID, driver)

	// Notify customer
	if order.ChatID != "" {
		if customerChatID, err := strconv.ParseInt(order.ChatID, 10, 64); err == nil {
			customerMsg := services.CustomerMessageForOrderStatus(order, services.OrderStatusAssigned)
			msg := tgbotapi.NewMessage(customerChatID, customerMsg)
			if _, sendErr := d.mainBot.Send(msg); sendErr != nil {
				log.Printf("send customer driver assign notify: %v", sendErr)
			} else {
				_ = services.SaveOutboundMessage(ctx, customerChatID, customerMsg, map[string]interface{}{
					"channel":  "telegram",
					"sent_via": "driver_assign",
					"order_id": orderID,
				})
			}
		}
	}

	// Notify admin: Driver accepted
	if d.messageBot != nil {
		adminIDs, _ := services.GetBranchAdmins(ctx, order.LocationID)
		if len(adminIDs) > 0 {
			driverInfo := fmt.Sprintf("✅ Driver accepted")
			if driver.Phone != "" {
				driverInfo += fmt.Sprintf("\nPhone: %s", driver.Phone)
			}
			if driver.CarPlate != "" {
				driverInfo += fmt.Sprintf("\nCar: %s", driver.CarPlate)
			}
			adminMsg := fmt.Sprintf("%s\n\nOrder #%d", driverInfo, orderID)
			for _, adminID := range adminIDs {
				msg := tgbotapi.NewMessage(adminID, adminMsg)
				if _, err := d.messageBot.Send(msg); err != nil {
					log.Printf("send admin driver accept notify: %v", err)
				} else {
					_ = services.SaveOutboundMessage(ctx, adminID, adminMsg, map[string]interface{}{
						"channel":  "telegram",
						"sent_via": "driver_accept_admin",
						"order_id": orderID,
					})
				}
			}
		}
	}

	// Update admin order card with driver details
	d.updateAdminOrderCard(ctx, orderID, driver)
}

// updateAdminOrderCard updates the admin order card message with driver details when driver accepts.
func (d *DriverBot) updateAdminOrderCard(ctx context.Context, orderID int64, driver *services.Driver) {
	if d.messageBot == nil {
		return
	}
	adminChatID, adminMessageID, err := services.GetAdminMessageIDs(ctx, orderID)
	if err != nil || adminChatID == nil || adminMessageID == nil {
		// Fallback: send new message to admin
		o, _ := services.GetOrder(ctx, orderID)
		if o == nil {
			return
		}
		adminIDs, _ := services.GetBranchAdmins(ctx, o.LocationID)
		if len(adminIDs) == 0 {
			return
		}
		driverInfo := fmt.Sprintf("✅ Driver accepted\nDriver ID: %s", driver.ID)
		if driver.Phone != "" {
			driverInfo += fmt.Sprintf("\nPhone: %s", driver.Phone)
		}
		if driver.CarPlate != "" {
			driverInfo += fmt.Sprintf("\nCar: %s", driver.CarPlate)
		}
		msgText := fmt.Sprintf("Order #%d\n\n%s", orderID, driverInfo)
		for _, adminID := range adminIDs {
			msg := tgbotapi.NewMessage(adminID, msgText)
			d.messageBot.Send(msg)
		}
		return
	}
	// Get order details
	o, err := services.GetOrder(ctx, orderID)
	if err != nil || o == nil {
		log.Printf("failed to get order %d for admin card update: %v", orderID, err)
		return
	}
	// Reconstruct order card with driver info
	statusLabel := strings.ToUpper(o.Status)
	switch o.Status {
	case services.OrderStatusNew:
		statusLabel = "NEW"
	case services.OrderStatusAssigned:
		statusLabel = "ASSIGNED"
	case services.OrderStatusPickedUp:
		statusLabel = "PICKED_UP"
	case services.OrderStatusDelivering:
		statusLabel = "DELIVERING"
	case services.OrderStatusCompleted:
		statusLabel = "COMPLETED"
	}
	text := fmt.Sprintf("Order #%d\n\nTotal: %d UZS\n\nStatus: %s\n\n✅ Driver accepted", 
		orderID, o.GrandTotal, statusLabel)
	if driver.Phone != "" {
		text += fmt.Sprintf("\nPhone: %s", driver.Phone)
	}
	if driver.CarPlate != "" {
		text += fmt.Sprintf("\nCar: %s", driver.CarPlate)
	}
	// Build keyboard with contact button if phone exists
	var rows [][]tgbotapi.InlineKeyboardButton
	if driver.Phone != "" {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonURL("📞 Contact Driver", "tel:"+driver.Phone),
		))
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	// Try to edit the message
	edit := tgbotapi.NewEditMessageText(*adminChatID, *adminMessageID, text)
	if len(kb.InlineKeyboard) > 0 {
		edit.ReplyMarkup = &kb
	} else {
		// Remove keyboard if empty
		emptyKb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
		editRemoveKb := tgbotapi.NewEditMessageReplyMarkup(*adminChatID, *adminMessageID, emptyKb)
		d.messageBot.Send(editRemoveKb)
	}
	if _, err := d.messageBot.Send(edit); err != nil {
		log.Printf("failed to edit admin order card for order %d: %v, falling back to new message", orderID, err)
		// Fallback: send new message
		msg := tgbotapi.NewMessage(*adminChatID, text)
		if len(kb.InlineKeyboard) > 0 {
			msg.ReplyMarkup = kb
		}
		d.messageBot.Send(msg)
	}
}

// handleDriverStatusUpdate handles driver status updates (picked_up, delivering).
func (d *DriverBot) handleDriverStatusUpdate(chatID int64, driver *services.Driver, orderID int64, newStatus string, messageID int) {
	ctx := context.Background()
	err := services.UpdateDriverOrderStatus(ctx, orderID, driver.ID, driver.TgUserID, newStatus)
	if err != nil {
		d.send(chatID, "❌ "+err.Error())
		return
	}

	order, _ := services.GetOrder(ctx, orderID)
	if order == nil {
		d.send(chatID, "❌ Buyurtma topilmadi.")
		return
	}

	// If status is picked_up, send customer delivery location to driver
	if newStatus == services.OrderStatusPickedUp {
		customerLat, customerLon, err := services.GetOrderCoordinates(ctx, orderID)
		if err == nil && customerLat != 0 && customerLon != 0 {
			// Send text message first
			d.send(chatID, "📍 Mijoz yetkazib berish manzili")
			// Send location
			locationMsg := tgbotapi.NewLocation(chatID, customerLat, customerLon)
			if _, err := d.api.Send(locationMsg); err != nil {
				log.Printf("send customer location to driver: %v", err)
			}
		}
	}

	// Notify customer
	if order.ChatID != "" {
		if customerChatID, err := strconv.ParseInt(order.ChatID, 10, 64); err == nil {
			customerMsg := services.CustomerMessageForOrderStatus(order, newStatus)
			msg := tgbotapi.NewMessage(customerChatID, customerMsg)
			if _, sendErr := d.mainBot.Send(msg); sendErr != nil {
				log.Printf("send customer status notify: %v", sendErr)
			} else {
				_ = services.SaveOutboundMessage(ctx, customerChatID, customerMsg, map[string]interface{}{
					"channel":  "telegram",
					"sent_via": "driver_status_update",
					"order_id": orderID,
					"status":   newStatus,
				})
			}
		}
	}

	// Notify admin
	if d.messageBot != nil {
		adminIDs, _ := services.GetBranchAdmins(ctx, order.LocationID)
		var adminMsg string
		switch newStatus {
		case services.OrderStatusPickedUp:
			adminMsg = fmt.Sprintf("📦 Order #%d driver tomonidan OLINDI (collected).", orderID)
		case services.OrderStatusDelivering:
			adminMsg = fmt.Sprintf("🛵 Order #%d yetkazilmoqda.", orderID)
		}
		if adminMsg != "" {
			for _, adminID := range adminIDs {
				msg := tgbotapi.NewMessage(adminID, adminMsg)
				if _, err := d.messageBot.Send(msg); err != nil {
					log.Printf("send admin status notify: %v", err)
				} else {
					_ = services.SaveOutboundMessage(ctx, adminID, adminMsg, map[string]interface{}{
						"channel":  "telegram",
						"sent_via": "driver_status_update_admin",
						"order_id": orderID,
						"status":   newStatus,
					})
				}
			}
		}
		// Update admin order card if message ID exists
		adminChatID, adminMessageID, _ := services.GetAdminMessageIDs(ctx, orderID)
		if adminChatID != nil && adminMessageID != nil {
			o, _ := services.GetOrder(ctx, orderID)
			if o != nil {
				statusLabel := strings.ToUpper(newStatus)
				switch newStatus {
				case services.OrderStatusPickedUp:
					statusLabel = "PICKED_UP"
				case services.OrderStatusDelivering:
					statusLabel = "DELIVERING"
				case services.OrderStatusCompleted:
					statusLabel = "COMPLETED"
				}
				text := fmt.Sprintf("Order #%d\n\nTotal: %d UZS\n\nStatus: %s", orderID, o.GrandTotal, statusLabel)
				edit := tgbotapi.NewEditMessageText(*adminChatID, *adminMessageID, text)
				d.messageBot.Send(edit)
			}
		}
	}

	// Refresh active order view - update the message with new status
	order, _ = services.GetOrder(ctx, orderID)
	if order != nil && messageID > 0 {
		var rows [][]tgbotapi.InlineKeyboardButton
		var statusText string
		
		switch order.Status {
		case services.OrderStatusAssigned:
			statusText = "Buyurtma qabul qilindi"
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("📦 Mark Collected", fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusPickedUp)),
			))
		case services.OrderStatusPickedUp:
			statusText = "Buyurtma olindi"
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🛵 Start Delivering", fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusDelivering)),
			))
		case services.OrderStatusDelivering:
			statusText = "Yetkazilmoqda"
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ Order completed", fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusCompleted)),
			))
		case services.OrderStatusCompleted:
			statusText = "Yetkazildi"
			// No buttons for completed orders
		}
		
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("« Back", "driver:back"),
		))
		
		text := fmt.Sprintf("📦 Faol buyurtma:\n\nBuyurtma #%d\nJami: %d UZS\nHolat: %s", order.ID, order.GrandTotal, statusText)
		kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
		edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
		if len(kb.InlineKeyboard) > 0 {
			edit.ReplyMarkup = &kb
		} else {
			// Remove keyboard if empty
			emptyKb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
			editRemoveKb := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, emptyKb)
			d.api.Send(editRemoveKb)
		}
		d.api.Send(edit)
	}
}

func (d *DriverBot) handleCompleteDelivery(chatID int64, driver *services.Driver, orderID int64, messageID int) {
	ctx := context.Background()
	err := services.CompleteDeliveryByDriver(ctx, orderID, driver.ID, driver.TgUserID)
	if err != nil {
		d.send(chatID, "❌ "+err.Error())
		return
	}
	order, _ := services.GetOrder(ctx, orderID)
	if order == nil {
		d.send(chatID, "❌ Buyurtma topilmadi.")
		return
	}

	// Update the message if called from callback
	if messageID > 0 {
		text := fmt.Sprintf("📦 Faol buyurtma:\n\nBuyurtma #%d\nJami: %d UZS\nHolat: Yetkazildi", order.ID, order.GrandTotal)
		emptyKb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
		editRemoveKb := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, emptyKb)
		d.api.Send(editRemoveKb)
		edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
		d.api.Send(edit)
	}

	d.send(chatID, fmt.Sprintf("✅ Buyurtma #%d yetkazib berildi va yakunlandi.", orderID))
	d.sendDriverPanel(chatID, driver)

	// Notify customer
	if order.ChatID != "" {
		if customerChatID, err := strconv.ParseInt(order.ChatID, 10, 64); err == nil {
			customerMsg := services.CustomerMessageForOrderStatus(order, services.OrderStatusCompleted)
			msg := tgbotapi.NewMessage(customerChatID, customerMsg)
			if _, sendErr := d.mainBot.Send(msg); sendErr != nil {
				log.Printf("send customer delivery complete: %v", sendErr)
			} else {
				_ = services.SaveOutboundMessage(ctx, customerChatID, customerMsg, map[string]interface{}{
					"channel":  "telegram",
					"sent_via": "driver_delivery_complete",
					"order_id": orderID,
				})
			}
		}
	}

	// Notify admin
	if d.messageBot != nil {
		adminIDs, _ := services.GetBranchAdmins(ctx, order.LocationID)
		if len(adminIDs) > 0 {
			adminMsg := fmt.Sprintf("✅ Order #%d yetkazildi va yakunlandi.", orderID)
			for _, adminID := range adminIDs {
				msg := tgbotapi.NewMessage(adminID, adminMsg)
				if _, err := d.messageBot.Send(msg); err != nil {
					log.Printf("send admin delivery complete notify: %v", err)
				} else {
					_ = services.SaveOutboundMessage(ctx, adminID, adminMsg, map[string]interface{}{
						"channel":  "telegram",
						"sent_via": "driver_delivery_complete_admin",
						"order_id": orderID,
					})
				}
			}
		}
		// Update admin order card if message ID exists
		adminChatID, adminMessageID, _ := services.GetAdminMessageIDs(ctx, orderID)
		if adminChatID != nil && adminMessageID != nil {
			statusLabel := "COMPLETED"
			text := fmt.Sprintf("Order #%d\n\nTotal: %d UZS\n\nStatus: %s", orderID, order.GrandTotal, statusLabel)
			edit := tgbotapi.NewEditMessageText(*adminChatID, *adminMessageID, text)
			emptyKb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
			editRemoveKb := tgbotapi.NewEditMessageReplyMarkup(*adminChatID, *adminMessageID, emptyKb)
			d.messageBot.Send(editRemoveKb)
			d.messageBot.Send(edit)
		}
	}
}
