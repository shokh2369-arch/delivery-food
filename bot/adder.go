package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"food-telegram/config"
	"food-telegram/models"
	"food-telegram/services"
)

type adderState struct {
	Step     string // "idle", "name", "price"
	Category string
	Name     string
}

// AdderBot is the admin bot for adding menu items (uses ADDER_TOKEN, LOGIN).
type AdderBot struct {
	api    *tgbotapi.BotAPI
	login  string
	state  map[int64]*adderState
	stateMu sync.RWMutex
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
		api:   api,
		login: strings.TrimSpace(cfg.Telegram.Login),
		state: make(map[int64]*adderState),
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

		// Handle add flow (name -> price)
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
			continue
		}
		if st != nil && st.Step == "price" {
			price, err := strconv.ParseInt(strings.TrimSpace(strings.ReplaceAll(text, " ", "")), 10, 64)
			if err != nil || price < 0 {
				a.send(msg.Chat.ID, "Invalid price. Send a number (e.g. 15000).")
				continue
			}
			ctx := context.Background()
			id, err := services.AddMenuItem(ctx, st.Category, st.Name, price)
			a.stateMu.Lock()
			delete(a.state, userID)
			a.stateMu.Unlock()
			if err != nil {
				a.send(msg.Chat.ID, "Failed to add: "+err.Error())
				continue
			}
			catLabel := map[string]string{
				models.CategoryFood: "Food", models.CategoryDrink: "Drink", models.CategoryDessert: "Dessert",
			}[st.Category]
			a.send(msg.Chat.ID, fmt.Sprintf("✅ Added %s: %s — %d (id %d)", catLabel, st.Name, price, id))
			a.sendAdminPanel(msg.Chat.ID, userID)
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
	a.send(chatID, "🔒 Admin panel. Send the password to continue.")
}

func (a *AdderBot) adminKeyboard() tgbotapi.InlineKeyboardMarkup {
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

func (a *AdderBot) sendAdminPanel(chatID int64, userID int64) {
	a.sendWithInline(chatID, "📋 Admin — Add or delete menu items\n\nChoose an action below:", a.adminKeyboard())
}

func (a *AdderBot) sendListCategory(chatID int64, userID int64, category string) {
	ctx := context.Background()
	items, err := services.ListMenuByCategory(ctx, category)
	if err != nil {
		a.send(chatID, "Failed to load list: "+err.Error())
		return
	}
	catLabel := map[string]string{
		models.CategoryFood: "Foods", models.CategoryDrink: "Drinks", models.CategoryDessert: "Desserts",
	}[category]
	if len(items) == 0 {
		a.sendWithInline(chatID, fmt.Sprintf("No %s in the menu.", catLabel), a.adminKeyboard())
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
	case strings.HasPrefix(data, "adder:add:"):
		cat := strings.TrimPrefix(data, "adder:add:")
		if cat != models.CategoryFood && cat != models.CategoryDrink && cat != models.CategoryDessert {
			return
		}
		a.stateMu.Lock()
		a.state[userID] = &adderState{Step: "name", Category: cat}
		a.stateMu.Unlock()

		catLabel := map[string]string{
			models.CategoryFood: "Food", models.CategoryDrink: "Drink", models.CategoryDessert: "Dessert",
		}[cat]
		a.send(chatID, fmt.Sprintf("Send the name for the new %s item (e.g. 🍕 Margherita Pizza):", catLabel))
	}
}
