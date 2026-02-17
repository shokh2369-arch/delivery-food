package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	"food-telegram/config"
	"food-telegram/models"
	"food-telegram/services"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type adderState struct {
	Step       string // "idle", "name", "price"
	Category   string
	Name       string
	LocationID int64
}

type locationAdderState struct {
	Step string // "name", "location", "admin_wait", "admin_id"
	Name string
	Lat  float64
	Lon  float64
}

// AdderBot is the admin bot for adding menu items (uses ADDER_TOKEN, LOGIN).
type AdderBot struct {
	api            *tgbotapi.BotAPI
	login          string
	state          map[int64]*adderState
	locState       map[int64]*locationAdderState
	activeLocation map[int64]int64 // per-admin selected location for menu items
	stateMu        sync.RWMutex
}

// NewAdderBot creates an adder bot using ADDER_TOKEN. login is the password from LOGIN.
func NewAdderBot(cfg *config.Config) (*AdderBot, error) {
	if cfg.Telegram.AdderToken == "" {
		return nil, fmt.Errorf("ADDER_TOKEN not set")
	}
	api, err := tgbotapi.NewBotAPI(cfg.Telegram.AdderToken)
	if err != nil {
		return nil, err
	}
	return &AdderBot{
		api:            api,
		login:          strings.TrimSpace(cfg.Telegram.Login),
		state:          make(map[int64]*adderState),
		locState:       make(map[int64]*locationAdderState),
		activeLocation: make(map[int64]int64),
	}, nil
}

func (a *AdderBot) Start() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := a.api.GetUpdatesChan(u)

	for update := range updates {
		if update.CallbackQuery != nil {
			a.handleCallback(update.CallbackQuery)
			continue
		}
		if update.Message == nil {
			continue
		}
		msg := update.Message
		userID := msg.From.ID
		text := strings.TrimSpace(msg.Text)

		if text == "/cancel" {
			a.cancelFlows(msg.Chat.ID, userID)
			continue
		}

		if text == "/start" {
			a.handleStart(msg.Chat.ID, userID)
			continue
		}

		// Check if user is logged in
		if !a.isLoggedIn(userID) {
			// Treat message as password attempt
			if a.login != "" && text == a.login {
				a.setLoggedIn(userID)
				a.sendAdminPanel(msg.Chat.ID, userID)
			} else {
				a.send(msg.Chat.ID, "🔒 Send the admin password to access the panel.")
			}
			continue
		}

		// Handle add menu item flow (name -> price)
		if a.handleMenuAddFlow(msg, userID, text) {
			continue
		}

		// Handle add location flow (name -> location)
		if a.handleLocationAddFlow(msg, userID, text) {
			continue
		}

		// Logged in, no state: show panel on any other message
		a.sendAdminPanel(msg.Chat.ID, userID)
	}
}

var adderLoggedIn = make(map[int64]bool)
var adderLoggedInMu sync.RWMutex

func (a *AdderBot) isLoggedIn(userID int64) bool {
	adderLoggedInMu.RLock()
	ok := adderLoggedIn[userID]
	adderLoggedInMu.RUnlock()
	return ok
}

func (a *AdderBot) setLoggedIn(userID int64) {
	adderLoggedInMu.Lock()
	adderLoggedIn[userID] = true
	adderLoggedInMu.Unlock()
}

func (a *AdderBot) clearLoggedIn(userID int64) {
	adderLoggedInMu.Lock()
	delete(adderLoggedIn, userID)
	adderLoggedInMu.Unlock()
}

func (a *AdderBot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := a.api.Send(msg); err != nil {
		log.Printf("adder send error: %v", err)
	}
}

func (a *AdderBot) sendWithInline(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := a.api.Send(msg); err != nil {
		log.Printf("adder send error: %v", err)
	}
}

func (a *AdderBot) handleStart(chatID int64, userID int64) {
	if a.login == "" {
		a.send(chatID, "Admin panel is not configured (LOGIN empty).")
		return
	}

	// If already logged in, show the panel instead of asking for password again.
	if a.isLoggedIn(userID) {
		a.stateMu.RLock()
		locFlow := a.locState[userID]
		a.stateMu.RUnlock()
		if locFlow != nil && locFlow.Step == "admin_id" {
			a.send(chatID, "ℹ️ Siz hozir filial qo'shish jarayonidasiz.\n\n👤 Iltimos, *branch admin* ning Telegram user ID raqamini yuboring.\nAgar bekor qilmoqchi bo'lsangiz: /cancel")
			return
		}
		a.sendAdminPanel(chatID, userID)
		return
	}

	a.send(chatID, "🔒 Admin panel. Send the password to continue.")
}

func (a *AdderBot) adminKeyboard(userID int64) tgbotapi.InlineKeyboardMarkup {
	a.stateMu.RLock()
	locID := a.activeLocation[userID]
	a.stateMu.RUnlock()

	// If no active location: only show location-related actions.
	if locID <= 0 {
		rows := [][]tgbotapi.InlineKeyboardButton{
			{
				tgbotapi.NewInlineKeyboardButtonData("📍 Select Location for Menu", "adder:select_location"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("📍 Add Fast Food Location", "adder:add_location"),
			},
		}
		return tgbotapi.NewInlineKeyboardMarkup(rows...)
	}

	// If a location is selected: show item add/list/delete + location controls.
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("🍽 Add Food", "adder:add:food"),
			tgbotapi.NewInlineKeyboardButtonData("🥤 Add Drink", "adder:add:drink"),
			tgbotapi.NewInlineKeyboardButtonData("🍰 Add Dessert", "adder:add:dessert"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("📋 List / Delete Foods", "adder:list:food"),
			tgbotapi.NewInlineKeyboardButtonData("📋 List / Delete Drinks", "adder:list:drink"),
			tgbotapi.NewInlineKeyboardButtonData("📋 List / Delete Desserts", "adder:list:dessert"),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("🗑 Delete This Location", "adder:del_location"),
		},
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (a *AdderBot) sendAdminPanel(chatID int64, userID int64) {
	a.stateMu.RLock()
	locID := a.activeLocation[userID]
	a.stateMu.RUnlock()

	if locID <= 0 {
		text := "📋 Admin — Locations\n\nAvval menyu uchun filialni tanlang yoki yangi fast food joyini qo'shing."
		a.sendWithInline(chatID, text, a.adminKeyboard(userID))
		return
	}

	locLabel := fmt.Sprintf("ID %d", locID)
	text := fmt.Sprintf("📋 Admin — Add or delete menu items\n\nActive location for menu items: %s\n\nChoose an action below:", locLabel)
	a.sendWithInline(chatID, text, a.adminKeyboard(userID))
}

func (a *AdderBot) sendListCategory(chatID int64, userID int64, category string) {
	ctx := context.Background()
	var (
		items []models.MenuItem
		err   error
	)
	// If admin has an active location, list only items for that location (plus globals)
	a.stateMu.RLock()
	locID := a.activeLocation[userID]
	a.stateMu.RUnlock()
	if locID > 0 {
		items, err = services.ListMenuByCategoryAndLocation(ctx, category, locID)
	} else {
		items, err = services.ListMenuByCategory(ctx, category)
	}
	if err != nil {
		a.send(chatID, "Failed to load list: "+err.Error())
		return
	}
	catLabel := map[string]string{
		models.CategoryFood: "Foods", models.CategoryDrink: "Drinks", models.CategoryDessert: "Desserts",
	}[category]
	if len(items) == 0 {
		a.sendWithInline(chatID, fmt.Sprintf("No %s in the menu.", catLabel), a.adminKeyboard(userID))
		return
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	text := fmt.Sprintf("📋 %s — tap Delete to remove:\n\n", catLabel)
	for _, item := range items {
		text += fmt.Sprintf("• %s — %d\n", item.Name, item.Price)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🗑 Delete", "adder:del:"+item.ID),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("« Back to panel", "adder:back"),
	))
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	a.sendWithInline(chatID, text, kb)
}

func (a *AdderBot) handleCallback(cq *tgbotapi.CallbackQuery) {
	chatID := cq.Message.Chat.ID
	userID := cq.From.ID
	data := cq.Data

	a.api.Request(tgbotapi.NewCallback(cq.ID, ""))

	if !a.isLoggedIn(userID) {
		return
	}

	switch {
	case data == "adder:back":
		a.sendAdminPanel(chatID, userID)
		return
	case data == "adder:select_location":
		a.sendSelectLocationList(chatID, userID)
		return
	case strings.HasPrefix(data, "adder:list:"):
		cat := strings.TrimPrefix(data, "adder:list:")
		if cat == models.CategoryFood || cat == models.CategoryDrink || cat == models.CategoryDessert {
			a.sendListCategory(chatID, userID, cat)
		}
		return
	case strings.HasPrefix(data, "adder:del:"):
		idStr := strings.TrimPrefix(data, "adder:del:")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			return
		}
		ctx := context.Background()
		if err := services.DeleteMenuItem(ctx, id); err != nil {
			a.send(chatID, "Failed to delete: "+err.Error())
			return
		}
		a.send(chatID, "✅ Item deleted.")
		a.sendAdminPanel(chatID, userID)
		return
	case data == "adder:add_location":
		a.startAddLocation(chatID, userID)
		return
	case data == "adder:locadmin:add":
		// Continue add-location flow: ask for branch admin user ID.
		a.stateMu.Lock()
		st := a.locState[userID]
		if st != nil {
			st.Step = "admin_id"
			a.locState[userID] = st
		}
		a.stateMu.Unlock()
		if st == nil {
			a.send(chatID, "No location flow active. Please start again: \"📍 Add Fast Food Location\".")
			return
		}
		a.send(chatID, "👤 Send the *Telegram user ID* of the branch admin.\n\n💡 To get the correct ID, tell the user to use @userinfobot.")
		return
	case data == "adder:locadmin:cancel":
		// Cancel add-location flow without saving anything.
		a.stateMu.Lock()
		delete(a.locState, userID)
		a.stateMu.Unlock()
		a.send(chatID, "❌ Cancelled. Location was not saved because no admin was assigned.")
		a.sendAdminPanel(chatID, userID)
		return
	case strings.HasPrefix(data, "adder:setloc:"):
		idStr := strings.TrimPrefix(data, "adder:setloc:")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			a.send(chatID, "Invalid location.")
			return
		}
		a.stateMu.Lock()
		a.activeLocation[userID] = id
		a.stateMu.Unlock()
		a.send(chatID, fmt.Sprintf("✅ Active location for menu items set to ID %d.", id))
		a.clearLoggedIn(userID)
		a.sendAdminPanel(chatID, userID)
		return
	case data == "adder:del_location":
		// Delete the currently active location (and its menu items & user bindings)
		a.stateMu.RLock()
		locID := a.activeLocation[userID]
		a.stateMu.RUnlock()
		if locID <= 0 {
			a.send(chatID, "Hech qanday faol filial tanlanmagan.")
			return
		}
		ctx := context.Background()
		if err := services.DeleteLocation(ctx, locID); err != nil {
			a.send(chatID, "Filialni o'chirishda xatolik yuz berdi: "+err.Error())
			return
		}
		a.stateMu.Lock()
		delete(a.activeLocation, userID)
		a.stateMu.Unlock()
		a.clearLoggedIn(userID)
		a.send(chatID, "✅ Filial va uning menyusi o'chirildi. Yangi operatsiya uchun qayta parol kiriting.")
		return
	case strings.HasPrefix(data, "adder:add:"):
		cat := strings.TrimPrefix(data, "adder:add:")
		if cat != models.CategoryFood && cat != models.CategoryDrink && cat != models.CategoryDessert {
			return
		}
		// Require an active location; no global items
		a.stateMu.RLock()
		activeLoc := a.activeLocation[userID]
		a.stateMu.RUnlock()
		if activeLoc <= 0 {
			a.send(chatID, "Iltimos, avval menyu uchun filialni tanlang (\"📍 Select Location for Menu\").")
			return
		}
		a.stateMu.Lock()
		a.state[userID] = &adderState{Step: "name", Category: cat, LocationID: activeLoc}
		a.stateMu.Unlock()

		catLabel := map[string]string{
			models.CategoryFood: "Food", models.CategoryDrink: "Drink", models.CategoryDessert: "Dessert",
		}[cat]
		a.send(chatID, fmt.Sprintf("Send the name for the new %s item for this location (e.g. 🍕 Margherita Pizza):", catLabel))
	}
}

// handleMenuAddFlow processes the existing menu add flow (name -> price).
func (a *AdderBot) handleMenuAddFlow(msg *tgbotapi.Message, userID int64, text string) bool {
	a.stateMu.RLock()
	st := a.state[userID]
	a.stateMu.RUnlock()

	if st != nil && st.Step == "name" {
		st.Name = text
		st.Step = "price"
		a.stateMu.Lock()
		a.state[userID] = st
		a.stateMu.Unlock()
		a.send(msg.Chat.ID, fmt.Sprintf("Enter the price in sum for «%s»:", text))
		return true
	}
	if st != nil && st.Step == "price" {
		price, err := strconv.ParseInt(strings.TrimSpace(strings.ReplaceAll(text, " ", "")), 10, 64)
		if err != nil || price < 0 {
			a.send(msg.Chat.ID, "Invalid price. Send a number (e.g. 15000).")
			return true
		}
		ctx := context.Background()
		// Must have a location; global items are not allowed
		if st.LocationID <= 0 {
			a.send(msg.Chat.ID, "Iltimos, avval menyu uchun filialni tanlang (\"📍 Select Location for Menu\").")
			return true
		}
		id, err := services.AddMenuItemForLocation(ctx, st.Category, st.Name, price, st.LocationID)
		a.stateMu.Lock()
		delete(a.state, userID)
		a.stateMu.Unlock()
		if err != nil {
			a.send(msg.Chat.ID, "Failed to add: "+err.Error())
			return true
		}
		catLabel := map[string]string{
			models.CategoryFood: "Food", models.CategoryDrink: "Drink", models.CategoryDessert: "Dessert",
		}[st.Category]
		a.send(msg.Chat.ID, fmt.Sprintf("✅ Added %s: %s — %d (id %d)", catLabel, st.Name, price, id))
		a.sendAdminPanel(msg.Chat.ID, userID)
		return true
	}
	return false
}

// startAddLocation initializes the flow for adding a fast food location.
func (a *AdderBot) startAddLocation(chatID int64, userID int64) {
	a.stateMu.Lock()
	a.locState[userID] = &locationAdderState{Step: "name"}
	a.stateMu.Unlock()
	a.send(chatID, "Send the name of the fast food location (e.g. \"FastFood Center Chilonzor\").")
}

// handleLocationAddFlow processes the add-location flow (name -> Telegram location).
func (a *AdderBot) handleLocationAddFlow(msg *tgbotapi.Message, userID int64, text string) bool {
	a.stateMu.RLock()
	st := a.locState[userID]
	a.stateMu.RUnlock()

	// No location flow active
	if st == nil {
		return false
	}

	switch st.Step {
	case "name":
		// Expecting the branch name as plain text
		if strings.TrimSpace(text) == "" {
			a.send(msg.Chat.ID, "Name cannot be empty. Please send the fast food location name.")
			return true
		}
		st.Name = text
		st.Step = "location"
		a.stateMu.Lock()
		a.locState[userID] = st
		a.stateMu.Unlock()

		// Ask for Telegram location share
		kb := tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(
				tgbotapi.NewKeyboardButtonLocation("📍 Share this branch location"),
			),
		)
		kb.OneTimeKeyboard = true
		kb.ResizeKeyboard = true

		resp := tgbotapi.NewMessage(msg.Chat.ID, "Now send the location of this fast food branch by pressing the button below.")
		resp.ReplyMarkup = kb
		if _, err := a.api.Send(resp); err != nil {
			log.Printf("adder send error: %v", err)
		}
		return true
	case "location":
		// In this step we expect a Telegram location, not plain text.
		if msg.Location == nil {
			a.send(msg.Chat.ID, "Please send the location using Telegram's location button.")
			return true
		}
		lat := float64(msg.Location.Latitude)
		lon := float64(msg.Location.Longitude)

		// Save coords in state and require admin assignment before inserting into DB.
		st.Lat = lat
		st.Lon = lon
		st.Step = "admin_wait"
		a.stateMu.Lock()
		a.locState[userID] = st
		a.stateMu.Unlock()

		// Remove reply keyboard, then show inline actions to assign admin.
		removeKb := tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf("📍 Location received for \"%s\".\n\n⚠️ This branch will be saved *only after* you assign a branch admin.", st.Name))
		removeKb.ReplyMarkup = tgbotapi.NewRemoveKeyboard(true)
		if _, err := a.api.Send(removeKb); err != nil {
			log.Printf("adder send error: %v", err)
		}

		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("➕ Add admin", "adder:locadmin:add"),
				tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", "adder:locadmin:cancel"),
			),
		)
		a.sendWithInline(msg.Chat.ID, "Assign a branch admin to complete saving this location.", kb)
		return true
	case "admin_id":
		// Expect numeric Telegram user ID for branch admin.
		adminID, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil || adminID <= 0 {
			// Users often paste the admin password here by mistake due to /start prompt.
			a.send(msg.Chat.ID, "❌ Invalid user ID. This step expects a *numeric Telegram user ID* (masalan: 123456789), not the admin password.\n\n💡 Ask the user to use @userinfobot to get their ID.\nBekor qilish: /cancel")
			return true
		}

		ctx := context.Background()
		locID, err := services.CreateLocationWithAdmin(ctx, st.Name, st.Lat, st.Lon, adminID, userID)
		if err != nil {
			a.send(msg.Chat.ID, "Failed to save location + admin: "+err.Error())
			log.Printf("failed to create location with admin (name=%q): %v", st.Name, err)
			return true
		}

		a.stateMu.Lock()
		delete(a.locState, userID)
		// Set as active location for menu items for convenience
		a.activeLocation[userID] = locID
		a.stateMu.Unlock()

		a.send(msg.Chat.ID, fmt.Sprintf("✅ Saved fast food location \"%s\" (id %d) and assigned admin user ID %d.", st.Name, locID, adminID))
		a.sendAdminPanel(msg.Chat.ID, userID)
		return true
	default:
		return false
	}
}

func (a *AdderBot) cancelFlows(chatID int64, userID int64) {
	a.stateMu.Lock()
	delete(a.state, userID)
	delete(a.locState, userID)
	a.stateMu.Unlock()

	if a.isLoggedIn(userID) {
		a.send(chatID, "✅ Cancelled. Admin panel opened.")
		a.sendAdminPanel(chatID, userID)
		return
	}
	a.send(chatID, "✅ Cancelled.")
	a.handleStart(chatID, userID)
}

// sendSelectLocationList shows all locations so admin can pick an active one for menu items.
func (a *AdderBot) sendSelectLocationList(chatID int64, userID int64) {
	ctx := context.Background()
	locs, err := services.ListLocations(ctx)
	if err != nil {
		a.send(chatID, "Joylashuvlar ro'yxatini yuklashda xatolik yuz berdi.")
		return
	}
	if len(locs) == 0 {
		a.send(chatID, "Hozircha birorta ham fast food joyi qo'shilmagan.")
		return
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, l := range locs {
		label := fmt.Sprintf("%s (ID %d)", l.Name, l.ID)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, fmt.Sprintf("adder:setloc:%d", l.ID)),
		))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("« Back to panel", "adder:back"),
	))

	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	a.sendWithInline(chatID, "Filialni tanlang, shundan so'ng menyudagi mahsulotlar shu filialga bog'lanadi.", kb)
}
