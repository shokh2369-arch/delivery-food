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
	Step           string // "name", "location", "admin_wait", "admin_id", "password"
	Name           string
	Lat            float64
	Lon            float64
	PendingAdminID int64
}

// addBranchAdminState is for adding a branch admin to an existing location.
type addBranchAdminState struct {
	LocationID      int64
	PendingAdminID  int64
	Step            string // "admin_id", "password"
}

// AdderBot is the admin bot for adding menu items (uses ADDER_TOKEN). Big admin uses LOGIN; branch admins use their unique password.
type AdderBot struct {
	api               *tgbotapi.BotAPI
	login             string
	superAdminID      int64
	state             map[int64]*adderState
	locState          map[int64]*locationAdderState
	addBranchAdmin    map[int64]*addBranchAdminState
	activeLocation    map[int64]int64 // per-admin selected location for menu items
	stateMu           sync.RWMutex
}

// NewAdderBot creates an adder bot using ADDER_TOKEN. superAdminID is the big admin (ADMIN_ID); they use LOGIN. Branch admins log in with their unique password.
func NewAdderBot(cfg *config.Config, superAdminID int64) (*AdderBot, error) {
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
		superAdminID:   superAdminID,
		state:          make(map[int64]*adderState),
		locState:       make(map[int64]*locationAdderState),
		addBranchAdmin: make(map[int64]*addBranchAdminState),
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
			// Treat message as password attempt: big admin uses LOGIN; branch admins use their unique password
			if a.superAdminID != 0 && userID == a.superAdminID && a.login != "" && text == a.login {
				a.setLoggedIn(userID, "super")
				a.sendAdminPanel(msg.Chat.ID, userID)
			} else if locID, ok, err := services.AuthenticateBranchAdmin(context.Background(), userID, text); err == nil && ok {
				a.setLoggedIn(userID, "branch")
				a.stateMu.Lock()
				a.activeLocation[userID] = locID
				a.stateMu.Unlock()
				locName, _ := services.GetLocationName(context.Background(), locID)
				if locName != "" {
					a.send(msg.Chat.ID, "✅ Logged in to «"+locName+"». You can add or edit menu items for your place.")
				}
				a.sendAdminPanel(msg.Chat.ID, userID)
			} else {
				a.send(msg.Chat.ID, "🔒 Send your admin password to access the panel. (Big admin: use LOGIN password; branch admins: use the unique password set for your place.)")
			}
			continue
		}

		// Handle add menu item flow (name -> price)
		if a.handleMenuAddFlow(msg, userID, text) {
			continue
		}

		// Handle add branch admin to existing location (admin_id -> password)
		if a.handleAddBranchAdminFlow(msg, userID, text) {
			continue
		}

		// Handle add location flow (name -> location -> admin_id -> password)
		if a.handleLocationAddFlow(msg, userID, text) {
			continue
		}

		// Logged in, no state: show panel on any other message
		a.sendAdminPanel(msg.Chat.ID, userID)
	}
}

var adderLoggedIn = make(map[int64]bool)
var adderRole     = make(map[int64]string) // "super" or "branch"
var adderLoggedInMu sync.RWMutex

func (a *AdderBot) isLoggedIn(userID int64) bool {
	adderLoggedInMu.RLock()
	ok := adderLoggedIn[userID]
	adderLoggedInMu.RUnlock()
	return ok
}

func (a *AdderBot) getRole(userID int64) string {
	adderLoggedInMu.RLock()
	r := adderRole[userID]
	adderLoggedInMu.RUnlock()
	if r == "" {
		return "super"
	}
	return r
}

func (a *AdderBot) setLoggedIn(userID int64, role string) {
	adderLoggedInMu.Lock()
	adderLoggedIn[userID] = true
	adderRole[userID] = role
	adderLoggedInMu.Unlock()
}

func (a *AdderBot) clearLoggedIn(userID int64) {
	adderLoggedInMu.Lock()
	delete(adderLoggedIn, userID)
	delete(adderRole, userID)
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
	// If already logged in, show the panel instead of asking for password again.
	if a.isLoggedIn(userID) {
		a.stateMu.RLock()
		locFlow := a.locState[userID]
		addFlow := a.addBranchAdmin[userID]
		a.stateMu.RUnlock()
		if locFlow != nil && (locFlow.Step == "admin_id" || locFlow.Step == "password") {
			if locFlow.Step == "admin_id" {
				a.send(chatID, "ℹ️ Siz hozir filial qo'shish jarayonidasiz.\n\n👤 Iltimos, *branch admin* ning Telegram user ID raqamini yuboring.\nAgar bekor qilmoqchi bo'lsangiz: /cancel")
			} else {
				a.send(chatID, "🔑 Send the unique password for this branch admin (must not be used by any other branch admin).\nCancel: /cancel")
			}
			return
		}
		if addFlow != nil {
			if addFlow.Step == "admin_id" {
				a.send(chatID, "👤 Send the Telegram user ID of the new branch admin.")
			} else {
				a.send(chatID, "🔑 Send the unique password for this branch admin.")
			}
			return
		}
		a.sendAdminPanel(chatID, userID)
		return
	}

	a.send(chatID, "🔒 Admin panel. Send your password to continue (big admin: LOGIN; branch admin: your unique password).")
}

func (a *AdderBot) adminKeyboard(userID int64) tgbotapi.InlineKeyboardMarkup {
	a.stateMu.RLock()
	locID := a.activeLocation[userID]
	a.stateMu.RUnlock()
	role := a.getRole(userID)

	// Branch admin: only their place — add/list/delete menu items (no location switch, no add location).
	if role == "branch" {
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
		}
		return tgbotapi.NewInlineKeyboardMarkup(rows...)
	}

	// Super admin: only location management (no adding food — that is for branch admins).
	if locID <= 0 {
		rows := [][]tgbotapi.InlineKeyboardButton{
			{
				tgbotapi.NewInlineKeyboardButtonData("📍 Select Location", "adder:select_location"),
			},
			{
				tgbotapi.NewInlineKeyboardButtonData("📍 Add Fast Food Location", "adder:add_location"),
			},
		}
		return tgbotapi.NewInlineKeyboardMarkup(rows...)
	}
	// Super admin with a location selected: one admin per location — show Add or Change depending on whether it already has an admin.
	admins, _ := services.GetBranchAdmins(context.Background(), locID)
	hasAdmin := len(admins) > 0

	var adminRow []tgbotapi.InlineKeyboardButton
	if hasAdmin {
		adminRow = []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("🔄 Change Admin", "adder:change_branch_admin"),
		}
	} else {
		adminRow = []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("👤 Add Branch Admin", "adder:add_branch_admin"),
		}
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("📍 Select Location", "adder:select_location"),
		},
		adminRow,
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
	role := a.getRole(userID)

	if locID <= 0 {
		text := "📋 Admin — Locations\n\nAvval menyu uchun filialni tanlang yoki yangi fast food joyini qo'shing."
		a.sendWithInline(chatID, text, a.adminKeyboard(userID))
		return
	}

	locLabel := fmt.Sprintf("ID %d", locID)
	if role == "branch" {
		if name, err := services.GetLocationName(context.Background(), locID); err == nil && name != "" {
			locLabel = name
		}
		text := fmt.Sprintf("📋 Admin — %s\n\nAdd or delete menu items for your place. Choose an action below:", locLabel)
		a.sendWithInline(chatID, text, a.adminKeyboard(userID))
		return
	}
	// Super admin with location selected (only location management, no menu items)
	locName, _ := services.GetLocationName(context.Background(), locID)
	if locName != "" {
		locLabel = fmt.Sprintf("%s (ID %d)", locName, locID)
	}
	text := fmt.Sprintf("📋 Admin — %s\n\nSelect a location, add/change branch admin, or delete this location. Choose an action below:", locLabel)
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
		if a.getRole(userID) != "super" {
			return
		}
		a.sendSelectLocationList(chatID, userID)
		return
	case data == "adder:add_branch_admin":
		if a.getRole(userID) != "super" {
			return
		}
		a.stateMu.RLock()
		locID := a.activeLocation[userID]
		a.stateMu.RUnlock()
		if locID <= 0 {
			a.send(chatID, "Please select a location first (📍 Select Location for Menu).")
			return
		}
		a.stateMu.Lock()
		a.addBranchAdmin[userID] = &addBranchAdminState{LocationID: locID, Step: "admin_id"}
		a.stateMu.Unlock()
		a.send(chatID, "👤 Send the Telegram user ID of the new branch admin for this place.\n\n💡 User can get their ID via @userinfobot. Cancel: /cancel")
		return
	case data == "adder:change_branch_admin":
		if a.getRole(userID) != "super" {
			return
		}
		a.stateMu.RLock()
		locID := a.activeLocation[userID]
		a.stateMu.RUnlock()
		if locID <= 0 {
			a.send(chatID, "Please select a location first (📍 Select Location).")
			return
		}
		ctx := context.Background()
		if err := services.RemoveAllBranchAdminsForLocation(ctx, locID); err != nil {
			a.send(chatID, "❌ Failed to remove previous admin(s): "+err.Error())
			return
		}
		a.stateMu.Lock()
		a.addBranchAdmin[userID] = &addBranchAdminState{LocationID: locID, Step: "admin_id"}
		a.stateMu.Unlock()
		a.send(chatID, "✅ Previous admin(s) removed. Send the Telegram user ID of the new branch admin for this place.\n\n💡 User can get their ID via @userinfobot. Cancel: /cancel")
		return
	case strings.HasPrefix(data, "adder:list:"):
		if a.getRole(userID) != "branch" {
			a.send(chatID, "Only branch admins can manage menu items. Big admin only manages locations.")
			return
		}
		cat := strings.TrimPrefix(data, "adder:list:")
		if cat == models.CategoryFood || cat == models.CategoryDrink || cat == models.CategoryDessert {
			a.sendListCategory(chatID, userID, cat)
		}
		return
	case strings.HasPrefix(data, "adder:del:"):
		if a.getRole(userID) != "branch" {
			a.send(chatID, "Only branch admins can manage menu items.")
			return
		}
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
		a.send(chatID, "✅ Item deleted. Send your password to continue.")
		a.clearLoggedIn(userID)
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
		a.send(chatID, "✅ Location set. Send your password to continue.")
		a.clearLoggedIn(userID)
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
		if a.getRole(userID) != "branch" {
			a.send(chatID, "Only branch admins can add menu items. Big admin only manages locations.")
			return
		}
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

// handleMenuAddFlow processes the existing menu add flow (name -> price). Only branch admins can add items.
func (a *AdderBot) handleMenuAddFlow(msg *tgbotapi.Message, userID int64, text string) bool {
	a.stateMu.RLock()
	st := a.state[userID]
	a.stateMu.RUnlock()
	if st != nil && a.getRole(userID) != "branch" {
		a.stateMu.Lock()
		delete(a.state, userID)
		a.stateMu.Unlock()
		a.send(msg.Chat.ID, "Only branch admins can add menu items.")
		return true
	}

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
		a.send(msg.Chat.ID, fmt.Sprintf("✅ Added %s: %s — %d (id %d). Send your password to continue.", catLabel, st.Name, price, id))
		a.clearLoggedIn(userID)
		return true
	}
	return false
}

// handleAddBranchAdminFlow processes adding a branch admin to an existing location (admin_id -> password).
func (a *AdderBot) handleAddBranchAdminFlow(msg *tgbotapi.Message, userID int64, text string) bool {
	a.stateMu.RLock()
	ab := a.addBranchAdmin[userID]
	a.stateMu.RUnlock()
	if ab == nil {
		return false
	}
	switch ab.Step {
	case "admin_id":
		adminID, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil || adminID <= 0 {
			a.send(msg.Chat.ID, "❌ Invalid user ID. Send a numeric Telegram user ID (e.g. 123456789). Cancel: /cancel")
			return true
		}
		a.stateMu.Lock()
		a.addBranchAdmin[userID] = &addBranchAdminState{LocationID: ab.LocationID, PendingAdminID: adminID, Step: "password"}
		a.stateMu.Unlock()
		a.send(msg.Chat.ID, "🔑 Send the unique password for this branch admin (must not be used by any other branch admin). Cancel: /cancel")
		return true
	case "password":
		password := strings.TrimSpace(text)
		if password == "" {
			a.send(msg.Chat.ID, "❌ Password cannot be empty.")
			return true
		}
		passwordHash, err := services.HashBranchAdminPassword(password)
		if err != nil {
			a.send(msg.Chat.ID, "❌ Invalid password: "+err.Error())
			return true
		}
		ctx := context.Background()
		if err := services.AddBranchAdmin(ctx, ab.LocationID, ab.PendingAdminID, userID, passwordHash); err != nil {
			a.send(msg.Chat.ID, "❌ Failed to add branch admin: "+err.Error())
			a.stateMu.Lock()
			delete(a.addBranchAdmin, userID)
			a.stateMu.Unlock()
			return true
		}
		a.stateMu.Lock()
		delete(a.addBranchAdmin, userID)
		a.stateMu.Unlock()
		a.send(msg.Chat.ID, fmt.Sprintf("✅ Branch admin (user ID %d) added. Send your password to continue.", ab.PendingAdminID))
		a.clearLoggedIn(userID)
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
		// Expect numeric Telegram user ID for branch admin; then we ask for unique password.
		adminID, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil || adminID <= 0 {
			a.send(msg.Chat.ID, "❌ Invalid user ID. This step expects a *numeric Telegram user ID* (e.g. 123456789), not the admin password.\n\n💡 Ask the user to use @userinfobot to get their ID. Cancel: /cancel")
			return true
		}
		st.PendingAdminID = adminID
		st.Step = "password"
		a.stateMu.Lock()
		a.locState[userID] = st
		a.stateMu.Unlock()
		a.send(msg.Chat.ID, "🔑 Send the unique password for this branch admin (must not be used by any other branch admin). They will use it to log in to the adder bot. Cancel: /cancel")
		return true
	case "password":
		password := strings.TrimSpace(text)
		if password == "" {
			a.send(msg.Chat.ID, "❌ Password cannot be empty. Send a unique password for this branch admin.")
			return true
		}
		passwordHash, err := services.HashBranchAdminPassword(password)
		if err != nil {
			a.send(msg.Chat.ID, "❌ Invalid password: "+err.Error())
			return true
		}
		ctx := context.Background()
		locID, err := services.CreateLocationWithAdmin(ctx, st.Name, st.Lat, st.Lon, st.PendingAdminID, userID, passwordHash)
		if err != nil {
			a.send(msg.Chat.ID, "Failed to save location + admin: "+err.Error())
			log.Printf("failed to create location with admin (name=%q): %v", st.Name, err)
			return true
		}
		a.stateMu.Lock()
		delete(a.locState, userID)
		a.activeLocation[userID] = locID
		a.stateMu.Unlock()
		a.send(msg.Chat.ID, fmt.Sprintf("✅ Saved fast food location \"%s\" (id %d) and assigned admin. Send your password to continue.", st.Name, locID))
		a.clearLoggedIn(userID)
		return true
	default:
		return false
	}
}

func (a *AdderBot) cancelFlows(chatID int64, userID int64) {
	a.stateMu.Lock()
	delete(a.state, userID)
	delete(a.locState, userID)
	delete(a.addBranchAdmin, userID)
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
