package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"food-telegram/config"
	"food-telegram/db"
	"food-telegram/lang"
	"food-telegram/models"
	"food-telegram/services"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func cartItemToService(ci cartItem) services.CartItem {
	return services.CartItem{
		ID:       ci.ID,
		Name:     ci.Name,
		Price:    ci.Price,
		Qty:      ci.Qty,
		Category: ci.Category,
	}
}

func serviceToCartItem(sci services.CartItem) cartItem {
	return cartItem{
		ID:       sci.ID,
		Name:     sci.Name,
		Price:    sci.Price,
		Qty:      sci.Qty,
		Category: sci.Category,
	}
}

func cartStateToService(cs *cartState) *services.Cart {
	items := make([]services.CartItem, len(cs.Items))
	for i, ci := range cs.Items {
		items[i] = cartItemToService(ci)
	}
	return &services.Cart{
		Items:      items,
		ItemsTotal: cs.ItemsTotal,
	}
}

func serviceToCartState(sc *services.Cart) *cartState {
	items := make([]cartItem, len(sc.Items))
	for i, sci := range sc.Items {
		items[i] = serviceToCartItem(sci)
	}
	return &cartState{
		Items:      items,
		ItemsTotal: sc.ItemsTotal,
	}
}

func (b *Bot) getCart(ctx context.Context, userID int64) (*cartState, error) {
	sc, err := services.GetCart(ctx, userID)
	if err != nil {
		return nil, err
	}
	return serviceToCartState(sc), nil
}

func (b *Bot) saveCart(ctx context.Context, userID int64, cart *cartState) error {
	sc := cartStateToService(cart)
	return services.SaveCart(ctx, userID, sc)
}

func (b *Bot) deleteCart(ctx context.Context, userID int64) error {
	return services.DeleteCart(ctx, userID)
}

type cartItem struct {
	ID       string
	Name     string
	Price    int64
	Qty      int
	Category string // "food", "drink", "dessert" — for suggestion step
}

type cartState struct {
	Items      []cartItem
	ItemsTotal int64
}

type checkoutState struct {
	Cart  *cartState
	Phone string
}

type Bot struct {
	api          *tgbotapi.BotAPI
	messageBot   *tgbotapi.BotAPI // bot for sending order notifications (MESSAGE_TOKEN)
	driverBotAPI *tgbotapi.BotAPI // for pushing READY orders to nearby drivers (DRIVER_BOT_TOKEN)
	cfg          *config.Config
	admin        int64

	locSuggestions   map[int64][]services.LocationWithDistance
	locSuggestionsMu sync.RWMutex

	userSharedCoords   map[int64]struct{ Lat, Lon float64 }
	userSharedCoordsMu sync.RWMutex

	userLang   map[int64]string // "uz" or "ru"
	userLangMu sync.RWMutex

	orderLocks sync.Map // map[orderID]*sync.Mutex, for per-order locking in RefreshOrderCards
}

func New(cfg *config.Config, adminUserID int64) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.Telegram.Token)
	if err != nil {
		return nil, err
	}
	bot := &Bot{
		api:              api,
		cfg:              cfg,
		admin:            adminUserID,
		locSuggestions:   make(map[int64][]services.LocationWithDistance),
		userSharedCoords: make(map[int64]struct{ Lat, Lon float64 }),
		userLang:         make(map[int64]string),
	}
	// Initialize message bot if MESSAGE_TOKEN is set
	if cfg.Telegram.MessageToken != "" {
		messageBot, err := tgbotapi.NewBotAPI(cfg.Telegram.MessageToken)
		if err != nil {
			log.Printf("warning: failed to initialize message bot: %v", err)
		} else {
			bot.messageBot = messageBot
		}
	}
	return bot, nil
}

// GetAPI returns the main bot API (for driver bot to send customer notifications).
func (b *Bot) GetAPI() *tgbotapi.BotAPI {
	return b.api
}

// GetMessageBot returns the message bot API (for driver bot to send admin notifications).
func (b *Bot) GetMessageBot() *tgbotapi.BotAPI {
	return b.messageBot
}

// SetDriverBotAPI sets the driver bot API so this bot can push READY orders to nearby drivers.
func (b *Bot) SetDriverBotAPI(api *tgbotapi.BotAPI) {
	b.driverBotAPI = api
}

// apiForAudience returns the bot API used to send/edit messages for the given audience.
func (b *Bot) apiForAudience(audience string) *tgbotapi.BotAPI {
	switch audience {
	case "admin":
		return b.messageBot
	case "customer":
		return b.api
	case "driver":
		return b.driverBotAPI
	default:
		return b.api
	}
}

// cardMarkup converts OrderCardContent.Buttons to Telegram inline keyboard (URL vs callback).
func cardMarkup(c services.OrderCardContent) *tgbotapi.InlineKeyboardMarkup {
	if len(c.Buttons) == 0 {
		return nil
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, row := range c.Buttons {
		var btns []tgbotapi.InlineKeyboardButton
		for _, btn := range row {
			if btn.URL != "" {
				btns = append(btns, tgbotapi.NewInlineKeyboardButtonURL(btn.Text, btn.URL))
			} else {
				btns = append(btns, tgbotapi.NewInlineKeyboardButtonData(btn.Text, btn.CallbackData))
			}
		}
		rows = append(rows, btns)
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	return &kb
}

// UpsertOrderCard edits the existing order card message if we have a pointer; otherwise sends new and saves pointer.
// On "message not found" (e.g. deleted): send new message and upsert pointer.
// On "message is not modified": ignore.
func (b *Bot) UpsertOrderCard(ctx context.Context, audience string, orderID int64, chatID int64, content services.OrderCardContent) {
	api := b.apiForAudience(audience)
	if api == nil {
		return
	}
	chatIDPtr, messageID, ok, err := services.GetOrderMessagePointer(ctx, orderID, audience)
	if err != nil {
		log.Printf("UpsertOrderCard get pointer order_id=%d audience=%s: %v", orderID, audience, err)
		return
	}
	if ok {
		edit := tgbotapi.NewEditMessageText(chatIDPtr, messageID, content.Text)
		edit.ParseMode = ""
		if kb := cardMarkup(content); kb != nil {
			edit.ReplyMarkup = kb
		} else {
			emptyKb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
			edit.ReplyMarkup = &emptyKb
		}
		_, err = api.Send(edit)
		if err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "not found") || strings.Contains(errStr, "message to edit not found") {
				// Fallback: send new and update pointer
				msg := tgbotapi.NewMessage(chatIDPtr, content.Text)
				if kb := cardMarkup(content); kb != nil {
					msg.ReplyMarkup = *kb
				}
				sent, sendErr := api.Send(msg)
				if sendErr != nil {
					log.Printf("UpsertOrderCard fallback send order_id=%d audience=%s: %v", orderID, audience, sendErr)
					return
				}
				_ = services.UpsertOrderMessagePointer(ctx, orderID, audience, chatIDPtr, sent.MessageID)
				return
			}
			if strings.Contains(errStr, "not modified") {
				return
			}
			log.Printf("UpsertOrderCard edit order_id=%d audience=%s: %v", orderID, audience, err)
			return
		}
		_ = services.UpsertOrderMessagePointer(ctx, orderID, audience, chatIDPtr, messageID)
		return
	}
	// No pointer: send new and save
	msg := tgbotapi.NewMessage(chatID, content.Text)
	if kb := cardMarkup(content); kb != nil {
		msg.ReplyMarkup = *kb
	}
	sent, err := api.Send(msg)
	if err != nil {
		log.Printf("UpsertOrderCard send order_id=%d audience=%s: %v", orderID, audience, err)
		return
	}
	_ = services.UpsertOrderMessagePointer(ctx, orderID, audience, chatID, sent.MessageID)
}

// AnswerCallbackQuery sends a short toast for the callback (no new message).
func (b *Bot) AnswerCallbackQuery(callbackQueryID, text string) {
	if b.messageBot != nil {
		b.messageBot.Request(tgbotapi.NewCallback(callbackQueryID, text))
	}
}

// lockOrder locks by orderID and returns an unlock function. Used to prevent concurrent edits of the same order cards.
func (b *Bot) lockOrder(orderID int64) func() {
	v, _ := b.orderLocks.LoadOrStore(orderID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// RefreshOrderCards updates the order card for admin, customer, and (if assigned) driver. Call after any order change (e.g. from driver bot).
func (b *Bot) RefreshOrderCards(ctx context.Context, orderID int64) {
	unlock := b.lockOrder(orderID)
	defer unlock()

	o, err := services.GetOrder(ctx, orderID)
	if err != nil || o == nil {
		return
	}
	var driver *services.Driver
	if o.DriverID != nil && *o.DriverID != "" {
		driver, _ = services.GetDriverByID(ctx, *o.DriverID)
	}

	// Admin card: chat from pointer or first branch admin
	adminChatID, _, ok, _ := services.GetOrderMessagePointer(ctx, orderID, "admin")
	if !ok {
		admins, _ := services.GetBranchAdmins(ctx, o.LocationID)
		if len(admins) > 0 {
			adminChatID = admins[0]
		}
	}
	if adminChatID != 0 {
		adminLang, _ := services.GetAdminOrderLang(ctx, adminChatID)
		if adminLang == "" {
			adminLang = lang.Uz
		}
		content := services.BuildAdminCard(o, driver, adminLang)
		b.UpsertOrderCard(ctx, "admin", orderID, adminChatID, content)
	}

	// Customer card
	if o.ChatID != "" {
		if customerChatID, parseErr := strconv.ParseInt(o.ChatID, 10, 64); parseErr == nil {
			var trackURL string
			if o.Status == services.OrderStatusDelivering && driver != nil {
				loc, _ := services.GetDriverLocation(ctx, driver.ID)
				if loc != nil {
					trackURL = fmt.Sprintf("https://www.google.com/maps?q=%f,%f", loc.Lat, loc.Lon)
				}
			}
			content := services.BuildCustomerCard(o, driver, trackURL)
			b.UpsertOrderCard(ctx, "customer", orderID, customerChatID, content)
		}
	}

	// Driver card (only when driver assigned)
	if driver != nil {
		driverLang := lang.Uz
		content := services.BuildDriverCard(o, driverLang)
		b.UpsertOrderCard(ctx, "driver", orderID, driver.ChatID, content)
	}
}

func (b *Bot) setBotCommands() error {
	cfg := tgbotapi.SetMyCommandsConfig{
		Commands: []tgbotapi.BotCommand{
			{Command: "start", Description: "Bosh sahifa"},
			{Command: "language", Description: "Tilni o'zgartirish"},
			{Command: "orders", Description: "Buyurtmalarim"},
		},
	}
	_, err := b.api.Request(cfg)
	return err
}

func (b *Bot) Start() {
	// Register bot command menu (Telegram client shows these in the input menu)
	_ = b.setBotCommands()
	if b.messageBot != nil {
		go b.startOrderStatusCallbacks()
	}
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)

	for update := range updates {
		if update.CallbackQuery != nil {
			b.handleCallback(update.CallbackQuery)
			continue
		}
		if update.Message == nil {
			continue
		}
		msg := update.Message
		userID := msg.From.ID
		text := strings.TrimSpace(msg.Text)

		// Handle shared location (from user flow, not admin adder)
		if msg.Location != nil {
			b.handleUserLocation(msg.Chat.ID, userID, msg.Location.Latitude, msg.Location.Longitude)
			continue
		}

		switch {
		case text == "/start":
			b.handleStart(msg.Chat.ID, userID)
		case text == "/language":
			b.handleLanguage(msg.Chat.ID, userID)
		case text == "/orders":
			b.handleOrders(msg.Chat.ID, userID)
		case text == "/menu":
			b.userSharedCoordsMu.RLock()
			_, hasLocation := b.userSharedCoords[userID]
			b.userSharedCoordsMu.RUnlock()
			if !hasLocation {
				b.sendLang(msg.Chat.ID, userID, "please_share_loc")
				b.showWelcomeWithLocation(msg.Chat.ID, userID, b.getLang(userID))
			} else {
				b.sendMenu(msg.Chat.ID, userID)
			}
		case msg.Contact != nil:
			username := ""
			if msg.From != nil && msg.From.UserName != "" {
				username = msg.From.UserName
			}
			b.handleContact(msg.Chat.ID, userID, msg.Contact.PhoneNumber, username)
		case strings.HasPrefix(text, "/override"):
			b.handleOverride(msg.Chat.ID, userID, text)
		case strings.HasPrefix(text, "/stats"):
			b.handleStats(msg.Chat.ID, userID, text)
		case strings.HasPrefix(text, "/promote"):
			b.handlePromote(msg.Chat.ID, userID, text)
		case strings.HasPrefix(text, "/list_admins"):
			b.handleListAdmins(msg.Chat.ID, userID, text)
		case strings.HasPrefix(text, "/remove_admin"):
			b.handleRemoveAdmin(msg.Chat.ID, userID, text)
		}
	}
}

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) getLang(userID int64) string {
	b.userLangMu.RLock()
	l := b.userLang[userID]
	b.userLangMu.RUnlock()
	if l == lang.Uz || l == lang.Ru {
		return l
	}
	// Load from DB (persisted language)
	ctx := context.Background()
	if stored, ok := services.GetCustomerLanguage(ctx, userID); ok && (stored == lang.Uz || stored == lang.Ru) {
		b.userLangMu.Lock()
		b.userLang[userID] = stored
		b.userLangMu.Unlock()
		return stored
	}
	return ""
}

func (b *Bot) setLang(userID int64, langCode string) {
	if langCode != lang.Uz && langCode != lang.Ru {
		return
	}
	b.userLangMu.Lock()
	defer b.userLangMu.Unlock()
	b.userLang[userID] = langCode
}

func (b *Bot) sendLang(chatID int64, userID int64, key string, args ...interface{}) {
	text := lang.T(b.getLang(userID), key, args...)
	b.send(chatID, text)
}

func (b *Bot) sendWithInline(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) handleStart(chatID int64, userID int64) {
	ctx := context.Background()
	storedLang, hasLang := services.GetCustomerLanguage(ctx, userID)
	if !hasLang {
		// First time: ask language only once
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("O'zbek", "lang:uz"),
				tgbotapi.NewInlineKeyboardButtonData("Русский", "lang:ru"),
			),
		)
		msg := tgbotapi.NewMessage(chatID, lang.T(lang.Uz, "choose_lang_both"))
		msg.ReplyMarkup = kb
		if _, err := b.api.Send(msg); err != nil {
			log.Printf("send error: %v", err)
		}
		return
	}
	// Language already set: request location (do not ask language again)
	b.setLang(userID, storedLang)
	b.requestLocationOnly(chatID, userID)
}

// requestLocationOnly sends the location keyboard and "Lokatsiyangizni yuboring" (no welcome text). Used on /start when language already known.
func (b *Bot) requestLocationOnly(chatID int64, userID int64) {
	l := b.getLang(userID)
	if l == "" {
		l = lang.Uz
	}
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButtonLocation(lang.T(l, "share_location")),
		),
	)
	kb.OneTimeKeyboard = true
	kb.ResizeKeyboard = true
	msg := tgbotapi.NewMessage(chatID, lang.T(l, "request_location_only"))
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) handleLanguage(chatID int64, userID int64) {
	l := b.getLang(userID)
	if l == "" {
		l = lang.Uz
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("O'zbek", "lang_change:uz"),
			tgbotapi.NewInlineKeyboardButtonData("Русский", "lang_change:ru"),
		),
	)
	msg := tgbotapi.NewMessage(chatID, lang.T(l, "choose_lang_both"))
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) handleOrders(chatID int64, userID int64) {
	ctx := context.Background()
	l := b.getLang(userID)
	if l == "" {
		l = lang.Uz
	}
	orders, err := services.ListOrdersByUserID(ctx, userID, 20)
	if err != nil {
		b.send(chatID, "Error: "+err.Error())
		return
	}
	if len(orders) == 0 {
		b.send(chatID, lang.T(l, "my_orders_empty"))
		return
	}
	text := lang.T(l, "my_orders_header")
	datePart := func(s string) string {
		if len(s) >= 10 {
			return s[:10]
		}
		return s
	}
	for _, o := range orders {
		text += fmt.Sprintf("#%d — %s — %d so'm — %s\n", o.ID, o.Status, o.GrandTotal, datePart(o.CreatedAt))
	}
	b.send(chatID, text)
}

// showWelcomeWithLocation shows welcome message and location keyboard in the given language (after user chose lang).
func (b *Bot) showWelcomeWithLocation(chatID int64, userID int64, langCode string) {
	b.setLang(userID, langCode)
	l := b.getLang(userID)
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButtonLocation(lang.T(l, "share_location")),
		),
	)
	kb.OneTimeKeyboard = true
	kb.ResizeKeyboard = true
	msg := tgbotapi.NewMessage(chatID, lang.T(l, "welcome"))
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) categoryKeyboard(langCode string) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData(lang.T(langCode, "cat_food"), "cat:food"),
			tgbotapi.NewInlineKeyboardButtonData(lang.T(langCode, "cat_drink"), "cat:drink"),
			tgbotapi.NewInlineKeyboardButtonData(lang.T(langCode, "cat_dessert"), "cat:dessert"),
		},
		{tgbotapi.NewInlineKeyboardButtonData(lang.T(langCode, "back"), "back")},
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (b *Bot) menuKeyboard(userID int64, category string, langCode string) tgbotapi.InlineKeyboardMarkup {
	ctx := context.Background()
	var items []models.MenuItem
	// Try to load user's selected location; if not found or subscription expired, fall back to global menu
	if loc, err := services.GetUserLocation(ctx, userID); err == nil && loc != nil {
		active, _ := services.LocationHasActiveSubscription(ctx, loc.ID)
		if active {
			items, err = services.ListMenuByCategoryAndLocation(ctx, category, loc.ID)
			if err != nil {
				log.Printf("list menu by location: %v", err)
				items = nil
			}
		}
	}
	if items == nil {
		var err error
		items, err = services.ListMenuByCategory(ctx, category)
		if err != nil {
			log.Printf("list menu: %v", err)
			items = nil
		}
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, item := range items {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("%s — %d", item.Name, item.Price),
				"add:"+item.ID+":"+category,
			),
		))
	}

	cart, _ := b.getCart(ctx, userID)

	if cart != nil && len(cart.Items) > 0 {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(lang.T(langCode, "confirm_order"), "confirm"),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData(lang.T(langCode, "back_cats"), "back_cats"),
	))

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (b *Bot) sendMenu(chatID int64, userID int64) {
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.sendLang(chatID, userID, "please_share_loc")
		b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
		return
	}
	l := b.getLang(userID)
	ctx := context.Background()
	cart, _ := b.getCart(ctx, userID)

	text := lang.T(l, "menu_header")
	if cart != nil && len(cart.Items) > 0 {
		text += "\n\n🛒 *" + lang.T(l, "cart_label") + ":*\n"
		for _, it := range cart.Items {
			text += fmt.Sprintf("• %s × %d — %d\n", it.Name, it.Qty, it.Price*int64(it.Qty))
		}
		text += fmt.Sprintf("\n*%s: %d*", lang.T(l, "jami"), cart.ItemsTotal)
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = b.categoryKeyboard(l)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) sendCategoryMenu(chatID int64, userID int64, category string) {
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.sendLang(chatID, userID, "please_share_loc")
		b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
		return
	}
	l := b.getLang(userID)
	ctx := context.Background()
	cart, _ := b.getCart(ctx, userID)

	catLabel := map[string]string{"food": lang.T(l, "cat_food_label"), "drink": lang.T(l, "cat_drink_label"), "dessert": lang.T(l, "cat_dessert_label")}[category]
	text := fmt.Sprintf("📋 *%s*\n\n%s", catLabel, lang.T(l, "category_choose"))
	if cart != nil && len(cart.Items) > 0 {
		text += "\n\n🛒 *" + lang.T(l, "cart_label") + ":*\n"
		for _, it := range cart.Items {
			text += fmt.Sprintf("• %s × %d — %d\n", it.Name, it.Qty, it.Price*int64(it.Qty))
		}
		text += fmt.Sprintf("\n*%s: %d*", lang.T(l, "jami"), cart.ItemsTotal)
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = b.menuKeyboard(userID, category, l)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) handleCallback(cq *tgbotapi.CallbackQuery) {
	chatID := cq.Message.Chat.ID
	userID := cq.From.ID
	data := cq.Data

	b.api.Request(tgbotapi.NewCallback(cq.ID, ""))

	switch {
	case data == "lang:uz" || data == "lang:ru":
		// First-time language selection (from /start): persist then request location
		langCode := strings.TrimPrefix(data, "lang:")
		if langCode != lang.Uz && langCode != lang.Ru {
			break
		}
		ctx := context.Background()
		_ = services.SetCustomerLanguage(ctx, userID, langCode)
		b.setLang(userID, langCode)
		b.showWelcomeWithLocation(chatID, userID, langCode)
	case data == "lang_change:uz" || data == "lang_change:ru":
		// Manual language change (from /language): persist and confirm
		langCode := strings.TrimPrefix(data, "lang_change:")
		if langCode != lang.Uz && langCode != lang.Ru {
			break
		}
		ctx := context.Background()
		_ = services.SetCustomerLanguage(ctx, userID, langCode)
		b.setLang(userID, langCode)
		b.send(chatID, lang.T(langCode, "language_changed"))
	case data == "menu":
		// Check if user has shared location before showing menu
		b.userSharedCoordsMu.RLock()
		_, hasLocation := b.userSharedCoords[userID]
		b.userSharedCoordsMu.RUnlock()
		if !hasLocation {
			b.sendLang(chatID, userID, "please_share_loc")
			b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
		} else {
			b.sendMenu(chatID, userID)
		}
	case strings.HasPrefix(data, "locsel:"):
		// User selected a specific fast food location
		idStr := strings.TrimPrefix(data, "locsel:")
		locID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || locID <= 0 {
			b.sendLang(chatID, userID, "wrong_branch")
			return
		}
		ctx := context.Background()
		if err := services.SetUserLocation(ctx, userID, locID); err != nil {
			b.sendLang(chatID, userID, "branch_save_err")
			return
		}
		l := b.getLang(userID)
		msgText := lang.T(l, "branch_selected")
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "view_menu"), "menu"),
			),
		)
		b.sendWithInline(chatID, msgText, kb)
	case strings.HasPrefix(data, "loc:page:"):
		// Paginate location suggestions with distance (after sharing location)
		pageStr := strings.TrimPrefix(data, "loc:page:")
		page, _ := strconv.Atoi(pageStr)
		if page < 0 {
			page = 0
		}
		b.sendLocationSuggestions(chatID, userID, page, true)
	case strings.HasPrefix(data, "locm:page:"):
		// Paginate manual location suggestions (no distance)
		pageStr := strings.TrimPrefix(data, "locm:page:")
		page, _ := strconv.Atoi(pageStr)
		if page < 0 {
			page = 0
		}
		b.sendLocationSuggestionsManual(chatID, userID, page)
	case data == "loc:menu":
		b.sendMenu(chatID, userID)
	case data == "back":
		b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
	case data == "back_cats":
		// Check location before showing menu
		b.userSharedCoordsMu.RLock()
		_, hasLocation := b.userSharedCoords[userID]
		b.userSharedCoordsMu.RUnlock()
		if !hasLocation {
			b.sendLang(chatID, userID, "please_share_loc")
			b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
		} else {
			b.sendMenu(chatID, userID)
		}
	case strings.HasPrefix(data, "cat:"):
		// Check location before showing category menu
		b.userSharedCoordsMu.RLock()
		_, hasLocation := b.userSharedCoords[userID]
		b.userSharedCoordsMu.RUnlock()
		if !hasLocation {
			b.sendLang(chatID, userID, "please_share_loc")
			b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
		} else {
			b.sendCategoryMenu(chatID, userID, strings.TrimPrefix(data, "cat:"))
		}
	case strings.HasPrefix(data, "add:"):
		rest := strings.TrimPrefix(data, "add:")
		parts := strings.SplitN(rest, ":", 2)
		itemID := parts[0]
		category := "food"
		if len(parts) > 1 {
			category = parts[1]
		}
		b.addToCart(chatID, userID, itemID, category, cq.Message.MessageID)
	case data == "confirm":
		b.sendSuggestionScreen(chatID, userID)
	case data == "confirm_final":
		b.requestPhone(chatID, userID)
	case data == "confirm_reject":
		b.sendMenu(chatID, userID)
	case strings.HasPrefix(data, "checkout_delivery:"):
		b.handleCheckoutDeliveryCallback(cq)
	case strings.HasPrefix(data, "suggest:"):
		// Check location before showing suggestions
		b.userSharedCoordsMu.RLock()
		_, hasLocation := b.userSharedCoords[userID]
		b.userSharedCoordsMu.RUnlock()
		if !hasLocation {
			b.sendLang(chatID, userID, "please_share_loc")
			b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
		} else {
			cat := strings.TrimPrefix(data, "suggest:")
			if cat == "food" || cat == "drink" || cat == "dessert" {
				b.sendCategoryMenu(chatID, userID, cat)
			}
		}
	}
}

func (b *Bot) addToCart(chatID int64, userID int64, itemID string, category string, editMsgID int) {
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.sendLang(chatID, userID, "please_share_loc")
		b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
		return
	}

	ctx := context.Background()
	item, err := services.GetMenuItem(ctx, itemID)
	if err != nil || item == nil {
		return
	}

	cart, err := b.getCart(ctx, userID)
	if err != nil {
		cart = &cartState{Items: []cartItem{}}
	}
	found := false
	for i := range cart.Items {
		if cart.Items[i].ID == itemID {
			cart.Items[i].Qty++
			found = true
			break
		}
	}
	if !found {
		cart.Items = append(cart.Items, cartItem{ID: item.ID, Name: item.Name, Price: item.Price, Qty: 1, Category: item.Category})
	}
	cart.ItemsTotal += item.Price
	if err := b.saveCart(ctx, userID, cart); err != nil {
		log.Printf("failed to save cart: %v", err)
	}

	l := b.getLang(userID)
	catLabel := map[string]string{"food": lang.T(l, "cat_food_label"), "drink": lang.T(l, "cat_drink_label"), "dessert": lang.T(l, "cat_dessert_label")}[category]
	text := fmt.Sprintf("📋 *%s*\n\n%s\n\n🛒 *%s:*\n", catLabel, lang.T(l, "product_added"), lang.T(l, "cart_label"))
	for _, it := range cart.Items {
		text += fmt.Sprintf("• %s × %d — %d\n", it.Name, it.Qty, it.Price*int64(it.Qty))
	}
	text += fmt.Sprintf("\n*%s: %d*\n\n%s", lang.T(l, "jami"), cart.ItemsTotal, lang.T(l, "confirm_prompt"))

	edit := tgbotapi.NewEditMessageText(chatID, editMsgID, text)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: b.menuKeyboard(userID, category, l).InlineKeyboard}
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("edit error: %v", err)
	}
}

// sendSuggestionScreen shows cart, delivery fee (0 or 1000 by rule), grand total; user can Accept (Tasdiqlash) or Reject (Bekor).
func (b *Bot) sendSuggestionScreen(chatID int64, userID int64) {
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.sendLang(chatID, userID, "please_share_loc")
		b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
		return
	}
	l := b.getLang(userID)
	ctx := context.Background()
	cart, err := b.getCart(ctx, userID)
	if err != nil || cart == nil || len(cart.Items) == 0 {
		b.sendLang(chatID, userID, "cart_empty")
		b.showWelcomeWithLocation(chatID, userID, l)
		return
	}

	// Delivery fee: distance from restaurant (branch) to customer (shared location). Require valid coords for both.
	var deliveryFee int64
	branch, _ := services.GetUserLocation(ctx, userID)
	customerLat, customerLon, hasCustomer := b.getCustomerCoords(ctx, userID)
	hasValidBranch := branch != nil && (branch.Lat != 0 || branch.Lon != 0)
	hasValidCustomer := hasCustomer && (customerLat != 0 || customerLon != 0)
	if hasValidBranch && hasValidCustomer {
		distanceKm := services.HaversineDistanceKm(branch.Lat, branch.Lon, customerLat, customerLon)
		baseFee := b.cfg.Delivery.BaseFee
		if baseFee < 0 {
			baseFee = 5000
		}
		ratePerKm := b.cfg.Delivery.RatePerKm
		if ratePerKm <= 0 {
			ratePerKm = 4000
		}
		rawFee := services.CalcDeliveryFee(distanceKm, baseFee, ratePerKm)
		deliveryFee = services.ApplyDeliveryFeeRule(rawFee)
	}
	grandTotal := cart.ItemsTotal + deliveryFee

	hasCategory := map[string]bool{}
	for _, it := range cart.Items {
		if it.Category != "" {
			hasCategory[it.Category] = true
		}
	}

	var row []tgbotapi.InlineKeyboardButton
	if !hasCategory["drink"] {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "cat_drink"), "suggest:drink"))
	}
	if !hasCategory["dessert"] {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "cat_dessert"), "suggest:dessert"))
	}
	if !hasCategory["food"] {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "cat_food"), "suggest:food"))
	}
	row = append(row, tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "accept_confirm"), "confirm_final"))
	row = append(row, tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "reject_cancel"), "confirm_reject"))

	kb := tgbotapi.NewInlineKeyboardMarkup(row)
	text := lang.T(l, "your_order") + "\n\n"
	for _, it := range cart.Items {
		text += fmt.Sprintf("• %s × %d — %d\n", it.Name, it.Qty, it.Price*int64(it.Qty))
	}
	text += fmt.Sprintf("\n*%s: %d*\n", lang.T(l, "jami"), cart.ItemsTotal)
	text += fmt.Sprintf("\n*%s*\n\n%s", lang.T(l, "grand_total_label", grandTotal), lang.T(l, "add_more_confirm"))

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) requestPhone(chatID int64, userID int64) {
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.sendLang(chatID, userID, "please_share_loc")
		b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
		return
	}
	l := b.getLang(userID)
	ctx := context.Background()
	cart, err := b.getCart(ctx, userID)
	if err != nil || cart == nil || len(cart.Items) == 0 {
		b.sendLang(chatID, userID, "cart_empty")
		return
	}
	// Copy cart into checkout
	checkout := &services.Checkout{
		CartItems:  make([]services.CartItem, len(cart.Items)),
		ItemsTotal: cart.ItemsTotal,
		Phone:      "",
	}
	for i, ci := range cart.Items {
		checkout.CartItems[i] = cartItemToService(ci)
	}
	if err := services.SaveCheckout(ctx, userID, checkout); err != nil {
		log.Printf("failed to save checkout: %v", err)
	}
	// Delete cart after moving to checkout
	b.deleteCart(ctx, userID)

	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButtonContact(lang.T(l, "share_phone")),
		),
	)
	kb.OneTimeKeyboard = true
	kb.ResizeKeyboard = true

	msg := tgbotapi.NewMessage(chatID, lang.T(l, "request_phone", checkout.ItemsTotal))
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) handleUserLocation(chatID int64, userID int64, lat, lon float64) {
	l := b.getLang(userID)
	ctx := context.Background()
	locs, err := services.ListLocationsForCustomer(ctx)
	if err != nil {
		b.removeKeyboard(chatID, lang.T(l, "locations_err"))
		return
	}
	if len(locs) == 0 {
		b.removeKeyboard(chatID, lang.T(l, "no_branches"))
		b.sendMenu(chatID, userID)
		return
	}

	withDist := services.SortLocationsByDistance(float64(lat), float64(lon), locs)
	b.locSuggestionsMu.Lock()
	b.locSuggestions[userID] = withDist
	b.locSuggestionsMu.Unlock()

	// Store user's shared coordinates (memory + DB so fee calculation works at checkout)
	b.userSharedCoordsMu.Lock()
	b.userSharedCoords[userID] = struct{ Lat, Lon float64 }{Lat: lat, Lon: lon}
	b.userSharedCoordsMu.Unlock()
	if err := services.SetUserDeliveryCoords(ctx, userID, lat, lon); err != nil {
		log.Printf("SetUserDeliveryCoords: %v", err)
	}

	b.sendLocationSuggestions(chatID, userID, 0, false)
}

// sendLocationSuggestions shows a paginated list (5 per page) of nearest fast food locations.
// If removeKeyboard is true, it will also remove the reply keyboard.
func (b *Bot) sendLocationSuggestions(chatID int64, userID int64, page int, fromCallback bool) {
	const pageSize = 5

	b.locSuggestionsMu.RLock()
	list := b.locSuggestions[userID]
	b.locSuggestionsMu.RUnlock()

	if len(list) == 0 {
		txt := lang.T(b.getLang(userID), "no_locations")
		if fromCallback {
			b.send(chatID, txt)
		} else {
			b.removeKeyboard(chatID, txt)
		}
		return
	}

	start := page * pageSize
	if start >= len(list) {
		page = 0
		start = 0
	}
	end := start + pageSize
	if end > len(list) {
		end = len(list)
	}

	langCode := b.getLang(userID)
	text := lang.T(langCode, "nearest_locations")
	var buttons [][]tgbotapi.InlineKeyboardButton
	for i := start; i < end; i++ {
		loc := list[i]
		text += fmt.Sprintf("%d) %s — %.1f km\n", i+1, loc.Location.Name, loc.Distance)
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("%d) %s", i+1, loc.Location.Name),
				fmt.Sprintf("locsel:%d", loc.Location.ID),
			),
		))
	}
	text += lang.T(langCode, "select_branch")

	var row []tgbotapi.InlineKeyboardButton
	if page > 0 {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(lang.T(langCode, "prev"), fmt.Sprintf("loc:page:%d", page-1)))
	}
	if end < len(list) {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(lang.T(langCode, "next"), fmt.Sprintf("loc:page:%d", page+1)))
	}
	if len(row) > 0 {
		buttons = append(buttons, row)
	}

	kb := tgbotapi.NewInlineKeyboardMarkup(buttons...)

	// For both first-time and pagination, send a single message with suggestions.
	// (We accept that the reply keyboard may remain visible until the next message
	// that removes it, to avoid duplicate texts.)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

// sendLocationSuggestionsManual shows all locations without distance (for users who skipped location).
func (b *Bot) sendLocationSuggestionsManual(chatID int64, userID int64, page int) {
	const pageSize = 5

	ctx := context.Background()
	locs, err := services.ListLocationsForCustomer(ctx)
	if err != nil {
		b.removeKeyboard(chatID, "Joylashuvlar ro'yxatini yuklashda xatolik yuz berdi.")
		return
	}
	l := b.getLang(userID)
	if len(locs) == 0 {
		b.removeKeyboard(chatID, lang.T(l, "no_branches_manual"))
		b.sendMenu(chatID, userID)
		return
	}

	start := page * pageSize
	if start >= len(locs) {
		page = 0
		start = 0
	}
	end := start + pageSize
	if end > len(locs) {
		end = len(locs)
	}

	text := lang.T(l, "locations_list")
	var buttons [][]tgbotapi.InlineKeyboardButton
	for i := start; i < end; i++ {
		loc := locs[i]
		text += fmt.Sprintf("%d) %s\n", i+1, loc.Name)
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("%d) %s", i+1, loc.Name),
				fmt.Sprintf("locsel:%d", loc.ID),
			),
		))
	}
	text += lang.T(l, "select_branch")

	var buttonsNav [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	if page > 0 {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "prev"), fmt.Sprintf("locm:page:%d", page-1)))
	}
	if end < len(locs) {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "next"), fmt.Sprintf("locm:page:%d", page+1)))
	}
	if len(row) > 0 {
		buttonsNav = append(buttonsNav, row)
	}

	allRows := append(buttons, buttonsNav...)
	kb := tgbotapi.NewInlineKeyboardMarkup(allRows...)

	// Remove the reply keyboard from start screen
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

// getCustomerCoords returns the customer's delivery coordinates (from memory or DB). Used for distance-based delivery fee.
func (b *Bot) getCustomerCoords(ctx context.Context, userID int64) (lat, lon float64, ok bool) {
	b.userSharedCoordsMu.RLock()
	if c, has := b.userSharedCoords[userID]; has {
		b.userSharedCoordsMu.RUnlock()
		return c.Lat, c.Lon, true
	}
	b.userSharedCoordsMu.RUnlock()
	return services.GetUserDeliveryCoords(ctx, userID)
}

func (b *Bot) handleContact(chatID int64, userID int64, phone string, customerUsername string) {
	// Verify user has shared location before allowing order creation
	_, _, hasLocation := b.getCustomerCoords(context.Background(), userID)
	if !hasLocation {
		b.sendLang(chatID, userID, "please_share_loc")
		b.showWelcomeWithLocation(chatID, userID, b.getLang(userID))
		return
	}
	l := b.getLang(userID)
	ctx := context.Background()
	checkout, err := services.GetCheckout(ctx, userID)
	if err != nil || checkout == nil || len(checkout.CartItems) == 0 {
		b.removeKeyboard(chatID, lang.T(l, "please_add_order"))
		return
	}
	checkout.Phone = phone
	if err := services.SaveCheckout(ctx, userID, checkout); err != nil {
		b.removeKeyboard(chatID, lang.T(l, "please_add_order"))
		return
	}
	b.removeKeyboard(chatID, "✅")

	// Branch = restaurant they ordered from; customer = shared delivery address. Fee = distance(branch → customer).
	branch, _ := services.GetUserLocation(ctx, userID)
	customerLat, customerLon, _ := b.getCustomerCoords(ctx, userID)

	baseFee := b.cfg.Delivery.BaseFee
	if baseFee < 0 {
		baseFee = 5000
	}
	ratePerKm := b.cfg.Delivery.RatePerKm
	if ratePerKm <= 0 {
		ratePerKm = 4000
	}
	var deliveryFee int64
	hasValidBranch := branch != nil && (branch.Lat != 0 || branch.Lon != 0)
	hasValidCustomer := customerLat != 0 || customerLon != 0
	if hasValidBranch && hasValidCustomer {
		distanceKm := services.HaversineDistanceKm(branch.Lat, branch.Lon, customerLat, customerLon)
		rawFee := services.CalcDeliveryFee(distanceKm, baseFee, ratePerKm)
		deliveryFee = services.ApplyDeliveryFeeRule(rawFee)
	}
	text := lang.T(l, "how_receive")
	if deliveryFee > 0 {
		text += fmt.Sprintf("\n\n🚚 Yetkazib berish: %d so'm", deliveryFee)
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "delivery_option", deliveryFee), "checkout_delivery:delivery"),
			tgbotapi.NewInlineKeyboardButtonData(lang.T(l, "pickup_option"), "checkout_delivery:pickup"),
		),
	)
	b.sendWithInline(chatID, text, kb)
}

func (b *Bot) handleCheckoutDeliveryCallback(cq *tgbotapi.CallbackQuery) {
	chatID := cq.Message.Chat.ID
	userID := cq.From.ID
	parts := strings.SplitN(cq.Data, ":", 2)
	if len(parts) != 2 {
		b.api.Request(tgbotapi.NewCallback(cq.ID, ""))
		return
	}
	deliveryType := parts[1]
	if deliveryType != "delivery" && deliveryType != "pickup" {
		b.api.Request(tgbotapi.NewCallback(cq.ID, ""))
		return
	}
	b.api.Request(tgbotapi.NewCallback(cq.ID, "✅"))

	ctx := context.Background()
	checkout, err := services.GetCheckout(ctx, userID)
	if err != nil || checkout == nil || len(checkout.CartItems) == 0 {
		b.send(chatID, lang.T(b.getLang(userID), "please_add_order"))
		return
	}
	itemsTotal := checkout.ItemsTotal
	phone := checkout.Phone
	services.DeleteCheckout(ctx, userID)

	// Branch = restaurant; customer = shared delivery address. Distance = branch → customer (from memory or DB).
	branch, _ := services.GetUserLocation(ctx, userID)
	customerLat, customerLon, hasCustomer := b.getCustomerCoords(ctx, userID)

	locationID := int64(0)
	if branch != nil {
		locationID = branch.ID
	}
	baseFee := b.cfg.Delivery.BaseFee
	if baseFee < 0 {
		baseFee = 5000
	}
	ratePerKm := b.cfg.Delivery.RatePerKm
	if ratePerKm <= 0 {
		ratePerKm = 4000
	}
	var distanceKm float64
	var deliveryFee int64
	hasValidBranch := branch != nil && (branch.Lat != 0 || branch.Lon != 0)
	hasValidCustomer := hasCustomer && (customerLat != 0 || customerLon != 0)
	if hasValidBranch && hasValidCustomer && deliveryType == "delivery" {
		distanceKm = services.HaversineDistanceKm(branch.Lat, branch.Lon, customerLat, customerLon)
		rawFee := services.CalcDeliveryFee(distanceKm, baseFee, ratePerKm)
		deliveryFee = services.ApplyDeliveryFeeRule(rawFee)
	}
	if deliveryType == "pickup" {
		deliveryFee = 0
	}

	id, err := services.CreateOrder(ctx, models.CreateOrderInput{
		UserID:       userID,
		ChatID:       strconv.FormatInt(chatID, 10),
		Phone:        phone,
		Lat:          customerLat,
		Lon:          customerLon,
		DistanceKm:   distanceKm,
		DeliveryFee:  deliveryFee,
		ItemsTotal:   itemsTotal,
		LocationID:   locationID,
		DeliveryType: deliveryType,
	})
	if err != nil {
		b.sendLang(chatID, userID, "order_failed", err.Error())
		return
	}

	l := b.getLang(userID)
	confirmMsg := lang.T(l, "order_confirmed", id, phone, itemsTotal)
	confirmMsg += lang.T(l, "order_total", itemsTotal+deliveryFee)
	b.send(chatID, confirmMsg)

	o, _ := services.GetOrder(ctx, id)
	if o != nil {
		b.UpsertOrderCard(ctx, "customer", id, chatID, services.BuildCustomerCard(o, nil, ""))
	}
	hasUserLocation := customerLat != 0 || customerLon != 0
	b.notifyAdmin(ctx, id, branch, customerLat, customerLon, hasUserLocation)
}

func (b *Bot) notifyAdmin(ctx context.Context, orderID int64, location *models.Location, userLat, userLon float64, hasUserLocation bool) {
	if b.messageBot == nil {
		return
	}
	o, _ := services.GetOrder(ctx, orderID)
	if o == nil {
		return
	}
	var admins []services.BranchAdminWithLang
	if location != nil {
		var err error
		admins, err = services.GetBranchAdminsWithLang(ctx, location.ID)
		if err != nil {
			log.Printf("failed to get branch admins for location %d: %v", location.ID, err)
		}
	}
	if len(admins) == 0 && b.admin != 0 {
		orderLang, _ := services.GetAdminOrderLang(ctx, b.admin)
		admins = []services.BranchAdminWithLang{{AdminUserID: b.admin, OrderLang: orderLang}}
	}
	if len(admins) == 0 {
		log.Printf("warning: no branch admins for order #%d", orderID)
		return
	}
	first := admins[0]
	adminLang := first.OrderLang
	if adminLang == "" {
		adminLang = lang.Uz
	}
	content := services.BuildAdminCard(o, nil, adminLang)
	b.UpsertOrderCard(ctx, "admin", orderID, first.AdminUserID, content)
	if hasUserLocation {
		locMsg := tgbotapi.NewLocation(first.AdminUserID, userLat, userLon)
		_, _ = b.messageBot.Send(locMsg)
	}
}

// orderStatusKeyboard returns inline buttons for the given order status, localized to adminLang ("uz" or "ru").
func (b *Bot) orderStatusKeyboard(orderID int64, status string, deliveryType *string, adminLang string) tgbotapi.InlineKeyboardMarkup {
	if adminLang == "" {
		adminLang = lang.Uz
	}
	switch status {
	case services.OrderStatusNew:
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(lang.T(adminLang, "adm_start_preparing"), "order_status:"+strconv.FormatInt(orderID, 10)+":"+services.OrderStatusPreparing),
				tgbotapi.NewInlineKeyboardButtonData(lang.T(adminLang, "adm_reject"), "order_status:"+strconv.FormatInt(orderID, 10)+":"+services.OrderStatusRejected),
			),
		)
	case services.OrderStatusPreparing:
		return tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData(lang.T(adminLang, "adm_mark_ready"), "order_status:"+strconv.FormatInt(orderID, 10)+":"+services.OrderStatusReady),
			),
		)
	case services.OrderStatusReady:
		// If delivery_type not set, show two options
		if deliveryType == nil {
			return tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(lang.T(adminLang, "adm_send_delivery"), "delivery_type:"+strconv.FormatInt(orderID, 10)+":delivery"),
					tgbotapi.NewInlineKeyboardButtonData(lang.T(adminLang, "adm_customer_pickup"), "delivery_type:"+strconv.FormatInt(orderID, 10)+":pickup"),
				),
			)
		}
		// If pickup, show Mark Completed
		if *deliveryType == "pickup" {
			return tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(
					tgbotapi.NewInlineKeyboardButtonData(lang.T(adminLang, "adm_mark_completed"), "order_status:"+strconv.FormatInt(orderID, 10)+":"+services.OrderStatusCompleted),
				),
			)
		}
		// If delivery, no buttons (driver will accept)
		return tgbotapi.NewInlineKeyboardMarkup()
	case services.OrderStatusAssigned, services.OrderStatusPickedUp, services.OrderStatusDelivering:
		return tgbotapi.NewInlineKeyboardMarkup() // No buttons (driver will manage)
	case services.OrderStatusCompleted:
		return tgbotapi.NewInlineKeyboardMarkup() // no buttons
	default:
		return tgbotapi.NewInlineKeyboardMarkup()
	}
}

// startOrderStatusCallbacks runs the message bot update loop to handle order_status callbacks from restaurant admins.
func (b *Bot) startOrderStatusCallbacks() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.messageBot.GetUpdatesChan(u)
	for update := range updates {
		if update.CallbackQuery == nil {
			continue
		}
		cq := update.CallbackQuery
		data := cq.Data
		if strings.HasPrefix(data, "order_status:") {
			b.handleOrderStatusCallback(cq)
		}
	}
}

func (b *Bot) handleOrderStatusCallback(cq *tgbotapi.CallbackQuery) {
	parts := strings.SplitN(cq.Data, ":", 3)
	if len(parts) != 3 {
		b.messageBot.Request(tgbotapi.NewCallback(cq.ID, "Invalid callback."))
		return
	}
	orderID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || orderID <= 0 {
		b.messageBot.Request(tgbotapi.NewCallback(cq.ID, "Invalid order."))
		return
	}
	newStatus := parts[2]
	adminUserID := cq.From.ID

	ctx := context.Background()
	adminLocID, err := services.GetAdminLocationID(ctx, adminUserID)
	if err != nil || adminLocID == 0 {
		b.messageBot.Request(tgbotapi.NewCallback(cq.ID, "Unauthorized."))
		return
	}

	err = services.UpdateOrderStatus(ctx, orderID, newStatus, adminLocID, adminUserID)
	if err != nil {
		b.messageBot.Request(tgbotapi.NewCallback(cq.ID, err.Error()))
		log.Printf("order status update failed: order=%d status=%s admin=%d: %v", orderID, newStatus, adminUserID, err)
		return
	}
	b.AnswerCallbackQuery(cq.ID, "✅ Status updated.")
	b.RefreshOrderCards(ctx, orderID)
	if newStatus == services.OrderStatusReady {
		o, _ := services.GetOrder(ctx, orderID)
		if o != nil && o.DeliveryType != nil && *o.DeliveryType == "delivery" {
			go b.pushReadyOrderToDrivers(context.Background(), orderID)
		}
	}
}

// pushReadyOrderToDrivers finds nearby online drivers and sends them a Telegram message with an Accept button.
// Uses a 10s timeout. Claims pushed_at once; skips if already pushed within 60s. Aborts loop if order no longer available.
func (b *Bot) pushReadyOrderToDrivers(ctx context.Context, orderID int64) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	claimed, err := services.TrySetOrderPushedAt(ctx, orderID)
	if err != nil {
		log.Printf("push order to drivers: claim pushed_at failed order_id=%d: %v", orderID, err)
		return
	}
	if !claimed {
		within, _ := services.OrderPushedWithinSeconds(ctx, orderID, 60)
		if within {
			log.Printf("push order to drivers: order_id=%d skipped due to pushed_at (already pushed within 60s)", orderID)
		}
		return
	}

	o, err := services.GetOrder(ctx, orderID)
	if err != nil || o == nil || o.Status != services.OrderStatusReady {
		return
	}
	orderLat, orderLon, err := services.GetOrderCoordinates(ctx, orderID)
	if err != nil || (orderLat == 0 && orderLon == 0) {
		return
	}
	if b.driverBotAPI == nil {
		return
	}
	radiusKm := b.cfg.Delivery.DriverPushRadiusKm
	if radiusKm <= 0 {
		radiusKm = 5
	}
	drivers, err := services.GetNearbyOnlineDriversForOrder(ctx, orderLat, orderLon, radiusKm, 10)
	if err != nil {
		log.Printf("push order to drivers: get nearby drivers failed order_id=%d: %v", orderID, err)
		return
	}
	acceptData := "driver_accept:" + strconv.FormatInt(orderID, 10)
	// Include items total, delivery fee, and grand total so driver sees full price
	msgText := "📦 Yangi buyurtma yaqin atrofda!\n\nMasofa: %.2f km\nBuyurtma: %d so'm\nYetkazib berish: %d so'm\nJami: %d so'm\n\nQabul qilasizmi?"
	for _, d := range drivers {
		ok, err := services.OrderAvailableForPush(ctx, orderID)
		if err != nil || !ok {
			log.Printf("push order to drivers: order_id=%d skipped due to no longer available (status/assigned)", orderID)
			return
		}
		text := fmt.Sprintf(msgText, d.DistanceKm, o.ItemsTotal, o.DeliveryFee, o.GrandTotal)
		msg := tgbotapi.NewMessage(d.ChatID, text)
		msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("Accept Order #"+strconv.FormatInt(orderID, 10), acceptData),
			),
		)
		if _, sendErr := b.driverBotAPI.Send(msg); sendErr != nil {
			log.Printf("push order to driver: send failed order_id=%d driver_chat_id=%d: %v", orderID, d.ChatID, sendErr)
		} else {
			log.Printf("push order to driver: order_id=%d driver_chat_id=%d distance_km=%.2f", orderID, d.ChatID, d.DistanceKm)
		}
	}
}

func replaceOrderStatusInMessage(text, newStatusLabel string) string {
	const prefix = "Status: "
	start := strings.Index(text, prefix)
	if start == -1 {
		return text + "\n\nStatus: " + newStatusLabel
	}
	end := start + len(prefix)
	for end < len(text) && text[end] != '\n' {
		end++
	}
	return text[:start] + prefix + newStatusLabel + text[end:]
}

func (b *Bot) removeKeyboard(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = tgbotapi.NewRemoveKeyboard(true)
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) handleOverride(chatID int64, userID int64, text string) {
	if userID != b.admin {
		b.send(chatID, "Unauthorized.")
		return
	}
	parts := strings.Fields(text)
	if len(parts) < 3 {
		b.send(chatID, "Usage: /override <order_id> <new_delivery_fee> [note]")
		return
	}
	orderID, _ := strconv.ParseInt(parts[1], 10, 64)
	newFee, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || newFee < 0 {
		b.send(chatID, "Invalid order_id or new_fee.")
		return
	}
	note := ""
	if len(parts) > 3 {
		note = strings.Join(parts[3:], " ")
	}

	err = services.OverrideDeliveryFee(context.Background(), models.OverrideDeliveryFeeInput{
		OrderID:    orderID,
		NewFee:     newFee,
		OverrideBy: b.admin,
		Note:       note,
	})
	if err != nil {
		b.send(chatID, "Override failed: "+err.Error())
		return
	}
	b.send(chatID, fmt.Sprintf("✅ Order #%d delivery fee overridden to %d", orderID, newFee))
}

func (b *Bot) handleStats(chatID int64, userID int64, text string) {
	if userID != b.admin {
		b.send(chatID, "Unauthorized.")
		return
	}
	date := time.Now().Format("2006-01-02")
	parts := strings.Fields(text)
	if len(parts) > 1 {
		date = parts[1]
	}

	stats, err := services.GetDailyStats(context.Background(), date)
	if err != nil {
		b.send(chatID, "Stats failed: "+err.Error())
		return
	}

	msg := fmt.Sprintf(
		"📊 Stats (%s)\n\nOrders: %d\nItems revenue: %d\nDelivery revenue: %d\nGrand revenue: %d\nOverrides: %d",
		date, stats.OrdersCount, stats.ItemsRevenue, stats.DeliveryRevenue, stats.GrandRevenue, stats.OverridesCount,
	)
	b.send(chatID, msg)
}

// handlePromote handles the /promote command to add a branch admin
// Usage: /promote <branch_location_id> <new_admin_user_id>
func (b *Bot) handlePromote(chatID int64, userID int64, text string) {
	// Check if user is main admin
	if userID != b.admin {
		b.send(chatID, "❌ Unauthorized. Only main admin can promote branch admins.")
		return
	}

	parts := strings.Fields(text)
	if len(parts) < 4 {
		b.send(chatID, "📝 Usage: /promote <branch_location_id> <new_admin_user_id> <password> [uz|ru]\n\n"+
			"Example: /promote 1 123456789 MyUniquePass123 uz\n\n"+
			"The password must be unique. Last arg is the language for order notifications: uz (Uzbek) or ru (Russian). Default: uz.\n\n"+
			"💡 To get a user's ID, ask them to use @userinfobot on Telegram.")
		return
	}

	branchLocationID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || branchLocationID <= 0 {
		b.send(chatID, "❌ Invalid branch location ID. Must be a positive number.")
		log.Printf("invalid branch location ID provided: %s", parts[1])
		return
	}

	newAdminID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || newAdminID <= 0 {
		b.send(chatID, "❌ Invalid admin user ID. Must be a positive number.\n\n"+
			"💡 To get a user's ID, ask them to use @userinfobot on Telegram.")
		log.Printf("invalid admin user ID provided: %s", parts[2])
		return
	}

	orderLang := "uz"
	if len(parts) >= 5 && (parts[4] == "ru" || parts[4] == "uz") {
		orderLang = parts[4]
	}
	// Password is parts[3]; if 5th part is uz/ru, password is just parts[3]; else password is parts[3:] (e.g. "My Pass Word")
	password := strings.TrimSpace(parts[3])
	if len(parts) > 4 && (parts[4] == "uz" || parts[4] == "ru") {
		password = strings.TrimSpace(parts[3])
	} else if len(parts) > 4 {
		password = strings.TrimSpace(strings.Join(parts[3:], " "))
	}
	if password == "" {
		b.send(chatID, "❌ Password cannot be empty.")
		return
	}

	passwordHash, err := services.HashBranchAdminPassword(password)
	if err != nil {
		b.send(chatID, "❌ Failed to set password: "+err.Error())
		return
	}

	ctx := context.Background()
	err = services.AddBranchAdmin(ctx, branchLocationID, newAdminID, userID, passwordHash, orderLang)
	if err != nil {
		b.send(chatID, "❌ Failed to promote admin: "+err.Error())
		log.Printf("failed to promote admin %d for branch %d by admin %d: %v", newAdminID, branchLocationID, userID, err)
		return
	}

	// Get branch name for confirmation
	var branchName string
	err = db.Pool.QueryRow(ctx, `SELECT name FROM locations WHERE id = $1`, branchLocationID).Scan(&branchName)
	if err != nil {
		branchName = fmt.Sprintf("Branch #%d", branchLocationID)
	}

	b.send(chatID, fmt.Sprintf("✅ Successfully promoted user %d as admin for branch '%s' (ID: %d)", newAdminID, branchName, branchLocationID))
	log.Printf("admin %d promoted user %d as admin for branch %d (%s)", userID, newAdminID, branchLocationID, branchName)
}

// handleListAdmins lists all admins for a branch
// Usage: /list_admins <branch_location_id>
func (b *Bot) handleListAdmins(chatID int64, userID int64, text string) {
	ctx := context.Background()
	// Safety net: if migrations weren't applied, create the table on-demand.
	if err := services.EnsureBranchAdminsTable(ctx); err != nil {
		b.send(chatID, "❌ DB error: "+err.Error())
		log.Printf("ensure branch_admins table: %v", err)
		return
	}

	// Check if user is main admin or branch admin
	if userID != b.admin {
		// Check if user is a branch admin for any branch
		rows, err := db.Pool.Query(ctx, `SELECT branch_location_id FROM branch_admins WHERE admin_user_id = $1 LIMIT 1`, userID)
		if err != nil || !rows.Next() {
			b.send(chatID, "❌ Unauthorized. Only admins can list branch admins.")
			if rows != nil {
				rows.Close()
			}
			return
		}
		rows.Close()
	}

	parts := strings.Fields(text)
	if len(parts) < 2 {
		b.send(chatID, "📝 Usage: /list_admins <branch_location_id>")
		return
	}

	branchLocationID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || branchLocationID <= 0 {
		b.send(chatID, "❌ Invalid branch location ID. Must be a positive number.")
		return
	}

	admins, err := services.ListBranchAdmins(ctx, branchLocationID)
	if err != nil {
		b.send(chatID, "❌ Failed to list admins: "+err.Error())
		log.Printf("failed to list admins for branch %d: %v", branchLocationID, err)
		return
	}

	if len(admins) == 0 {
		b.send(chatID, fmt.Sprintf("ℹ️ No admins found for branch location ID %d.", branchLocationID))
		return
	}

	// Get branch name
	var branchName string
	err = db.Pool.QueryRow(ctx, `SELECT name FROM locations WHERE id = $1`, branchLocationID).Scan(&branchName)
	if err != nil {
		branchName = fmt.Sprintf("Branch #%d", branchLocationID)
	}

	msg := fmt.Sprintf("👥 Admins for '%s' (ID: %d):\n\n", branchName, branchLocationID)
	for i, admin := range admins {
		msg += fmt.Sprintf("%d. User ID: %d\n   Promoted by: %d\n   Promoted at: %s\n\n", i+1, admin.AdminUserID, admin.PromotedBy, admin.PromotedAt)
	}
	b.send(chatID, msg)
	log.Printf("listed %d admins for branch %d by user %d", len(admins), branchLocationID, userID)
}

// handleRemoveAdmin removes an admin from a branch
// Usage: /remove_admin <branch_location_id> <admin_user_id>
func (b *Bot) handleRemoveAdmin(chatID int64, userID int64, text string) {
	// Check if user is main admin
	if userID != b.admin {
		b.send(chatID, "❌ Unauthorized. Only main admin can remove branch admins.")
		return
	}

	parts := strings.Fields(text)
	if len(parts) < 3 {
		b.send(chatID, "📝 Usage: /remove_admin <branch_location_id> <admin_user_id>")
		return
	}

	branchLocationID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || branchLocationID <= 0 {
		b.send(chatID, "❌ Invalid branch location ID. Must be a positive number.")
		return
	}

	adminID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || adminID <= 0 {
		b.send(chatID, "❌ Invalid admin user ID. Must be a positive number.")
		return
	}

	ctx := context.Background()
	// Safety net: if migrations weren't applied, create the table on-demand.
	if err := services.EnsureBranchAdminsTable(ctx); err != nil {
		b.send(chatID, "❌ DB error: "+err.Error())
		log.Printf("ensure branch_admins table: %v", err)
		return
	}
	err = services.RemoveBranchAdmin(ctx, branchLocationID, adminID)
	if err != nil {
		b.send(chatID, "❌ Failed to remove admin: "+err.Error())
		log.Printf("failed to remove admin %d from branch %d by admin %d: %v", adminID, branchLocationID, userID, err)
		return
	}

	// Get branch name
	var branchName string
	err = db.Pool.QueryRow(ctx, `SELECT name FROM locations WHERE id = $1`, branchLocationID).Scan(&branchName)
	if err != nil {
		branchName = fmt.Sprintf("Branch #%d", branchLocationID)
	}

	b.send(chatID, fmt.Sprintf("✅ Successfully removed user %d as admin from branch '%s' (ID: %d)", adminID, branchName, branchLocationID))
	log.Printf("admin %d removed user %d as admin from branch %d (%s)", userID, adminID, branchLocationID, branchName)
}
