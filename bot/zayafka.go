package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"food-telegram/config"
	"food-telegram/services"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type zayafkaApplyState struct {
	Step           string
	FullName       string
	Phone          string
	RestaurantName string
	Lat            float64
	Lon            float64
}

// ZayafkaBot is the application-form-only bot (ariza). New-application notification is sent via adder so superadmin Approve/Reject in adder.
// Reject-reason state is stored in DB (reject_in_progress_by) for restart safety when adderAPI == nil.
type ZayafkaBot struct {
	api             *tgbotapi.BotAPI
	adderAPI        *tgbotapi.BotAPI // when set, new-application notification is sent from adder (so callbacks go to adder)
	superAdminID    int64
	applyRestaurant map[int64]*zayafkaApplyState
	onExpRenew      func(tgUserID int64, role string, replyChatID int64) // when superadmin taps renew in Zayafka
	stateMu         sync.RWMutex
}

const zayafkaCancelButtonText = "❌ Bekor qilish"

func NewZayafkaBot(cfg *config.Config, superAdminID int64, adderAPI *tgbotapi.BotAPI) (*ZayafkaBot, error) {
	if cfg.Telegram.ZayafkaToken == "" {
		return nil, fmt.Errorf("ZAYAFKA token not set")
	}
	api, err := tgbotapi.NewBotAPI(cfg.Telegram.ZayafkaToken)
	if err != nil {
		return nil, err
	}
	sid := cfg.Telegram.SuperadminID
	if sid == 0 {
		sid = superAdminID
	}
	return &ZayafkaBot{
		api:             api,
		adderAPI:        adderAPI,
		superAdminID:    sid,
		applyRestaurant: make(map[int64]*zayafkaApplyState),
	}, nil
}

func (z *ZayafkaBot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := z.api.Send(msg); err != nil {
		log.Printf("zayafka send: %v", err)
	}
}

func (z *ZayafkaBot) sendWithInline(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := z.api.Send(msg); err != nil {
		log.Printf("zayafka send: %v", err)
	}
}

func (z *ZayafkaBot) sendWithReplyKeyboard(chatID int64, text string, kb tgbotapi.ReplyKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := z.api.Send(msg); err != nil {
		log.Printf("zayafka send: %v", err)
	}
}

func (z *ZayafkaBot) sendRemoveKeyboard(chatID int64, text string) {
	kb := tgbotapi.NewRemoveKeyboard(true)
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := z.api.Send(msg); err != nil {
		log.Printf("zayafka send: %v", err)
	}
}

// answerCallback answers the callback query (required for every callback path). If showAlert, Telegram shows a popup. On Request error, logs and sends error message to chat.
func (z *ZayafkaBot) answerCallback(cq *tgbotapi.CallbackQuery, text string, showAlert bool) {
	cb := tgbotapi.NewCallback(cq.ID, text)
	cb.ShowAlert = showAlert
	if _, err := z.api.Request(cb); err != nil {
		log.Printf("zayafka answerCallback: %v", err)
		z.send(cq.Message.Chat.ID, "Xatolik yuz berdi. Iltimos qayta urinib ko'ring.")
	}
}

// notifyAdminNewApplication sends the new-application message to superadmin. Uses adder API when set so Approve/Reject are handled in adder.
// GetAPI returns the Zayafka bot API (e.g. for adder to send password/rejection to applicants).
func (z *ZayafkaBot) GetAPI() *tgbotapi.BotAPI {
	return z.api
}

// SetOnExpRenew sets the callback when superadmin taps "Yangilash" in Zayafka (subscription renewal).
func (z *ZayafkaBot) SetOnExpRenew(f func(tgUserID int64, role string, replyChatID int64)) {
	z.onExpRenew = f
}

// notifyAdminNewApplication sends the new-application message to superadmin in Zayavka only (approve/reject handled in Zayavka; password delivered only via Zayavka).
func (z *ZayafkaBot) notifyAdminNewApplication(appID, fullName, phone, restaurantName string, lat, lon float64) {
	adminMsg := fmt.Sprintf("🆕 **Yangi restoran arizasi**\n\n👤 %s\n📱 %s\n🏪 %s\n📍 %.4f, %.4f", fullName, phone, restaurantName, lat, lon)
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Tasdiqlash (parol yuboriladi)", "app_approve:"+appID),
			tgbotapi.NewInlineKeyboardButtonData("❌ Rad etish", "app_reject:"+appID),
		),
	)
	adm := tgbotapi.NewMessage(z.superAdminID, adminMsg)
	adm.ParseMode = "Markdown"
	adm.ReplyMarkup = kb
	if _, sendErr := z.api.Send(adm); sendErr != nil {
		log.Printf("zayafka: notify admin of new application: %v", sendErr)
	}
}

func (z *ZayafkaBot) Start() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := z.api.GetUpdatesChan(u)

	for update := range updates {
		if update.CallbackQuery != nil {
			z.handleCallback(update.CallbackQuery)
			continue
		}
		if update.Message == nil {
			continue
		}
		msg := update.Message
		userID := msg.From.ID
		text := strings.TrimSpace(msg.Text)

		if text == "/cancel" {
			z.cancelFlows(msg.Chat.ID, userID)
			continue
		}
		if text == "/start" {
			z.handleStart(msg.Chat.ID, userID)
			continue
		}
		if text == "/apply" {
			z.handleApplyCommand(msg.Chat.ID, userID)
			continue
		}

		// Superadmin reject reason
		if z.handleRejectReasonFlow(msg, userID, text) {
			continue
		}

		// Apply flow: contact
		if msg.Contact != nil {
			if z.handleApplyFlowContact(msg.Chat.ID, userID, msg.Contact.PhoneNumber) {
				continue
			}
		}
		// Apply flow: location
		if msg.Location != nil {
			if z.handleApplyFlowLocation(msg.Chat.ID, userID, msg.Location.Latitude, msg.Location.Longitude) {
				continue
			}
		}

		// In apply flow
		if z.handleApplyFlow(msg, userID, text) {
			continue
		}

		// Status gating (credential first: no credential => never "already approved")
		ctx := context.Background()
		hasCred, _ := services.HasApprovedCredential(ctx, userID, services.UserRoleRestaurantAdmin)
		status, _ := services.GetUserApplicationStatus(ctx, userID, services.ApplicationTypeRestaurantAdmin)
		if hasCred && status == services.ApplicationStatusApproved {
			z.send(msg.Chat.ID, "✅ Siz allaqachon tasdiqlangansiz. Admin botga kirib /login orqali foydalaning.")
			continue
		}
		if status == services.ApplicationStatusPending {
			z.send(msg.Chat.ID, "⏳ Arizangiz ko'rib chiqilmoqda.")
			continue
		}
		if status == services.ApplicationStatusRejected {
			kb := tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📋 Qayta ariza", "apply_start")),
			)
			z.sendWithInline(msg.Chat.ID, "❌ Ariza rad etildi.\n\nQayta topshirish: /apply", kb)
			continue
		}
		if status == services.ApplicationStatusApproved {
			_, _ = services.MarkApprovedRestaurantAdminRejectedIfNoCredential(ctx, userID)
			kb := tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📋 Qayta ariza", "apply_start")),
			)
			z.sendWithInline(msg.Chat.ID, "📋 Avvalgi filial o'chirilgan. Yangi filial uchun ariza yuboring: /apply", kb)
			continue
		}

		z.send(msg.Chat.ID, "📋 Ariza yuborish uchun /apply bosing.")
	}
}

func (z *ZayafkaBot) cancelFlows(chatID int64, userID int64) {
	z.stateMu.Lock()
	delete(z.applyRestaurant, userID)
	z.stateMu.Unlock()
	z.sendRemoveKeyboard(chatID, "✅")
	z.send(chatID, "Bekor qilindi. /start yoki /apply bilan qaytadan boshlang.")
}

func (z *ZayafkaBot) handleStart(chatID int64, userID int64) {
	ctx := context.Background()
	// Check credential first: with empty/new DB there is no credential, so we never show "already approved"
	hasCred, _ := services.HasApprovedCredential(ctx, userID, services.UserRoleRestaurantAdmin)
	if !hasCred {
		status, _ := services.GetUserApplicationStatus(ctx, userID, services.ApplicationTypeRestaurantAdmin)
		if status == services.ApplicationStatusApproved {
			_, _ = services.MarkApprovedRestaurantAdminRejectedIfNoCredential(ctx, userID)
			kb := tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📋 Ariza yuborish", "apply_start")),
			)
			z.sendWithInline(chatID, "📋 Avvalgi filial o'chirilgan. Yangi filial uchun ariza yuboring.", kb)
			return
		}
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📋 Ariza yuborish", "apply_start")),
		)
		z.sendWithInline(chatID, "📋 Restoran admini bo'lish uchun ariza yuboring.", kb)
		return
	}
	status, _ := services.GetUserApplicationStatus(ctx, userID, services.ApplicationTypeRestaurantAdmin)
	if status == services.ApplicationStatusApproved {
		z.send(chatID, "✅ Siz allaqachon tasdiqlangansiz. Admin botga kirib /login orqali foydalaning.")
		return
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📋 Ariza yuborish", "apply_start")),
	)
	z.sendWithInline(chatID, "📋 Restoran admini bo'lish uchun ariza yuboring.", kb)
}

func (z *ZayafkaBot) handleApplyCommand(chatID int64, userID int64) {
	ctx := context.Background()
	hasCred, _ := services.HasApprovedCredential(ctx, userID, services.UserRoleRestaurantAdmin)
	if hasCred {
		z.send(chatID, "✅ Siz allaqachon tasdiqlangansiz. Admin botga kirib /login orqali foydalaning.")
		return
	}
	status, _ := services.GetUserApplicationStatus(ctx, userID, services.ApplicationTypeRestaurantAdmin)
	if status == services.ApplicationStatusPending {
		z.send(chatID, "⏳ Arizangiz ko'rib chiqilmoqda.")
		return
	}
	if status == services.ApplicationStatusApproved {
		_, _ = services.MarkApprovedRestaurantAdminRejectedIfNoCredential(ctx, userID)
	}
	z.stateMu.Lock()
	z.applyRestaurant[userID] = &zayafkaApplyState{Step: "full_name"}
	z.stateMu.Unlock()
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Bekor qilish", "adder_cancel")),
	)
	z.sendWithInline(chatID, "📋 Ism familyangizni yuboring:", kb)
}

func (z *ZayafkaBot) handleApplyFlow(msg *tgbotapi.Message, userID int64, text string) bool {
	z.stateMu.Lock()
	st := z.applyRestaurant[userID]
	z.stateMu.Unlock()
	if st == nil {
		return false
	}
	chatID := msg.Chat.ID

	if text == zayafkaCancelButtonText {
		z.stateMu.Lock()
		delete(z.applyRestaurant, userID)
		z.stateMu.Unlock()
		z.sendRemoveKeyboard(chatID, "✅")
		z.send(chatID, "Bekor qilindi. /start yoki /apply bilan qaytadan boshlang.")
		return true
	}

	switch st.Step {
	case "full_name":
		st.FullName = text
		st.Step = "phone"
		z.stateMu.Lock()
		z.applyRestaurant[userID] = st
		z.stateMu.Unlock()
		kb := tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(tgbotapi.NewKeyboardButtonContact("📱 Raqamni ulashish")),
			tgbotapi.NewKeyboardButtonRow(tgbotapi.NewKeyboardButton(zayafkaCancelButtonText)),
		)
		kb.ResizeKeyboard = true
		kb.OneTimeKeyboard = true
		z.sendWithReplyKeyboard(chatID, "📱 Telefon raqamingizni yuboring yoki tugma orqali ulashing:", kb)
		return true
	case "phone":
		st.Phone = text
		st.Step = "restaurant_name"
		z.stateMu.Lock()
		z.applyRestaurant[userID] = st
		z.stateMu.Unlock()
		z.sendRemoveKeyboard(chatID, "✅")
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Bekor qilish", "adder_cancel")),
		)
		z.sendWithInline(chatID, "🏪 Restoran nomini yuboring:", kb)
		return true
	case "restaurant_name":
		st.RestaurantName = text
		st.Step = "location"
		z.stateMu.Lock()
		z.applyRestaurant[userID] = st
		z.stateMu.Unlock()
		kb := tgbotapi.NewReplyKeyboard(
			tgbotapi.NewKeyboardButtonRow(tgbotapi.NewKeyboardButtonLocation("📍 Lokatsiyani ulashish")),
			tgbotapi.NewKeyboardButtonRow(tgbotapi.NewKeyboardButton(zayafkaCancelButtonText)),
		)
		kb.ResizeKeyboard = true
		kb.OneTimeKeyboard = true
		z.sendWithReplyKeyboard(chatID, "📍 Lokatsiyani yuboring (quyidagi tugma orqali).", kb)
		return true
	case "location":
		z.sendRemoveKeyboard(chatID, "✅")
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Bekor qilish", "adder_cancel")),
		)
		z.sendWithInline(chatID, "📍 Iltimos, lokatsiyani tugma orqali yuboring.", kb)
		return true
	case "confirm":
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("✅ Arizani yuborish", "apply_confirm")),
		)
		z.sendWithInline(chatID, "Arizani yuborish uchun quyidagi tugmani bosing.", kb)
		return true
	}
	return true
}

func (z *ZayafkaBot) handleApplyFlowContact(chatID int64, userID int64, phone string) bool {
	z.stateMu.Lock()
	st := z.applyRestaurant[userID]
	z.stateMu.Unlock()
	if st == nil || st.Step != "phone" {
		return false
	}
	st.Phone = strings.TrimSpace(phone)
	if st.Phone == "" {
		st.Phone = phone
	}
	st.Step = "restaurant_name"
	z.stateMu.Lock()
	z.applyRestaurant[userID] = st
	z.stateMu.Unlock()
	z.sendRemoveKeyboard(chatID, "✅")
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Bekor qilish", "adder_cancel")),
	)
	z.sendWithInline(chatID, "🏪 Restoran nomini yuboring:", kb)
	return true
}

func (z *ZayafkaBot) handleApplyFlowLocation(chatID int64, userID int64, lat, lon float64) bool {
	z.stateMu.Lock()
	st := z.applyRestaurant[userID]
	z.stateMu.Unlock()
	if st == nil || st.Step != "location" {
		return false
	}
	st.Lat = lat
	st.Lon = lon
	st.Step = "confirm"
	z.stateMu.Lock()
	z.applyRestaurant[userID] = st
	z.stateMu.Unlock()
	z.sendRemoveKeyboard(chatID, "✅")
	summary := fmt.Sprintf("Ism: %s\nTel: %s\nRestoran: %s\nLokatsiya: %.4f, %.4f\n\nTasdiqlaysizmi?", st.FullName, st.Phone, st.RestaurantName, st.Lat, st.Lon)
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("✅ Arizani yuborish", "apply_confirm")),
	)
	z.sendWithInline(chatID, summary, kb)
	return true
}

func (z *ZayafkaBot) handleRejectReasonFlow(msg *tgbotapi.Message, userID int64, text string) bool {
	if z.superAdminID == 0 || userID != z.superAdminID {
		return false
	}
	ctx := context.Background()
	appID, err := services.GetApplicationIDByRejectInProgressBy(ctx, userID)
	if err != nil || appID == "" {
		return false
	}
	reason := strings.TrimSpace(text)
	if reason == "/skip" || reason == "" {
		reason = "Sabab ko'rsatilmadi."
	}
	if err := services.RejectApplication(ctx, appID, userID, reason); err != nil {
		z.send(msg.Chat.ID, "❌ "+err.Error())
	} else {
		app, _, _, _ := services.GetApplicationByID(ctx, appID)
		if app != nil {
			notifyMsg := "Sizning so'rovnomangizda xatolik/to'lov qilinmaganligi bor. @nonfindable ga bog'laning"
			kb := tgbotapi.NewInlineKeyboardMarkup(
				tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📋 Qayta ariza", "apply_show_previous:"+app.ID)),
			)
			z.sendWithInline(app.ChatID, notifyMsg, kb)
		}
		z.send(msg.Chat.ID, "✅ Ariza rad etildi.")
	}
	return true
}

const zayafkaAlreadyReviewedAlert = "Bu ariza allaqachon ko'rib chiqilgan."

func (z *ZayafkaBot) handleCallback(cq *tgbotapi.CallbackQuery) {
	chatID := cq.Message.Chat.ID
	userID := cq.From.ID
	data := cq.Data

	if data == "apply_start" {
		z.handleApplyCommand(chatID, userID)
		z.answerCallback(cq, "", false)
		return
	}
	if strings.HasPrefix(data, "apply_show_previous:") {
		prevID := strings.TrimPrefix(data, "apply_show_previous:")
		ctx := context.Background()
		app, rest, _, err := services.GetApplicationByID(ctx, prevID)
		if err != nil || app == nil || rest == nil || app.Type != services.ApplicationTypeRestaurantAdmin || app.TgUserID != userID {
			z.send(chatID, "❌ Ariza topilmadi.")
			z.answerCallback(cq, "", false)
			return
		}
		summary := fmt.Sprintf("Avvalgi ariza:\n\n👤 %s\n📱 %s\n🏪 %s\n📍 %.4f, %.4f\n\nQayta yuborish yoki yangi ariza?", app.FullName, app.Phone, rest.RestaurantName, rest.Lat, rest.Lon)
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("✅ Arizani yuborish", "apply_resubmit:"+prevID),
				tgbotapi.NewInlineKeyboardButtonData("📋 Yangi ariza", "apply_start"),
			),
		)
		z.sendWithInline(chatID, summary, kb)
		z.answerCallback(cq, "", false)
		return
	}
	if strings.HasPrefix(data, "apply_resubmit:") {
		prevID := strings.TrimPrefix(data, "apply_resubmit:")
		ctx := context.Background()
		app, rest, _, err := services.GetApplicationByID(ctx, prevID)
		if err != nil || app == nil || rest == nil || app.Type != services.ApplicationTypeRestaurantAdmin || app.TgUserID != userID {
			z.send(chatID, "❌ Ariza topilmadi.")
			z.answerCallback(cq, "", false)
			return
		}
		status, _ := services.GetUserApplicationStatus(ctx, app.TgUserID, services.ApplicationTypeRestaurantAdmin)
		if status != services.ApplicationStatusRejected {
			z.send(chatID, "Yangi ariza yuborib bo'lmaydi.")
			z.answerCallback(cq, "Yangi ariza yuborib bo'lmaydi.", true)
			return
		}
		appID, err := services.CreateApplicationRestaurant(ctx, app.TgUserID, chatID, app.FullName, app.Phone, app.Language, rest.RestaurantName, rest.Lat, rest.Lon, rest.Address)
		if err != nil {
			z.send(chatID, "❌ "+err.Error())
			z.answerCallback(cq, "", false)
			return
		}
		z.send(chatID, "✅ Arizangiz qayta qabul qilindi. Superadmin tekshiradi.")
		if z.superAdminID != 0 {
			z.notifyAdminNewApplication(appID, app.FullName, app.Phone, rest.RestaurantName, rest.Lat, rest.Lon)
		}
		z.answerCallback(cq, "", false)
		return
	}
	if data == "adder_cancel" {
		z.stateMu.Lock()
		delete(z.applyRestaurant, userID)
		z.stateMu.Unlock()
		z.send(chatID, "Bekor qilindi. /start yoki /apply bilan qaytadan boshlang.")
		z.answerCallback(cq, "", false)
		return
	}
	if data == "apply_confirm" {
		z.stateMu.Lock()
		st := z.applyRestaurant[userID]
		z.stateMu.Unlock()
		if st != nil && st.Step == "confirm" {
			ctx := context.Background()
			appID, err := services.CreateApplicationRestaurant(ctx, userID, chatID, st.FullName, st.Phone, "uz", st.RestaurantName, st.Lat, st.Lon, nil)
			z.stateMu.Lock()
			delete(z.applyRestaurant, userID)
			z.stateMu.Unlock()
			if err != nil {
				z.send(chatID, "❌ "+err.Error())
			} else {
				z.send(chatID, "✅ Arizangiz qabul qilindi. Superadmin tekshiradi.")
				if z.superAdminID != 0 {
					z.notifyAdminNewApplication(appID, st.FullName, st.Phone, st.RestaurantName, st.Lat, st.Lon)
				}
			}
		} else {
			z.stateMu.Lock()
			delete(z.applyRestaurant, userID)
			z.stateMu.Unlock()
		}
		z.answerCallback(cq, "", false)
		return
	}
	// Subscription renewal: superadmin tapped "Yangilash" in Zayafka
	if strings.HasPrefix(data, "exp_renew:") && z.superAdminID != 0 && userID == z.superAdminID && z.onExpRenew != nil {
		rest := strings.TrimPrefix(data, "exp_renew:")
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) == 2 {
			var tgUserID int64
			if _, err := fmt.Sscanf(parts[0], "%d", &tgUserID); err == nil && tgUserID > 0 {
				role := parts[1]
				if role == services.UserRoleRestaurantAdmin || role == services.UserRoleDriver {
					z.onExpRenew(tgUserID, role, chatID)
					z.answerCallback(cq, "OK", false)
					return
				}
			}
		}
		z.answerCallback(cq, "", false)
		return
	}

	// Approve/reject only in Zayavka; password delivered only via Zayavka.
	if strings.HasPrefix(data, "app_approve:") {
		appID := strings.TrimPrefix(data, "app_approve:")
		if z.superAdminID == 0 || userID != z.superAdminID {
			z.answerCallback(cq, "", false)
			return
		}
		ctx := context.Background()
		app, _, _, _ := services.GetApplicationByID(ctx, appID)
		if app == nil || app.Status != services.ApplicationStatusPending {
			z.answerCallback(cq, zayafkaAlreadyReviewedAlert, true)
			return
		}
		plainPass, err := services.ApproveApplication(ctx, appID, userID)
		if err != nil {
			z.send(chatID, "❌ "+err.Error())
			z.answerCallback(cq, "", false)
			return
		}
		app, _, _, _ = services.GetApplicationByID(ctx, appID)
		if app != nil {
			z.send(app.ChatID, fmt.Sprintf("✅ Tasdiqlandi.\nParolingiz: %s\n\nAdmin botga kirib /login orqali foydalaning.", plainPass))
		}
		z.send(chatID, "✅ Ariza tasdiqlandi. Parol arizachiga yuborildi.")
		z.answerCallback(cq, "Approved", false)
		return
	}
	if strings.HasPrefix(data, "app_reject:") {
		appID := strings.TrimPrefix(data, "app_reject:")
		if z.superAdminID == 0 || userID != z.superAdminID {
			z.answerCallback(cq, "", false)
			return
		}
		ctx := context.Background()
		app, _, _, _ := services.GetApplicationByID(ctx, appID)
		if app == nil || app.Status != services.ApplicationStatusPending {
			z.answerCallback(cq, zayafkaAlreadyReviewedAlert, true)
			return
		}
		ok, err := services.SetRejectInProgress(ctx, appID, userID)
		if err != nil || !ok {
			z.answerCallback(cq, zayafkaAlreadyReviewedAlert, true)
			return
		}
		z.send(chatID, "Sabab yuboring (yoki /skip standart sabab uchun):")
		z.answerCallback(cq, "Send reason", false)
		return
	}

	z.answerCallback(cq, "", false)
}
