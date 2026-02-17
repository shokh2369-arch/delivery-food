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

	userSharedCoords   map[int64]struct{ Lat, Lon float64 } // Store user's shared location coordinates
	userSharedCoordsMu sync.RWMutex
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

func (b *Bot) Start() {
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
			// Check if user has shared location before showing menu
			b.userSharedCoordsMu.RLock()
			_, hasLocation := b.userSharedCoords[userID]
			b.userSharedCoordsMu.RUnlock()
			if !hasLocation {
				b.send(msg.Chat.ID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
				b.handleStart(msg.Chat.ID, userID)
			} else {
				b.sendMenu(msg.Chat.ID, userID)
			}
		case msg.Contact != nil:
			b.handleContact(msg.Chat.ID, userID, msg.Contact.PhoneNumber)
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

func (b *Bot) sendWithInline(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) handleStart(chatID int64, userID int64) {
	// Require location sharing before proceeding
	kb := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButtonLocation("📍 Lokatsiyani ulashish"),
		),
	)
	kb.OneTimeKeyboard = true
	kb.ResizeKeyboard = true

	msg := tgbotapi.NewMessage(chatID, "Xush kelibsiz! Buyurtma berish uchun lokatsiyangizni ulashing. Iltimos, \"📍 Lokatsiyani ulashish\" tugmasini bosing.")
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) categoryKeyboard() tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🍽 Yeguliklar", "cat:food"),
			tgbotapi.NewInlineKeyboardButtonData("🥤 Ichimliklar", "cat:drink"),
			tgbotapi.NewInlineKeyboardButtonData("🍰 Kekslar", "cat:dessert"),
		},
		{tgbotapi.NewInlineKeyboardButtonData("« Back", "back")},
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (b *Bot) menuKeyboard(userID int64, category string) tgbotapi.InlineKeyboardMarkup {
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
			tgbotapi.NewInlineKeyboardButtonData("✅ Confirm order", "confirm"),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("« Back to categories", "back_cats"),
	))

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (b *Bot) sendMenu(chatID int64, userID int64) {
	// Verify user has shared location
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.send(chatID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
		b.handleStart(chatID, userID)
		return
	}

	ctx := context.Background()
	cart, _ := b.getCart(ctx, userID)

	text := "📋 *Menu*\n\nKategoriyani tanlang: Yeguliklar, Ichimliklar yoki Kekslar."
	if cart != nil && len(cart.Items) > 0 {
		text += "\n\n🛒 *Savatchangiz:*\n"
		for _, it := range cart.Items {
			text += fmt.Sprintf("• %s × %d — %d\n", it.Name, it.Qty, it.Price*int64(it.Qty))
		}
		text += fmt.Sprintf("\n*Jami: %d*", cart.ItemsTotal)
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = b.categoryKeyboard()
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) sendCategoryMenu(chatID int64, userID int64, category string) {
	// Verify user has shared location
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.send(chatID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
		b.handleStart(chatID, userID)
		return
	}

	ctx := context.Background()
	cart, _ := b.getCart(ctx, userID)

	catLabel := map[string]string{"food": "Yeguliklar", "drink": "Ichimliklar", "dessert": "Kekslar"}[category]
	text := fmt.Sprintf("📋 *%s*\n\nBu kategoriyadagi maxsulotlarni tanlang.", catLabel)
	if cart != nil && len(cart.Items) > 0 {
		text += "\n\n🛒 *Savatchangiz:*\n"
		for _, it := range cart.Items {
			text += fmt.Sprintf("• %s × %d — %d\n", it.Name, it.Qty, it.Price*int64(it.Qty))
		}
		text += fmt.Sprintf("\n*Jami: %d*", cart.ItemsTotal)
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = b.menuKeyboard(userID, category)
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
	case data == "menu":
		// Check if user has shared location before showing menu
		b.userSharedCoordsMu.RLock()
		_, hasLocation := b.userSharedCoords[userID]
		b.userSharedCoordsMu.RUnlock()
		if !hasLocation {
			b.send(chatID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
			b.handleStart(chatID, userID)
		} else {
			b.sendMenu(chatID, userID)
		}
	case strings.HasPrefix(data, "locsel:"):
		// User selected a specific fast food location
		idStr := strings.TrimPrefix(data, "locsel:")
		locID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || locID <= 0 {
			b.send(chatID, "Noto'g'ri filial tanlandi.")
			return
		}
		ctx := context.Background()
		if err := services.SetUserLocation(ctx, userID, locID); err != nil {
			b.send(chatID, "Filialni saqlashda xatolik yuz berdi.")
			return
		}
		// Confirm selection and show button to open menu for this location
		msgText := "✅ Filial tanlandi. Endi menyuni ko'rishingiz mumkin."
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("📋Menyuni ko'rish", "menu"),
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
		b.handleStart(chatID, userID)
	case data == "back_cats":
		// Check location before showing menu
		b.userSharedCoordsMu.RLock()
		_, hasLocation := b.userSharedCoords[userID]
		b.userSharedCoordsMu.RUnlock()
		if !hasLocation {
			b.send(chatID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
			b.handleStart(chatID, userID)
		} else {
			b.sendMenu(chatID, userID)
		}
	case strings.HasPrefix(data, "cat:"):
		// Check location before showing category menu
		b.userSharedCoordsMu.RLock()
		_, hasLocation := b.userSharedCoords[userID]
		b.userSharedCoordsMu.RUnlock()
		if !hasLocation {
			b.send(chatID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
			b.handleStart(chatID, userID)
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
	case strings.HasPrefix(data, "suggest:"):
		// Check location before showing suggestions
		b.userSharedCoordsMu.RLock()
		_, hasLocation := b.userSharedCoords[userID]
		b.userSharedCoordsMu.RUnlock()
		if !hasLocation {
			b.send(chatID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
			b.handleStart(chatID, userID)
		} else {
			cat := strings.TrimPrefix(data, "suggest:")
			if cat == "food" || cat == "drink" || cat == "dessert" {
				b.sendCategoryMenu(chatID, userID, cat)
			}
		}
	}
}

func (b *Bot) addToCart(chatID int64, userID int64, itemID string, category string, editMsgID int) {
	// Verify user has shared location before adding to cart
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.send(chatID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
		b.handleStart(chatID, userID)
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

	catLabel := map[string]string{"food": "Yeguliklar", "drink": "Ichimliklar", "dessert": "Kekslar"}[category]
	text := fmt.Sprintf("📋 *%s*\n\nMaxsulot qo'shildi.\n\n🛒 *Savatchangiz:*\n", catLabel)
	for _, it := range cart.Items {
		text += fmt.Sprintf("• %s × %d — %d\n", it.Name, it.Qty, it.Price*int64(it.Qty))
	}
	text += fmt.Sprintf("\n*Jami: %d*\n\nBuyurtmani tasdiqlash uchun *Tasdiqlash* tugmasini bosing.", cart.ItemsTotal)

	edit := tgbotapi.NewEditMessageText(chatID, editMsgID, text)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: b.menuKeyboard(userID, category).InlineKeyboard}
	if _, err := b.api.Send(edit); err != nil {
		log.Printf("edit error: %v", err)
	}
}

// sendSuggestionScreen shows "Add something more?" with inline: missing categories (Drinks, Desserts, Foods) + Confirm order.
func (b *Bot) sendSuggestionScreen(chatID int64, userID int64) {
	// Verify user has shared location
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.send(chatID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
		b.handleStart(chatID, userID)
		return
	}

	ctx := context.Background()
	cart, err := b.getCart(ctx, userID)
	if err != nil || cart == nil || len(cart.Items) == 0 {
		b.send(chatID, "Sizning savatchangiz bo'sh. Iltimos, avval buyurtma qo'shing.")
		b.handleStart(chatID, userID)
		return
	}
	// Which categories are already in the cart?
	hasCategory := map[string]bool{}
	for _, it := range cart.Items {
		if it.Category != "" {
			hasCategory[it.Category] = true
		}
	}

	var row []tgbotapi.InlineKeyboardButton
	if !hasCategory["drink"] {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🥤 Ichimliklar", "suggest:drink"))
	}
	if !hasCategory["dessert"] {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🍰 Kekslar", "suggest:dessert"))
	}
	if !hasCategory["food"] {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("🍽 Yeguliklar", "suggest:food"))
	}
	row = append(row, tgbotapi.NewInlineKeyboardButtonData("✅ Buyurtmani tasdiqlash", "confirm_final"))

	kb := tgbotapi.NewInlineKeyboardMarkup(row)
	text := "🛒 *Your order*\n\n"
	for _, it := range cart.Items {
		text += fmt.Sprintf("• %s × %d — %d\n", it.Name, it.Qty, it.Price*int64(it.Qty))
	}
	text += fmt.Sprintf("\n*Jami: %d*\n\nYana narsa qo'shish yoki buyurtmani tasdiqlash.", cart.ItemsTotal)

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

func (b *Bot) requestPhone(chatID int64, userID int64) {
	// Verify user has shared location before requesting phone
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.send(chatID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
		b.handleStart(chatID, userID)
		return
	}

	ctx := context.Background()
	cart, err := b.getCart(ctx, userID)
	if err != nil || cart == nil || len(cart.Items) == 0 {
		b.send(chatID, "Sizning savatchangiz bo'sh. Iltimos, avval buyurtma qo'shing.")
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
			tgbotapi.NewKeyboardButtonContact("📱Raqamingizni ulashish"),
		),
	)
	kb.OneTimeKeyboard = true
	kb.ResizeKeyboard = true

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🛒 Buyurtma jami: %d\n\n📱 Iltimos, raqamingizni ulashing.", checkout.ItemsTotal))
	msg.ReplyMarkup = kb
	if _, err := b.api.Send(msg); err != nil {
		log.Printf("send error: %v", err)
	}
}

// handleUserLocation is called when the user shares their own location on the main bot.
// It loads all configured fast food locations and shows the nearest ones (5 per page).
func (b *Bot) handleUserLocation(chatID int64, userID int64, lat, lon float64) {
	ctx := context.Background()
	locs, err := services.ListLocations(ctx)
	if err != nil {
		b.removeKeyboard(chatID, "Joylashuvlar ro'yxatini yuklashda xatolik yuz berdi.")
		return
	}
	if len(locs) == 0 {
		// Even if no branches exist, user has shared location, so allow menu access
		b.removeKeyboard(chatID, "Hozircha fast food joylari ro'yxatiga qo'shilmagan. Lekin siz menyuni ko'rishingiz mumkin.")
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
		if fromCallback {
			b.send(chatID, "Hozircha joylashuvlar topilmadi.")
		} else {
			b.removeKeyboard(chatID, "Hozircha joylashuvlar topilmadi.")
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

	text := "📍 Sizga eng yaqin fast food joylari:\n\n"
	var buttons [][]tgbotapi.InlineKeyboardButton
	for i := start; i < end; i++ {
		l := list[i]
		text += fmt.Sprintf("%d) %s — %.1f km\n", i+1, l.Location.Name, l.Distance)
		// One button per location to select it
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("%d) %s", i+1, l.Location.Name),
				fmt.Sprintf("locsel:%d", l.Location.ID),
			),
		))
	}
	text += "\nFilialni tanlash uchun yuqoridagi tugmalardan birini bosing.\nSahifalar orasida o'tish uchun quyidagi tugmalardan foydalaning."

	var row []tgbotapi.InlineKeyboardButton
	if page > 0 {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("« Oldingi", fmt.Sprintf("loc:page:%d", page-1)))
	}
	if end < len(list) {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("Keyingi »", fmt.Sprintf("loc:page:%d", page+1)))
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
	if len(locs) == 0 {
		b.removeKeyboard(chatID, "Hozircha fast food joylari ro'yxatiga qo'shilmagan.")
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

	text := "📍 Fast food joylari ro'yxati:\n\n"
	var buttons [][]tgbotapi.InlineKeyboardButton
	for i := start; i < end; i++ {
		l := locs[i]
		text += fmt.Sprintf("%d) %s\n", i+1, l.Name)
		buttons = append(buttons, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(
				fmt.Sprintf("%d) %s", i+1, l.Name),
				fmt.Sprintf("locsel:%d", l.ID),
			),
		))
	}
	text += "\nFilialni tanlash uchun yuqoridagi tugmalardan birini bosing.\nSahifalar orasida o'tish uchun quyidagi tugmalardan foydalaning."

	var buttonsNav [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	if page > 0 {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("« Oldingi", fmt.Sprintf("locm:page:%d", page-1)))
	}
	if end < len(locs) {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("Keyingi »", fmt.Sprintf("locm:page:%d", page+1)))
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

func (b *Bot) handleContact(chatID int64, userID int64, phone string) {
	// Verify user has shared location before allowing order creation
	b.userSharedCoordsMu.RLock()
	_, hasLocation := b.userSharedCoords[userID]
	b.userSharedCoordsMu.RUnlock()
	if !hasLocation {
		b.removeKeyboard(chatID, "Iltimos, avval lokatsiyangizni ulashing. Buyurtma berish uchun lokatsiya majburiydir.")
		b.handleStart(chatID, userID)
		return
	}

	ctx := context.Background()
	checkout, err := services.GetCheckout(ctx, userID)
	if err != nil || checkout == nil || len(checkout.CartItems) == 0 {
		b.removeKeyboard(chatID, "Iltimos, avval buyurtma qo'shing.")
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

	id, err := services.CreateOrder(ctx, models.CreateOrderInput{
		UserID:      userID,
		ChatID:      strconv.FormatInt(chatID, 10),
		Phone:       phone,
		Lat:         0,
		Lon:         0,
		DistanceKm:  0,
		DeliveryFee: 0,
		ItemsTotal:  itemsTotal,
	})
	if err != nil {
		b.send(chatID, "Order failed: "+err.Error())
		return
	}

	b.removeKeyboard(chatID, fmt.Sprintf(
		"✅ Buyurtma #%d tasdiqlandi\n\n📱 Raqam: %s\n🛒 Mahsulotlar jami: %d\n💵 Jami: %d\n\nTez orada siz bilan aloqada bo'lamiz.",
		id, phone, itemsTotal, itemsTotal,
	))

	// Send order notification to admin
	b.notifyAdmin(id, phone, items, itemsTotal, userLocation, userLat, userLon, hasUserLocation)
}

func (b *Bot) notifyAdmin(orderID int64, phone string, items []cartItem, total int64, location *models.Location, userLat, userLon float64, hasUserLocation bool) {
	if b.messageBot == nil {
		return // MESSAGE_TOKEN not set or failed to initialize
	}

	// Build order details text message
	text := fmt.Sprintf("🆕 Yangi buyurtma #%d\n\n", orderID)
	text += fmt.Sprintf("📱 Raqam: %s\n\n", phone)

	// Add location references in text (locations will be sent as separate location messages)
	if hasUserLocation {
		text += "📍 Foydalanuvchi lokatsiyasi (quyida yuboriladi)\n"
	}
	if location != nil {
		text += fmt.Sprintf("🏪 Filial: %s (quyida yuboriladi)\n", location.Name)
	}
	if hasUserLocation || location != nil {
		text += "\n"
	}

	text += "🛒 *Savatcha:*\n"
	for _, it := range items {
		text += fmt.Sprintf("• %s × %d — %d\n", it.Name, it.Qty, it.Price*int64(it.Qty))
	}
	text += fmt.Sprintf("\n💵 *Jami: %d*", total)

	// Get branch admins for the selected location
	var adminIDs []int64
	ctx := context.Background()
	if location != nil {
		var err error
		adminIDs, err = services.GetBranchAdmins(ctx, location.ID)
		if err != nil {
			log.Printf("failed to get branch admins for location %d: %v", location.ID, err)
		}
		log.Printf("found %d branch admin(s) for location '%s' (ID: %d)", len(adminIDs), location.Name, location.ID)
	}

	// If no branch admins found, fall back to main admin
	if len(adminIDs) == 0 {
		if b.admin != 0 {
			adminIDs = []int64{b.admin}
			log.Printf("no branch admins found, using main admin %d", b.admin)
		} else {
			log.Printf("warning: no branch admins found and no main admin set for order #%d", orderID)
			return
		}
	}

	// Send order details and location to each branch admin
	for _, adminID := range adminIDs {
		// Send order details text message
		msg := tgbotapi.NewMessage(adminID, text)
		msg.ParseMode = "Markdown"
		if _, err := b.messageBot.Send(msg); err != nil {
			log.Printf("failed to send order details to admin %d via MESSAGE_TOKEN: %v", adminID, err)
			continue
		}
		log.Printf("successfully sent order details for order #%d to branch admin %d", orderID, adminID)

		// Send user's shared location as a real Telegram location message
		if hasUserLocation {
			locMsg := tgbotapi.NewLocation(adminID, userLat, userLon)
			if _, err := b.messageBot.Send(locMsg); err != nil {
				log.Printf("failed to send user location to admin %d via MESSAGE_TOKEN: %v", adminID, err)
			} else {
				log.Printf("successfully sent user location (%.6f, %.6f) for order #%d to admin %d", userLat, userLon, orderID, adminID)
			}
		}
	}
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
	if len(parts) < 3 {
		b.send(chatID, "📝 Usage: /promote <branch_location_id> <new_admin_user_id>\n\n"+
			"Example: /promote 1 123456789\n\n"+
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

	ctx := context.Background()
	err = services.AddBranchAdmin(ctx, branchLocationID, newAdminID, userID)
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
