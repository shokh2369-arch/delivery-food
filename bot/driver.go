package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"food-telegram/config"
	"food-telegram/lang"
	"food-telegram/services"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// DriverBot handles driver interactions (uses DRIVER_BOT_TOKEN).
type DriverBot struct {
	api         *tgbotapi.BotAPI
	mainBot     *tgbotapi.BotAPI // for sending customer notifications
	messageBot  *tgbotapi.BotAPI // for sending admin notifications
	config      *config.Config
	stateMu     sync.RWMutex
	driverLang  map[int64]string // "uz" or "ru"
	driverLangMu sync.RWMutex
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
		api:         api,
		mainBot:     mainBotAPI,
		messageBot:  messageBotAPI,
		config:      cfg,
		driverLang:  make(map[int64]string),
	}, nil
}

func (d *DriverBot) getLang(userID int64) string {
	d.driverLangMu.RLock()
	defer d.driverLangMu.RUnlock()
	l := d.driverLang[userID]
	if l == "" || (l != lang.Uz && l != lang.Ru) {
		return ""
	}
	return l
}

func (d *DriverBot) setLang(userID int64, langCode string) {
	if langCode != lang.Uz && langCode != lang.Ru {
		return
	}
	d.driverLangMu.Lock()
	defer d.driverLangMu.Unlock()
	d.driverLang[userID] = langCode
}

func (d *DriverBot) sendLang(chatID int64, userID int64, key string, args ...interface{}) {
	l := d.getLang(userID)
	if l == "" {
		l = lang.Uz
	}
	text := lang.T(l, key, args...)
	d.send(chatID, text)
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

		d.sendLang(msg.Chat.ID, userID, "dr_please_use_buttons")
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
	// Always show language selection on /start
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("O'zbek", "lang:uz"),
			tgbotapi.NewInlineKeyboardButtonData("Русский", "lang:ru"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, lang.T(lang.Uz, "choose_lang_both"))
	msg.ReplyMarkup = kb
	if _, err := d.api.Send(msg); err != nil {
		log.Printf("driver bot send error: %v", err)
	}
}

func (d *DriverBot) sendDriverPanel(chatID int64, driver *services.Driver) {
	d.sendDriverPanelWithLocation(chatID, driver, nil)
}

func (d *DriverBot) sendDriverPanelWithLocation(chatID int64, driver *services.Driver, knownLocation *services.DriverLocation) {
	l := d.getLang(driver.TgUserID)
	if l == "" {
		l = lang.Uz
	}
	statusEmoji := "🟢"
	if driver.Status == services.DriverStatusOffline {
		statusEmoji = "🔴"
	}
	text := fmt.Sprintf(lang.T(l, "dr_panel"), statusEmoji, driver.Status)

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
		text += "\n" + fmt.Sprintf(lang.T(l, "dr_location_coords"), loc.Lat, loc.Lon)
	} else if driver.Status == services.DriverStatusOnline {
		text += "\n" + lang.T(l, "dr_location_missing")
	}

	kb := d.driverKeyboard(driver.TgUserID, driver.Status, hasLocation)
	d.sendWithInline(chatID, text, kb)
}

func (d *DriverBot) driverKeyboard(userID int64, status string, hasLocation bool) tgbotapi.InlineKeyboardMarkup {
	l := d.getLang(userID)
	if l == "" {
		l = lang.Uz
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_go_online"), "driver:online"),
			tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_go_offline"), "driver:offline"),
		},
	}
	if status == services.DriverStatusOnline && hasLocation {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_jobs"), "driver:jobs"),
			tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_active_order"), "driver:active"),
		})
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (d *DriverBot) handleLocation(chatID int64, userID int64, lat, lon float64) {
	ctx := context.Background()
	driver, err := services.GetDriverByTgUserID(ctx, userID)
	if err != nil || driver == nil {
		d.sendLang(chatID, userID, "dr_please_start")
		return
	}
	if driver.Status != services.DriverStatusOnline {
		d.sendLang(chatID, userID, "dr_please_go_online")
		return
	}
	if err := services.UpdateDriverLocation(ctx, driver.ID, lat, lon); err != nil {
		log.Printf("update driver location: %v", err)
		d.sendLang(chatID, userID, "dr_location_save_err")
		return
	}
	log.Printf("driver location saved: driver_id=%s lat=%.6f lon=%.6f", driver.ID, lat, lon)
	driver, _ = services.GetDriverByTgUserID(ctx, userID)
	if driver != nil {
		loc := &services.DriverLocation{
			DriverID: driver.ID,
			Lat:      lat,
			Lon:      lon,
		}
		l := d.getLang(userID)
		if l == "" {
			l = lang.Uz
		}
		kb := tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButtonLocation(lang.T(l, "dr_share_location")),
			),
		)
		kb.OneTimeKeyboard = false
		kb.ResizeKeyboard = true
		msg := tgbotapi.NewMessage(chatID, lang.T(l, "dr_location_updated"))
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

	// Language selection (before driver is required)
	if data == "lang:uz" || data == "lang:ru" {
		langCode := strings.TrimPrefix(data, "lang:")
		d.setLang(userID, langCode)
		ctx := context.Background()
		driver, err := services.RegisterDriver(ctx, userID, chatID)
		if err != nil {
			d.sendLang(chatID, userID, "dr_error", err.Error())
			return
		}
		d.sendDriverPanel(chatID, driver)
		return
	}

	ctx := context.Background()
	driver, err := services.GetDriverByTgUserID(ctx, userID)
	if err != nil || driver == nil {
		d.sendLang(chatID, userID, "dr_please_start")
		return
	}

	switch {
	case data == "driver:online":
		if err := services.UpdateDriverStatus(ctx, driver.ID, services.DriverStatusOnline); err != nil {
			d.sendLang(chatID, driver.TgUserID, "dr_error", err.Error())
			return
		}
		l := d.getLang(driver.TgUserID)
		if l == "" {
			l = lang.Uz
		}
		kb := tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButtonLocation(lang.T(l, "dr_share_location")),
			),
		)
		kb.OneTimeKeyboard = false
		kb.ResizeKeyboard = true
		msg := tgbotapi.NewMessage(chatID, lang.T(l, "dr_online_success"))
		msg.ReplyMarkup = kb
		d.api.Send(msg)
	case data == "driver:offline":
		if err := services.UpdateDriverStatus(ctx, driver.ID, services.DriverStatusOffline); err != nil {
			d.sendLang(chatID, driver.TgUserID, "dr_error", err.Error())
			return
		}
		removeKb := tgbotapi.NewMessage(chatID, lang.T(d.getLang(driver.TgUserID), "dr_offline_success"))
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
			d.sendLang(chatID, driver.TgUserID, "dr_invalid_order_id")
			return
		}
		d.handleAcceptOrder(chatID, driver, orderID)
	case strings.HasPrefix(data, "driver_status:"):
		parts := strings.SplitN(data, ":", 3)
		if len(parts) != 3 {
			d.api.Request(tgbotapi.NewCallback(cq.ID, lang.T(d.getLang(driver.TgUserID), "dr_invalid_order_id")))
			return
		}
		orderID, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || orderID <= 0 {
			d.api.Request(tgbotapi.NewCallback(cq.ID, lang.T(d.getLang(driver.TgUserID), "dr_invalid_order_id")))
			return
		}
		newStatus := parts[2]
		if newStatus == services.OrderStatusCompleted {
			d.handleCompleteDelivery(chatID, driver, orderID, cq.Message.MessageID)
		} else {
			d.handleDriverStatusUpdate(chatID, driver, orderID, newStatus, cq.Message.MessageID)
		}
		d.api.Request(tgbotapi.NewCallback(cq.ID, lang.T(d.getLang(driver.TgUserID), "dr_status_updated")))
	case strings.HasPrefix(data, "driver_done:"):
		orderIDStr := strings.TrimPrefix(data, "driver_done:")
		orderID, err := strconv.ParseInt(orderIDStr, 10, 64)
		if err != nil || orderID <= 0 {
			d.sendLang(chatID, driver.TgUserID, "dr_invalid_order_id")
			return
		}
		d.handleCompleteDelivery(chatID, driver, orderID, 0)
	case data == "driver:back":
		d.sendDriverPanel(chatID, driver)
	}
}

func (d *DriverBot) handleJobsNearMe(chatID int64, driver *services.Driver) {
	l := d.getLang(driver.TgUserID)
	if l == "" {
		l = lang.Uz
	}
	if driver.Status != services.DriverStatusOnline {
		d.sendLang(chatID, driver.TgUserID, "dr_please_go_online")
		return
	}
	ctx := context.Background()

	loc, err := services.GetDriverLocation(ctx, driver.ID)
	if err != nil || loc == nil {
		log.Printf("driver jobs: get location error driver_id=%s: %v", driver.ID, err)
		locAny, _ := services.GetDriverLocationAny(ctx, driver.ID)
		if locAny != nil {
			log.Printf("driver jobs: found old location lat=%.6f lon=%.6f updated_at=%s", locAny.Lat, locAny.Lon, locAny.UpdatedAt)
			kb := tgbotapi.NewReplyKeyboard(
				tgbotapi.NewKeyboardButtonRow(
					tgbotapi.NewKeyboardButtonLocation(lang.T(l, "dr_share_location")),
				),
			)
			kb.OneTimeKeyboard = false
			kb.ResizeKeyboard = true
			msg := tgbotapi.NewMessage(chatID, lang.T(l, "dr_location_stale"))
			msg.ReplyMarkup = kb
			d.api.Send(msg)
			return
		}
		d.sendLang(chatID, driver.TgUserID, "dr_location_stale")
		return
	}

	radiusKm := d.config.Delivery.DriverJobsRadius
	orders, err := services.GetNearbyReadyOrders(ctx, loc.Lat, loc.Lon, radiusKm, 10)
	if err != nil {
		log.Printf("driver jobs: query error: %v", err)
		d.sendLang(chatID, driver.TgUserID, "dr_error", err.Error())
		return
	}

	if len(orders) == 0 {
		d.sendLang(chatID, driver.TgUserID, "dr_no_jobs")
		return
	}

	displayLimit := 5
	if len(orders) < displayLimit {
		displayLimit = len(orders)
	}
	text := lang.T(l, "dr_jobs_header")
	var rows [][]tgbotapi.InlineKeyboardButton
	for i := 0; i < displayLimit; i++ {
		o := orders[i]
		text += fmt.Sprintf(lang.T(l, "dr_order_line"), o.ID)
		text += fmt.Sprintf(lang.T(l, "dr_order_items"), o.ItemsTotal)
		text += fmt.Sprintf(lang.T(l, "dr_order_delivery"), o.DeliveryFee)
		text += fmt.Sprintf(lang.T(l, "dr_order_total"), o.GrandTotal)
		text += fmt.Sprintf(lang.T(l, "dr_distance"), o.DistanceKm)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf(lang.T(l, "dr_accept_order"), o.ID), "driver_accept:"+strconv.FormatInt(o.ID, 10)),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_back"), "driver:back"),
	))
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	d.sendWithInline(chatID, text, kb)
}

func (d *DriverBot) handleActiveOrder(chatID int64, driver *services.Driver) {
	l := d.getLang(driver.TgUserID)
	if l == "" {
		l = lang.Uz
	}
	if driver.Status != services.DriverStatusOnline {
		d.sendLang(chatID, driver.TgUserID, "dr_please_go_online")
		return
	}
	ctx := context.Background()
	loc, err := services.GetDriverLocation(ctx, driver.ID)
	if err != nil || loc == nil {
		locAny, _ := services.GetDriverLocationAny(ctx, driver.ID)
		if locAny != nil {
			loc = locAny
		} else {
			kb := tgbotapi.NewReplyKeyboard(
				tgbotapi.NewKeyboardButtonRow(
					tgbotapi.NewKeyboardButtonLocation(lang.T(l, "dr_share_location")),
				),
			)
			kb.OneTimeKeyboard = false
			kb.ResizeKeyboard = true
			msg := tgbotapi.NewMessage(chatID, lang.T(l, "dr_location_stale"))
			msg.ReplyMarkup = kb
			d.api.Send(msg)
			return
		}
	}
	order, err := services.GetDriverActiveOrder(ctx, driver.ID)
	if err != nil {
		d.sendLang(chatID, driver.TgUserID, "dr_error", err.Error())
		return
	}
	if order == nil {
		d.sendLang(chatID, driver.TgUserID, "dr_no_active")
		return
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	var statusText string
	switch order.Status {
	case services.OrderStatusAssigned:
		statusText = lang.T(l, "dr_status_accepted")
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_mark_collected"), fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusPickedUp)),
		))
	case services.OrderStatusPickedUp:
		statusText = lang.T(l, "dr_status_picked")
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_start_delivering"), fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusDelivering)),
		))
	case services.OrderStatusDelivering:
		statusText = lang.T(l, "dr_status_delivering")
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_order_completed_btn"), fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusCompleted)),
		))
	case services.OrderStatusCompleted:
		statusText = lang.T(l, "dr_status_completed")
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_back"), "driver:back"),
	))
	text := fmt.Sprintf(lang.T(l, "dr_active_header"), order.ID, order.ItemsTotal, order.DeliveryFee, order.GrandTotal, statusText)
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	d.sendWithInline(chatID, text, kb)
}

func (d *DriverBot) handleAcceptOrder(chatID int64, driver *services.Driver, orderID int64) {
	if driver.Status != services.DriverStatusOnline {
		d.sendLang(chatID, driver.TgUserID, "dr_only_online_accept")
		return
	}
	ctx := context.Background()
	order, err := services.AcceptOrder(ctx, orderID, driver.ID, driver.TgUserID)
	if err != nil {
		if err.Error() == "bu buyurtma allaqachon olingan" {
			d.sendLang(chatID, driver.TgUserID, "dr_order_already_taken")
		} else {
			d.sendLang(chatID, driver.TgUserID, "dr_error", err.Error())
		}
		return
	}
	l := d.getLang(driver.TgUserID)
	if l == "" {
		l = lang.Uz
	}
	if order.LocationID > 0 {
		restaurantLoc, err := services.GetLocationByID(ctx, order.LocationID)
		if err == nil && restaurantLoc != nil {
			d.send(chatID, fmt.Sprintf(lang.T(l, "dr_restaurant_location"), restaurantLoc.Name))
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
	// Get admin's preferred order language
	adminLang, _ := services.GetAdminOrderLang(ctx, *adminChatID)
	if adminLang == "" {
		adminLang = lang.Uz
	}
	var statusLabel string
	switch o.Status {
	case services.OrderStatusNew:
		statusLabel = lang.T(adminLang, "adm_status_new")
	case services.OrderStatusAssigned:
		statusLabel = lang.T(adminLang, "adm_status_assigned")
	case services.OrderStatusPickedUp:
		statusLabel = lang.T(adminLang, "adm_status_picked_up")
	case services.OrderStatusDelivering:
		statusLabel = lang.T(adminLang, "adm_status_delivering")
	case services.OrderStatusCompleted:
		statusLabel = lang.T(adminLang, "adm_status_completed")
	default:
		statusLabel = strings.ToUpper(o.Status)
	}
	text := fmt.Sprintf(lang.T(adminLang, "adm_order_id"), orderID) + "\n\n" + fmt.Sprintf(lang.T(adminLang, "adm_total"), o.ItemsTotal) + "\n\n" + fmt.Sprintf(lang.T(adminLang, "adm_status"), statusLabel) + "\n\n" + lang.T(adminLang, "adm_driver_accepted")
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
		d.sendLang(chatID, driver.TgUserID, "dr_error", err.Error())
		return
	}

	order, _ := services.GetOrder(ctx, orderID)
	if order == nil {
		d.sendLang(chatID, driver.TgUserID, "dr_order_not_found")
		return
	}

	if newStatus == services.OrderStatusPickedUp {
		customerLat, customerLon, err := services.GetOrderCoordinates(ctx, orderID)
		if err == nil && customerLat != 0 && customerLon != 0 {
			d.sendLang(chatID, driver.TgUserID, "dr_customer_address")
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
				adminLang, _ := services.GetAdminOrderLang(ctx, *adminChatID)
				if adminLang == "" {
					adminLang = lang.Uz
				}
				var statusLabel string
				switch newStatus {
				case services.OrderStatusPickedUp:
					statusLabel = lang.T(adminLang, "adm_status_picked_up")
				case services.OrderStatusDelivering:
					statusLabel = lang.T(adminLang, "adm_status_delivering")
				case services.OrderStatusCompleted:
					statusLabel = lang.T(adminLang, "adm_status_completed")
				default:
					statusLabel = strings.ToUpper(newStatus)
				}
				text := fmt.Sprintf(lang.T(adminLang, "adm_order_id"), orderID) + "\n\n" + fmt.Sprintf(lang.T(adminLang, "adm_total"), o.ItemsTotal) + "\n\n" + fmt.Sprintf(lang.T(adminLang, "adm_status"), statusLabel)
				edit := tgbotapi.NewEditMessageText(*adminChatID, *adminMessageID, text)
				d.messageBot.Send(edit)
			}
		}
	}

	order, _ = services.GetOrder(ctx, orderID)
	if order != nil && messageID > 0 {
		l := d.getLang(driver.TgUserID)
		if l == "" {
			l = lang.Uz
		}
		var rows [][]tgbotapi.InlineKeyboardButton
		var statusText string
		switch order.Status {
		case services.OrderStatusAssigned:
			statusText = lang.T(l, "dr_status_accepted")
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_mark_collected"), fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusPickedUp)),
			))
		case services.OrderStatusPickedUp:
			statusText = lang.T(l, "dr_status_picked")
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_start_delivering"), fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusDelivering)),
			))
		case services.OrderStatusDelivering:
			statusText = lang.T(l, "dr_status_delivering")
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_order_completed_btn"), fmt.Sprintf("driver_status:%d:%s", order.ID, services.OrderStatusCompleted)),
			))
		case services.OrderStatusCompleted:
			statusText = lang.T(l, "dr_status_completed")
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "dr_back"), "driver:back"),
		))
		text := fmt.Sprintf(lang.T(l, "dr_active_header"), order.ID, order.ItemsTotal, order.DeliveryFee, order.GrandTotal, statusText)
		kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
		edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
		if len(kb.InlineKeyboard) > 0 {
			edit.ReplyMarkup = &kb
		} else {
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
		d.sendLang(chatID, driver.TgUserID, "dr_error", err.Error())
		return
	}
	order, _ := services.GetOrder(ctx, orderID)
	if order == nil {
		d.sendLang(chatID, driver.TgUserID, "dr_order_not_found")
		return
	}

	l := d.getLang(driver.TgUserID)
	if l == "" {
		l = lang.Uz
	}
	if messageID > 0 {
		text := fmt.Sprintf(lang.T(l, "dr_active_header_done"), order.ID, order.ItemsTotal, order.DeliveryFee, order.GrandTotal)
		emptyKb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
		editRemoveKb := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, emptyKb)
		d.api.Send(editRemoveKb)
		edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
		d.api.Send(edit)
	}

	d.sendLang(chatID, driver.TgUserID, "dr_delivery_completed", orderID)
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
			adminLang, _ := services.GetAdminOrderLang(ctx, *adminChatID)
			if adminLang == "" {
				adminLang = lang.Uz
			}
			text := fmt.Sprintf(lang.T(adminLang, "adm_order_id"), orderID) + "\n\n" + fmt.Sprintf(lang.T(adminLang, "adm_total"), order.ItemsTotal) + "\n\n" + fmt.Sprintf(lang.T(adminLang, "adm_status"), lang.T(adminLang, "adm_status_completed"))
			edit := tgbotapi.NewEditMessageText(*adminChatID, *adminMessageID, text)
			emptyKb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
			editRemoveKb := tgbotapi.NewEditMessageReplyMarkup(*adminChatID, *adminMessageID, emptyKb)
			d.messageBot.Send(editRemoveKb)
			d.messageBot.Send(edit)
		}
	}
}
