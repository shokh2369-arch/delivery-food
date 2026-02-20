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
	api        *tgbotapi.BotAPI
	messageBot *tgbotapi.BotAPI // bot for sending order notifications (MESSAGE_TOKEN)
	cfg        *config.Config
	admin      int64

	locSuggestions   map[int64][]services.LocationWithDistance
	locSuggestionsMu sync.RWMutex

	userSharedCoords   map[int64]struct{ Lat, Lon float64 }
	userSharedCoordsMu sync.RWMutex

	userLang   map[int64]string // "uz" or "ru"
	userLangMu sync.RWMutex
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

func (b *Bot) Start() {
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
	defer b.userLangMu.RUnlock()
	l := b.userLang[userID]
	// Return "" if user has not chosen a language yet (so /start shows language selection)
	if l == "" || (l != lang.Uz && l != lang.Ru) {
		return ""
	}
	return l
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
	// Always show language selection on /start
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
	// Try to load user's selected location; if not found, fall back to global menu
	if loc, err := services.GetUserLocation(ctx, userID); err == nil && loc != nil {
		items, err = services.ListMenuByCategoryAndLocation(ctx, category, loc.ID)
		if err != nil {
			log.Printf("list menu by location: %v", err)
			items = nil
		}
	} else {
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
		langCode := strings.TrimPrefix(data, "lang:")
		b.showWelcomeWithLocation(chatID, userID, langCode)
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

	// Compute delivery fee: distance from branch to user, then apply rule (< 500 -> 0, >= 500 -> 1000)
	var deliveryFee int64
	userLocation, _ := services.GetUserLocation(ctx, userID)
	b.userSharedCoordsMu.RLock()
	coords, hasCoords := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if userLocation != nil && hasCoords {
		distanceKm := services.HaversineDistanceKm(userLocation.Lat, userLocation.Lon, coords.Lat, coords.Lon)
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
	locs, err := services.ListLocations(ctx)
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

	// Store user's shared coordinates
	b.userSharedCoordsMu.Lock()
	b.userSharedCoords[userID] = struct{ Lat, Lon float64 }{Lat: lat, Lon: lon}
	b.userSharedCoordsMu.Unlock()

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
	locs, err := services.ListLocations(ctx)
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

func (b *Bot) handleContact(chatID int64, userID int64, phone string, customerUsername string) {
	// Verify user has shared location before allowing order creation
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
	checkout, err := services.GetCheckout(ctx, userID)
	if err != nil || checkout == nil || len(checkout.CartItems) == 0 {
		b.removeKeyboard(chatID, lang.T(l, "please_add_order"))
		return
	}
	itemsTotal := checkout.ItemsTotal
	// Save items for admin notification before deleting checkout
	items := make([]cartItem, len(checkout.CartItems))
	for i, sci := range checkout.CartItems {
		items[i] = serviceToCartItem(sci)
	}
	services.DeleteCheckout(ctx, userID)

	// Get user's selected location for order and notification
	var userLocation *models.Location
	if loc, err := services.GetUserLocation(ctx, userID); err == nil && loc != nil {
		userLocation = loc
	}

	// Get user's shared coordinates
	var userLat, userLon float64
	var hasUserLocation bool
	b.userSharedCoordsMu.RLock()
	if coords, ok := b.userSharedCoords[userID]; ok {
		userLat = coords.Lat
		userLon = coords.Lon
		hasUserLocation = true
	}
	b.userSharedCoordsMu.RUnlock()

	locationID := int64(0)
	if userLocation != nil {
		locationID = userLocation.ID
	}
	// Taxi-style delivery fee: 5000 start + 4000 per km
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
	if hasUserLocation && userLocation != nil {
		distanceKm = services.HaversineDistanceKm(userLocation.Lat, userLocation.Lon, userLat, userLon)
		rawFee := services.CalcDeliveryFee(distanceKm, baseFee, ratePerKm)
		deliveryFee = services.ApplyDeliveryFeeRule(rawFee)
	}
	id, err := services.CreateOrder(ctx, models.CreateOrderInput{
		UserID:      userID,
		ChatID:      strconv.FormatInt(chatID, 10),
		Phone:       phone,
		Lat:         userLat,
		Lon:         userLon,
		DistanceKm:  distanceKm,
		DeliveryFee: deliveryFee,
		ItemsTotal:  itemsTotal,
		LocationID:  locationID,
	})
	if err != nil {
		b.sendLang(chatID, userID, "order_failed", err.Error())
		return
	}

	confirmMsg := lang.T(l, "order_confirmed", id, phone, itemsTotal)
	confirmMsg += lang.T(l, "order_total", itemsTotal+deliveryFee)
	b.removeKeyboard(chatID, confirmMsg)

	// Send order card only to that restaurant's admin (with status buttons)
	b.notifyAdmin(id, phone, items, itemsTotal, deliveryFee, distanceKm, customerUsername, userLocation, userLat, userLon, hasUserLocation)
}

func (b *Bot) notifyAdmin(orderID int64, phone string, items []cartItem, itemsTotal, deliveryFee int64, distanceKm float64, customerUsername string, location *models.Location, userLat, userLon float64, hasUserLocation bool) {
	if b.messageBot == nil {
		return // MESSAGE_TOKEN not set or failed to initialize
	}

	ctx := context.Background()
	// Get branch admins with their preferred order language
	var admins []services.BranchAdminWithLang
	if location != nil {
		var err error
		admins, err = services.GetBranchAdminsWithLang(ctx, location.ID)
		if err != nil {
			log.Printf("failed to get branch admins for location %d: %v", location.ID, err)
		}
	}
	// Fallback to main admin if no branch admins
	if len(admins) == 0 && b.admin != 0 {
		orderLang, _ := services.GetAdminOrderLang(ctx, b.admin)
		admins = []services.BranchAdminWithLang{{AdminUserID: b.admin, OrderLang: orderLang}}
	}
	if len(admins) == 0 {
		log.Printf("warning: no branch admins for order #%d", orderID)
		return
	}

	// Send localized order card to each admin
	for _, admin := range admins {
		adminLang := admin.OrderLang
		if adminLang == "" {
			adminLang = lang.Uz
		}

		// Build order card text in admin's language
		customerLabel := lang.T(adminLang, "adm_customer_no_user")
		if customerUsername != "" {
			customerLabel = fmt.Sprintf(lang.T(adminLang, "adm_customer"), customerUsername)
		}
		text := fmt.Sprintf(lang.T(adminLang, "adm_new_order"), orderID) + "\n" + customerLabel + "\n\n" + lang.T(adminLang, "adm_items") + "\n"
		for _, it := range items {
			text += fmt.Sprintf("- %s x%d\n", it.Name, it.Qty)
		}
		text += fmt.Sprintf("\n%s\n\n%s", fmt.Sprintf(lang.T(adminLang, "adm_total"), itemsTotal), fmt.Sprintf(lang.T(adminLang, "adm_status"), lang.T(adminLang, "adm_status_new")))

		// Buttons in admin's language
		kb := b.orderStatusKeyboard(orderID, services.OrderStatusNew, nil, adminLang)

		msg := tgbotapi.NewMessage(admin.AdminUserID, text)
		msg.ReplyMarkup = kb
		sentMsg, err := b.messageBot.Send(msg)
		if err != nil {
			log.Printf("failed to send order card to admin %d: %v", admin.AdminUserID, err)
			continue
		}
		// Store admin message ID for this order
		if err := services.SetAdminMessageID(ctx, orderID, admin.AdminUserID, sentMsg.MessageID); err != nil {
			log.Printf("failed to store admin message ID for order %d: %v", orderID, err)
		}
		if hasUserLocation {
			locMsg := tgbotapi.NewLocation(admin.AdminUserID, userLat, userLon)
			_, _ = b.messageBot.Send(locMsg)
		}
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
		} else if strings.HasPrefix(data, "delivery_type:") {
			b.handleDeliveryTypeCallback(cq)
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
		// 403 / 400: return error to admin via callback
		b.messageBot.Request(tgbotapi.NewCallback(cq.ID, err.Error()))
		log.Printf("order status update failed: order=%d status=%s admin=%d: %v", orderID, newStatus, adminUserID, err)
		return
	}
	b.messageBot.Request(tgbotapi.NewCallback(cq.ID, "✅ Status updated."))

	// Get admin's preferred order language
	adminLang, _ := services.GetAdminOrderLang(ctx, adminUserID)
	if adminLang == "" {
		adminLang = lang.Uz
	}

	// Edit admin message: replace Status line and update keyboard
	var statusLabel string
	switch newStatus {
	case services.OrderStatusNew:
		statusLabel = lang.T(adminLang, "adm_status_new")
	case services.OrderStatusPreparing:
		statusLabel = lang.T(adminLang, "adm_status_preparing")
	case services.OrderStatusReady:
		statusLabel = lang.T(adminLang, "adm_status_ready")
	case services.OrderStatusAssigned:
		statusLabel = lang.T(adminLang, "adm_status_assigned")
	case services.OrderStatusPickedUp:
		statusLabel = lang.T(adminLang, "adm_status_picked_up")
	case services.OrderStatusDelivering:
		statusLabel = lang.T(adminLang, "adm_status_delivering")
	case services.OrderStatusCompleted:
		statusLabel = lang.T(adminLang, "adm_status_completed")
	case services.OrderStatusRejected:
		statusLabel = lang.T(adminLang, "adm_status_rejected")
	default:
		statusLabel = strings.ToUpper(newStatus)
	}
	newText := replaceOrderStatusInMessage(cq.Message.Text, fmt.Sprintf(lang.T(adminLang, "adm_status"), statusLabel))
	// Get order to check delivery_type
	o, _ := services.GetOrder(ctx, orderID)
	var deliveryType *string
	if o != nil {
		deliveryType = o.DeliveryType
	}
	kb := b.orderStatusKeyboard(orderID, newStatus, deliveryType, adminLang)
	edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, newText)
	// Only set ReplyMarkup if keyboard has buttons (Telegram doesn't accept empty keyboards)
	if len(kb.InlineKeyboard) > 0 {
		edit.ReplyMarkup = &kb
		if _, err := b.messageBot.Send(edit); err != nil {
			log.Printf("edit order message: %v", err)
		}
	} else {
		// Remove keyboard by editing with explicitly empty keyboard array
		emptyKb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
		editRemoveKb := tgbotapi.NewEditMessageReplyMarkup(cq.Message.Chat.ID, cq.Message.MessageID, emptyKb)
		if _, err := b.messageBot.Send(editRemoveKb); err != nil {
			log.Printf("remove keyboard from order message: %v", err)
		}
		// Then send text edit without keyboard
		if _, err := b.messageBot.Send(edit); err != nil {
			log.Printf("edit order message: %v", err)
		}
	}

	// Notify customer (preparing / ready / completed / rejected) after DB commit; de-dup within 30s
	if newStatus == services.OrderStatusPreparing || newStatus == services.OrderStatusReady || newStatus == services.OrderStatusCompleted || newStatus == services.OrderStatusRejected {
		o, _ := services.GetOrder(ctx, orderID)
		if o != nil && o.ChatID != "" {
			skip, _ := services.SentOrderStatusNotifyWithin30s(ctx, orderID, newStatus)
			if !skip {
				text := services.CustomerMessageForOrderStatus(o, newStatus)
				if chatID, err := strconv.ParseInt(o.ChatID, 10, 64); err == nil {
					msg := tgbotapi.NewMessage(chatID, text)
					if _, sendErr := b.api.Send(msg); sendErr != nil {
						log.Printf("send customer order status notify: %v", sendErr)
					} else {
						_ = services.SaveOutboundMessage(ctx, chatID, text, map[string]interface{}{
							"channel":  "telegram",
							"sent_via": "order_status_notify",
							"order_id": orderID,
							"status":   newStatus,
						})
					}
				}
			}
		}
	}
}

func (b *Bot) handleDeliveryTypeCallback(cq *tgbotapi.CallbackQuery) {
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
	deliveryType := parts[2]
	if deliveryType != "pickup" && deliveryType != "delivery" {
		b.messageBot.Request(tgbotapi.NewCallback(cq.ID, "Invalid delivery type."))
		return
	}
	adminUserID := cq.From.ID

	ctx := context.Background()
	adminLocID, err := services.GetAdminLocationID(ctx, adminUserID)
	if err != nil || adminLocID == 0 {
		b.messageBot.Request(tgbotapi.NewCallback(cq.ID, "Unauthorized."))
		return
	}

	err = services.SetDeliveryType(ctx, orderID, deliveryType, adminLocID)
	if err != nil {
		b.messageBot.Request(tgbotapi.NewCallback(cq.ID, err.Error()))
		log.Printf("set delivery type failed: order=%d type=%s admin=%d: %v", orderID, deliveryType, adminUserID, err)
		return
	}

	var msgText string
	if deliveryType == "pickup" {
		msgText = "✅ Buyurtma mijoz o'zi olib ketish uchun belgilandi.\n\nEndi \"Mark Completed\" tugmasini bosing."
		b.messageBot.Request(tgbotapi.NewCallback(cq.ID, "✅ Customer Pickup selected."))
	} else {
		msgText = "✅ Buyurtma driverga uzatildi, driver qabul qilishi kutilmoqda."
		b.messageBot.Request(tgbotapi.NewCallback(cq.ID, "✅ Sent to Delivery."))
		// Log order details for debugging driver visibility
		o, _ := services.GetOrder(ctx, orderID)
		if o != nil {
			var lat, lon float64
			var hasLocation bool
			err := db.Pool.QueryRow(ctx, `SELECT lat, lon FROM orders WHERE id = $1`, orderID).Scan(&lat, &lon)
			if err == nil {
				hasLocation = lat != 0 && lon != 0
			}
			log.Printf("order sent to delivery: order_id=%d status=%s delivery_type=%s has_location=%v lat=%.6f lon=%.6f driver_visible=%v",
				orderID, o.Status, deliveryType, hasLocation, lat, lon, hasLocation)
		}
	}

	// Get admin's preferred order language
	adminLang, _ := services.GetAdminOrderLang(ctx, adminUserID)
	if adminLang == "" {
		adminLang = lang.Uz
	}

	// Edit admin message: update keyboard
	o, _ := services.GetOrder(ctx, orderID)
	if o == nil {
		return
	}
	deliveryTypePtr := &deliveryType
	kb := b.orderStatusKeyboard(orderID, o.Status, deliveryTypePtr, adminLang)
	edit := tgbotapi.NewEditMessageText(cq.Message.Chat.ID, cq.Message.MessageID, cq.Message.Text+"\n\n"+msgText)
	// Only set ReplyMarkup if keyboard has buttons (Telegram doesn't accept empty keyboards)
	if len(kb.InlineKeyboard) > 0 {
		edit.ReplyMarkup = &kb
		if _, err := b.messageBot.Send(edit); err != nil {
			log.Printf("edit order message: %v", err)
		}
	} else {
		// Remove keyboard by editing with explicitly empty keyboard array
		emptyKb := tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}}
		editRemoveKb := tgbotapi.NewEditMessageReplyMarkup(cq.Message.Chat.ID, cq.Message.MessageID, emptyKb)
		if _, err := b.messageBot.Send(editRemoveKb); err != nil {
			log.Printf("remove keyboard from order message: %v", err)
		}
		// Then send text edit without keyboard
		if _, err := b.messageBot.Send(edit); err != nil {
			log.Printf("edit order message: %v", err)
		}
	}

	// If pickup, notify customer immediately
	if deliveryType == "pickup" {
		o, _ := services.GetOrder(ctx, orderID)
		if o != nil && o.ChatID != "" {
			skip, _ := services.SentOrderStatusNotifyWithin30s(ctx, orderID, services.OrderStatusReady)
			if !skip {
				text := services.CustomerMessageForOrderStatus(o, services.OrderStatusReady)
				if chatID, err := strconv.ParseInt(o.ChatID, 10, 64); err == nil {
					msg := tgbotapi.NewMessage(chatID, text)
					if _, sendErr := b.api.Send(msg); sendErr != nil {
						log.Printf("send customer order status notify: %v", sendErr)
					} else {
						_ = services.SaveOutboundMessage(ctx, chatID, text, map[string]interface{}{
							"channel":  "telegram",
							"sent_via": "order_status_notify",
							"order_id": orderID,
							"status":   services.OrderStatusReady,
						})
					}
				}
			}
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
