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
	Step                string // "name", "location", "admin_wait", "admin_id", "password", "order_lang"
	Name                string
	Lat                 float64
	Lon                 float64
	PendingAdminID      int64
	PendingPasswordHash string
}

// addBranchAdminState is for adding a branch admin to an existing location.
type addBranchAdminState struct {
	LocationID          int64
	PendingAdminID      int64
	PendingPasswordHash string
	Step                string // "admin_id", "password", "order_lang"
}

// Admin (Adder) bot: password-only entry; no application form, no password sending. Applications and password delivery are in Zayavka only.
const (
	adderLoginPrompt         = "🔒 Kirish uchun parolni kiriting.\nParolni Zayavka bot orqali olasiz."
	adderRequireLoginMsg     = "🔒 Avval /login orqali kiring."
	adderSubscriptionExpiredMsg = "❌ Abonement tugagan.\nYangilash uchun Zayavka bot orqali so'rov yuboring."
)

// AdderBot is the admin bot: password login and admin panel only. No application form; no password generation/sending.
type AdderBot struct {
	api               *tgbotapi.BotAPI
	zayafkaAPI        *tgbotapi.BotAPI // optional: send password/rejection to applicant via Zayafka (they applied there)
	cfg               *config.Config
	login             string
	superAdminID      int64
	state             map[int64]*adderState
	locState          map[int64]*locationAdderState
	addBranchAdmin  map[int64]*addBranchAdminState
	activeLocation  map[int64]int64 // per-admin selected location for menu items
	expiredNotified      map[string]bool // "tg_user_id:role" -> already sent superadmin expiry notification
	onSubscriptionRenewed func(tgUserID int64, role string) // clear background-job "already notified" so next expiry can notify again
	stateMu              sync.RWMutex
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
	sid := cfg.Telegram.SuperadminID
	if sid == 0 {
		sid = superAdminID
	}
	return &AdderBot{
		api:               api,
		cfg:               cfg,
		login:             strings.TrimSpace(cfg.Telegram.Login),
		superAdminID:      sid,
		state:             make(map[int64]*adderState),
		locState:          make(map[int64]*locationAdderState),
		addBranchAdmin: make(map[int64]*addBranchAdminState),
		activeLocation: make(map[int64]int64),
		expiredNotified:   make(map[string]bool),
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
		if text == "/apply" {
			a.send(msg.Chat.ID, "📋 Ariza yuborish uchun Zayafka botidan foydalaning.")
			continue
		}
		if text == "/applications" {
			a.handleApplicationsCommand(msg.Chat.ID, userID)
			continue
		}
		if a.superAdminID != 0 && userID == a.superAdminID {
			if text == "/subs_pending" {
				a.handleSubsPending(msg.Chat.ID)
				continue
			}
			if strings.HasPrefix(text, "/renew ") {
				a.handleRenew(msg.Chat.ID, userID, strings.TrimSpace(text[6:]))
				continue
			}
			if strings.HasPrefix(text, "/reset_password ") {
				a.handleResetPassword(msg.Chat.ID, strings.TrimSpace(text[15:]))
				continue
			}
			if strings.HasPrefix(text, "/sub_info ") {
				a.handleSubInfo(msg.Chat.ID, strings.TrimSpace(text[9:]))
				continue
			}
			if strings.HasPrefix(text, "/pause ") {
				a.handlePause(msg.Chat.ID, strings.TrimSpace(text[6:]))
				continue
			}
			if strings.HasPrefix(text, "/unpause ") {
				a.handleUnpause(msg.Chat.ID, strings.TrimSpace(text[8:]))
				continue
			}
			if strings.HasPrefix(text, "/add_driver ") {
				a.handleAddDriver(msg.Chat.ID, strings.TrimSpace(text[11:]))
				continue
			}
		}
		if text == "/login" {
			a.send(msg.Chat.ID, adderLoginPrompt)
			continue
		}

		// Not logged in: only password entry (no application flow; that is in Zayavka only).
		if !a.isLoggedIn(userID) {
			ctx := context.Background()
			// Superadmin can always log in with LOGIN password first
			if a.superAdminID != 0 && userID == a.superAdminID && a.login != "" {
				if wait, _ := services.LoginThrottleWaitSeconds(ctx, userID, services.ThrottleRoleSuperadmin); wait > 0 {
					a.send(msg.Chat.ID, fmt.Sprintf("⏳ Iltimos %d soniya kutib qayta urinib ko'ring.", wait))
					continue
				}
				if text == a.login {
					_ = services.RecordLoginSuccess(ctx, userID, services.ThrottleRoleSuperadmin)
					a.setLoggedIn(userID, "super")
					a.sendAdminPanel(msg.Chat.ID, userID)
					continue
				}
			}
			// Approved applicant or has credential: try login
			hasCred, _ := services.HasApprovedCredential(ctx, userID, services.UserRoleRestaurantAdmin)
			credExists, _ := services.CredentialExists(ctx, userID, services.UserRoleRestaurantAdmin)
			if hasCred || credExists {
				if wait, _ := services.LoginThrottleWaitSeconds(ctx, userID, services.ThrottleRoleRestaurantAdmin); wait > 0 {
					a.send(msg.Chat.ID, fmt.Sprintf("⏳ Iltimos %d soniya kutib qayta urinib ko'ring.", wait))
					continue
				}
				ok, _ := services.VerifyCredential(ctx, userID, services.UserRoleRestaurantAdmin, text)
				if ok {
					_ = services.RecordLoginSuccess(ctx, userID, services.ThrottleRoleRestaurantAdmin)
					services.MarkExpiredIfNeeded(ctx, userID, services.UserRoleRestaurantAdmin)
					subOk, subMsg := services.RequireActiveSubscription(ctx, userID, services.UserRoleRestaurantAdmin)
					if !subOk {
						a.sendExpiredUserAndNotifySuperadmin(msg.Chat.ID, userID, services.UserRoleRestaurantAdmin, subMsg)
						continue
					}
					a.setLoggedIn(userID, "branch")
					locID, _ := services.GetAdminLocationID(ctx, userID)
					a.stateMu.Lock()
					a.activeLocation[userID] = locID
					a.stateMu.Unlock()
					a.send(msg.Chat.ID, "✅ Kirish muvaffaqiyatli.")
					if within, warn := services.SubscriptionExpiresWithinDays(ctx, userID, services.UserRoleRestaurantAdmin, 3); within && warn != "" {
						a.send(msg.Chat.ID, warn)
					}
					a.sendAdminPanel(msg.Chat.ID, userID)
				} else {
					_ = services.RecordLoginFailed(ctx, userID, services.ThrottleRoleRestaurantAdmin)
					if correctInactive, _ := services.PasswordCorrectButInactive(ctx, userID, services.UserRoleRestaurantAdmin, text); correctInactive {
						a.sendExpiredUserAndNotifySuperadmin(msg.Chat.ID, userID, services.UserRoleRestaurantAdmin, adderSubscriptionExpiredMsg)
					} else {
						a.send(msg.Chat.ID, "❌ Noto'g'ri parol. "+adderLoginPrompt)
					}
				}
				continue
			}
			// Application status gating (only for users without approved credential)
			if status, _ := services.GetUserApplicationStatus(ctx, userID, services.ApplicationTypeRestaurantAdmin); status == services.ApplicationStatusPending {
				a.send(msg.Chat.ID, "⏳ Arizangiz ko'rib chiqilmoqda.")
				continue
			}
			if status, _ := services.GetUserApplicationStatus(ctx, userID, services.ApplicationTypeRestaurantAdmin); status == services.ApplicationStatusRejected {
				if a.superAdminID != 0 && userID == a.superAdminID && a.login != "" {
					if wait, _ := services.LoginThrottleWaitSeconds(ctx, userID, services.ThrottleRoleSuperadmin); wait > 0 {
						a.send(msg.Chat.ID, fmt.Sprintf("⏳ Iltimos %d soniya kutib qayta urinib ko'ring.", wait))
						continue
					}
					if text == a.login {
						_ = services.RecordLoginSuccess(ctx, userID, services.ThrottleRoleSuperadmin)
						a.setLoggedIn(userID, "super")
						a.sendAdminPanel(msg.Chat.ID, userID)
						continue
					}
				}
				if wait, _ := services.LoginThrottleWaitSeconds(ctx, userID, services.ThrottleRoleRestaurantAdmin); wait > 0 {
					a.send(msg.Chat.ID, fmt.Sprintf("⏳ Iltimos %d soniya kutib qayta urinib ko'ring.", wait))
					continue
				}
				if locID, ok, err := services.AuthenticateBranchAdmin(ctx, userID, text); err == nil && ok {
					_ = services.RecordLoginSuccess(ctx, userID, services.ThrottleRoleRestaurantAdmin)
					a.setLoggedIn(userID, "branch")
					a.stateMu.Lock()
					a.activeLocation[userID] = locID
					a.stateMu.Unlock()
					locName, _ := services.GetLocationName(ctx, locID)
					if locName != "" {
						a.send(msg.Chat.ID, "✅ Logged in to «"+locName+"». You can add or edit menu items for your place.")
					}
					a.sendAdminPanel(msg.Chat.ID, userID)
					continue
				}
				_ = services.RecordLoginFailed(ctx, userID, services.ThrottleRoleRestaurantAdmin)
				a.send(msg.Chat.ID, "❌ Ariza rad etildi. Qayta ariza uchun Zayafka botidan foydalaning.")
				continue
			}
			// Superadmin or branch admin: password prompt
			if a.superAdminID != 0 && userID == a.superAdminID && a.login != "" {
				if wait, _ := services.LoginThrottleWaitSeconds(ctx, userID, services.ThrottleRoleSuperadmin); wait > 0 {
					a.send(msg.Chat.ID, fmt.Sprintf("⏳ Iltimos %d soniya kutib qayta urinib ko'ring.", wait))
				} else if text == a.login {
					_ = services.RecordLoginSuccess(ctx, userID, services.ThrottleRoleSuperadmin)
					a.setLoggedIn(userID, "super")
					a.sendAdminPanel(msg.Chat.ID, userID)
				} else {
					_ = services.RecordLoginFailed(ctx, userID, services.ThrottleRoleSuperadmin)
					a.send(msg.Chat.ID, adderLoginPrompt)
				}
			} else {
				if wait, _ := services.LoginThrottleWaitSeconds(ctx, userID, services.ThrottleRoleRestaurantAdmin); wait > 0 {
					a.send(msg.Chat.ID, fmt.Sprintf("⏳ Iltimos %d soniya kutib qayta urinib ko'ring.", wait))
				} else if locID, ok, err := services.AuthenticateBranchAdmin(ctx, userID, text); err == nil && ok {
					_ = services.RecordLoginSuccess(ctx, userID, services.ThrottleRoleRestaurantAdmin)
					a.setLoggedIn(userID, "branch")
					a.stateMu.Lock()
					a.activeLocation[userID] = locID
					a.stateMu.Unlock()
					locName, _ := services.GetLocationName(ctx, locID)
					if locName != "" {
						a.send(msg.Chat.ID, "✅ Logged in to «"+locName+"». You can add or edit menu items for your place.")
					}
					a.sendAdminPanel(msg.Chat.ID, userID)
				} else {
					_ = services.RecordLoginFailed(ctx, userID, services.ThrottleRoleRestaurantAdmin)
					a.send(msg.Chat.ID, adderLoginPrompt)
				}
			}
			continue
		}

		// Branch admin: gate by branch subscription (expired => deny and log out; no password rotation)
		if a.getRole(userID) == "branch" {
			ctx := context.Background()
			locID, _ := services.GetAdminLocationID(ctx, userID)
			if locID != 0 {
				active, _ := services.LocationHasActiveSubscription(ctx, locID)
				if !active {
					services.MarkExpiredForBranch(ctx, locID)
					a.clearLoggedIn(userID)
					primaryID, _ := services.GetPrimaryAdminUserID(ctx, locID)
					a.sendExpiredUserAndNotifySuperadmin(msg.Chat.ID, primaryID, services.UserRoleRestaurantAdmin, adderSubscriptionExpiredMsg)
					continue
				}
			}
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

		// Expiry warning for branch admins (by primary's subscription)
		if a.getRole(userID) == "branch" {
			ctx := context.Background()
			locID, _ := services.GetAdminLocationID(ctx, userID)
			if locID != 0 {
				primaryID, _ := services.GetPrimaryAdminUserID(ctx, locID)
				if primaryID != 0 {
					if within, warn := services.SubscriptionExpiresWithinDays(ctx, primaryID, services.UserRoleRestaurantAdmin, 3); within && warn != "" {
						a.send(msg.Chat.ID, warn)
					}
				}
			}
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

// requireAdminLogin returns (true, "") if user may access admin panel; (false, msg) if not logged in or subscription expired. For branch, checks by branch subscription (primary's), not current user.
func (a *AdderBot) requireAdminLogin(chatID int64, userID int64) (allowed bool, denyMsg string) {
	if !a.isLoggedIn(userID) {
		return false, adderRequireLoginMsg
	}
	if a.getRole(userID) == "branch" {
		ctx := context.Background()
		locID, _ := services.GetAdminLocationID(ctx, userID)
		if locID != 0 {
			active, _ := services.LocationHasActiveSubscription(ctx, locID)
			if !active {
				services.MarkExpiredForBranch(ctx, locID)
				a.clearLoggedIn(userID)
				return false, adderSubscriptionExpiredMsg
			}
		}
	}
	return true, ""
}

// GetAPI returns the adder bot API.
func (a *AdderBot) GetAPI() *tgbotapi.BotAPI {
	return a.api
}

// SetZayafkaAPI sets the Zayafka bot API so adder can send password/rejection to applicants in the bot they used.
func (a *AdderBot) SetZayafkaAPI(api *tgbotapi.BotAPI) {
	a.zayafkaAPI = api
}

// SetOnSubscriptionRenewed sets the callback when a subscription is renewed (so background job can clear "already notified" for next expiry).
func (a *AdderBot) SetOnSubscriptionRenewed(f func(tgUserID int64, role string)) {
	a.onSubscriptionRenewed = f
}

// sendToSuperadminViaZayafka sends a message to superadmin in the Zayafka bot (so they get subscription alerts there too).
func (a *AdderBot) sendToSuperadminViaZayafka(text string, kb tgbotapi.InlineKeyboardMarkup) {
	if a.zayafkaAPI == nil || a.superAdminID == 0 {
		return
	}
	msg := tgbotapi.NewMessage(a.superAdminID, text)
	msg.ReplyMarkup = kb
	if _, err := a.zayafkaAPI.Send(msg); err != nil {
		log.Printf("adder: send to superadmin via zayafka: %v", err)
	}
}

// sendToApplicant sends a message to the applicant (prefer Zayafka so they see it where they applied).
func (a *AdderBot) sendToApplicant(chatID int64, text string) {
	if a.zayafkaAPI != nil {
		msg := tgbotapi.NewMessage(chatID, text)
		if _, err := a.zayafkaAPI.Send(msg); err != nil {
			log.Printf("adder: send to applicant via zayafka: %v", err)
		}
		return
	}
	a.send(chatID, text)
}

func (a *AdderBot) sendToApplicantWithInline(chatID int64, text string, kb tgbotapi.InlineKeyboardMarkup) {
	if a.zayafkaAPI != nil {
		msg := tgbotapi.NewMessage(chatID, text)
		msg.ReplyMarkup = kb
		if _, err := a.zayafkaAPI.Send(msg); err != nil {
			log.Printf("adder: send to applicant via zayafka: %v", err)
		}
		return
	}
	a.sendWithInline(chatID, text, kb)
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

func (a *AdderBot) sendWithReplyKeyboard(chatID int64, text string, kb tgbotapi.ReplyKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = kb
	if _, err := a.api.Send(msg); err != nil {
		log.Printf("adder send error: %v", err)
	}
}

func (a *AdderBot) sendRemoveKeyboard(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = tgbotapi.NewRemoveKeyboard(true)
	if _, err := a.api.Send(msg); err != nil {
		log.Printf("adder send error: %v", err)
	}
}

// sendExpiredUserAndNotifySuperadmin sends expiry message to user with "contact superadmin" button, and notifies superadmin once with "renew" button.
func (a *AdderBot) sendExpiredUserAndNotifySuperadmin(userChatID int64, tgUserID int64, role string, denyMsg string) {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📋 Yangilash so'rovini yuborish", "exp_contact")),
	)
	a.sendWithInline(userChatID, denyMsg, kb)

	key := fmt.Sprintf("%d:%s", tgUserID, role)
	a.stateMu.Lock()
	already := a.expiredNotified[key]
	if !already {
		a.expiredNotified[key] = true
	}
	a.stateMu.Unlock()
	if already || a.superAdminID == 0 {
		return
	}
	superMsg := fmt.Sprintf("❌ Abonement tugadi: tg_user_id=%d role=%s\n\nYangi parol berish va abonement yangilash uchun quyidagi tugmani bosing.", tgUserID, role)
	superKb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("✅ Yangilash (parol aylantirish)", fmt.Sprintf("exp_renew:%d:%s", tgUserID, role))),
	)
	a.sendWithInline(a.superAdminID, superMsg, superKb)
	a.sendToSuperadminViaZayafka(superMsg, superKb)
}

// SendRenewalRequestToSuperadmin sends a "renewal request" message to superadmin (when expired user presses the button).
func (a *AdderBot) SendRenewalRequestToSuperadmin(tgUserID int64, role string) {
	adminChatID := a.superAdminID
	if adminChatID == 0 && a.cfg != nil {
		adminChatID = a.cfg.Telegram.SuperadminID
	}
	if adminChatID == 0 {
		log.Printf("adder: SendRenewalRequestToSuperadmin skipped — superadmin ID not set (set ADMIN_ID or SUPERADMIN_TG_ID in .env)")
		return
	}
	superMsg := fmt.Sprintf("📩 Yangilash so'rovi: tg_user_id=%d role=%s\n\nYangi parol berish va abonement yangilash uchun quyidagi tugmani bosing.", tgUserID, role)
	superKb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("✅ Yangilash (parol aylantirish)", fmt.Sprintf("exp_renew:%d:%s", tgUserID, role))),
	)
	msg := tgbotapi.NewMessage(adminChatID, superMsg)
	msg.ReplyMarkup = superKb
	if _, err := a.api.Send(msg); err != nil {
		log.Printf("adder: failed to send renewal request to superadmin (chat_id=%d): %v — ensure superadmin has started the Qo'shuvchi (adder) bot", adminChatID, err)
		return
	}
	a.sendToSuperadminViaZayafka(superMsg, superKb)
	log.Printf("adder: renewal request sent to superadmin (chat_id=%d) for tg_user_id=%d role=%s", adminChatID, tgUserID, role)
}

// NotifyExpiredUser sends the subscription-expired message with renewal button to the user (used by background job; for restaurant_admin sends via Zayafka when set).
func (a *AdderBot) NotifyExpiredUser(chatID int64, tgUserID int64, role string) {
	if chatID == 0 {
		return
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📋 Yangilash so'rovini yuborish", "exp_contact")),
	)
	a.sendToApplicantWithInline(chatID, services.SubscriptionDenyMessage, kb)
}

// SendExpiredNotificationToSuperadmin is called from driver bot (or elsewhere) when a driver's subscription expires.
func (a *AdderBot) SendExpiredNotificationToSuperadmin(tgUserID int64, role string) {
	if a.superAdminID == 0 {
		return
	}
	key := fmt.Sprintf("%d:%s", tgUserID, role)
	a.stateMu.Lock()
	already := a.expiredNotified[key]
	if !already {
		a.expiredNotified[key] = true
	}
	a.stateMu.Unlock()
	if already {
		return
	}
	superMsg := fmt.Sprintf("❌ Abonement tugadi: tg_user_id=%d role=%s\n\nYangi parol berish va abonement yangilash uchun quyidagi tugmani bosing.", tgUserID, role)
	superKb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("✅ Yangilash (parol aylantirish)", fmt.Sprintf("exp_renew:%d:%s", tgUserID, role))),
	)
	a.sendWithInline(a.superAdminID, superMsg, superKb)
	a.sendToSuperadminViaZayafka(superMsg, superKb)
}

// HandleExpRenewFromZayafka runs renewal when superadmin taps the renew button in Zayafka; replyChatID is where to send confirmation (via Zayafka). For restaurant_admin only reactivates (no new password).
func (a *AdderBot) HandleExpRenewFromZayafka(tgUserID int64, role string, replyChatID int64) {
	ctx := context.Background()
	newPass, err := services.RenewSubscription(ctx, tgUserID, role, 1, a.superAdminID, nil, "")
	if err != nil {
		a.sendToApplicant(replyChatID, "❌ "+err.Error())
		return
	}
	a.stateMu.Lock()
	delete(a.expiredNotified, fmt.Sprintf("%d:%s", tgUserID, role))
	a.stateMu.Unlock()
	if a.onSubscriptionRenewed != nil {
		a.onSubscriptionRenewed(tgUserID, role)
	}
	if role == services.UserRoleRestaurantAdmin {
		a.sendToApplicant(replyChatID, "✅ Abonement yangilandi. Parol o'zgarmadi.")
	} else {
		subChatID, _ := services.GetChatIDForSubscriber(ctx, tgUserID, role)
		if subChatID != 0 {
			a.sendToApplicant(subChatID, newPass)
		}
		if subChatID != 0 {
			a.sendToApplicant(replyChatID, "✅ Yangilandi. Yangi parol Zayafka orqali foydalanuvchiga yuborildi.")
		} else {
			a.sendToApplicant(replyChatID, fmt.Sprintf("✅ Yangilandi. Yangi parol (qo'lda yuboring): %s", newPass))
		}
	}
}

func (a *AdderBot) handleApplyCommand(chatID int64, userID int64) {
	a.send(chatID, "📋 Ariza yuborish uchun Zayavka botidan foydalaning.")
}

func (a *AdderBot) handleApplicationsCommand(chatID int64, userID int64) {
	if a.superAdminID == 0 || userID != a.superAdminID {
		return
	}
	a.send(chatID, "📋 Arizalarni Zayavka botda ko'ring.")
}

func (a *AdderBot) handleAddDriver(chatID int64, args string) {
	args = strings.TrimSpace(args)
	if args == "" {
		a.send(chatID, "Ishlatish: /add_driver <tg_user_id>\nHaydovchini qo'shadi, parol yuboradi. Haydovchi driver botda /login qiladi.")
		return
	}
	var tgUserID int64
	if _, err := fmt.Sscanf(args, "%d", &tgUserID); err != nil || tgUserID <= 0 {
		a.send(chatID, "❌ tg_user_id raqam bo'lishi kerak.")
		return
	}
	ctx := context.Background()
	plainPass, err := services.AddDriverDirect(ctx, tgUserID)
	if err != nil {
		a.send(chatID, "❌ "+err.Error())
		return
	}
	a.send(chatID, fmt.Sprintf("✅ Haydovchi qo'shildi (tg_user_id=%d).\n\n🔑 Parol: %s\n\nBu parolni haydovchiga yuboring. U driver botda /login qiladi.", tgUserID, plainPass))
}

func (a *AdderBot) handleSubsPending(chatID int64) {
	ctx := context.Background()
	list, err := services.ListExpiredSubscriptions(ctx, 50)
	if err != nil {
		a.send(chatID, "❌ "+err.Error())
		return
	}
	if len(list) == 0 {
		a.send(chatID, "📭 Tugagan abonementlar yo'q.")
		return
	}
	var b strings.Builder
	b.WriteString("📋 Tugagan / tugashga yaqin abonementlar:\n\n")
	for _, r := range list {
		b.WriteString(fmt.Sprintf("• tg_user_id=%d role=%s tugadi=%s\n", r.TgUserID, r.Role, r.ExpiresAt.Format("2006-01-02")))
	}
	b.WriteString("\nYangilash: /renew <tg_user_id> <role> [oylar=1] [summa]")
	b.WriteString("\nParol yangilash: /reset_password <restaurant_id>")
	a.send(chatID, b.String())
}

func (a *AdderBot) handleRenew(chatID int64, superadminID int64, args string) {
	parts := strings.Fields(args)
	if len(parts) < 2 {
		a.send(chatID, "Ishlatish: /renew <tg_user_id> <role> [oylar=1] [summa]\nMisol: /renew 123456789 restaurant_admin 1 500000")
		return
	}
	var tgUserID int64
	if _, err := fmt.Sscanf(parts[0], "%d", &tgUserID); err != nil || tgUserID <= 0 {
		a.send(chatID, "❌ tg_user_id raqam bo'lishi kerak.")
		return
	}
	role := strings.ToLower(parts[1])
	if role != services.UserRoleRestaurantAdmin && role != services.UserRoleDriver {
		a.send(chatID, "❌ role: restaurant_admin yoki driver")
		return
	}
	days := 1
	if len(parts) >= 3 {
		if n, err := strconv.Atoi(parts[2]); err == nil && n > 0 {
			days = n
		}
	}
	var amount *float64
	if len(parts) >= 4 {
		if v, err := strconv.ParseFloat(parts[3], 64); err == nil && v >= 0 {
			amount = &v
		}
	}
	ctx := context.Background()
	newPass, err := services.RenewSubscription(ctx, tgUserID, role, days, superadminID, amount, "")
	if err != nil {
		a.send(chatID, "❌ "+err.Error())
		return
	}
	if role == services.UserRoleRestaurantAdmin {
		if a.onSubscriptionRenewed != nil {
			a.onSubscriptionRenewed(tgUserID, role)
		}
		a.send(chatID, fmt.Sprintf("✅ Abonement yangilandi (tg_user_id=%d, role=%s). Parol o'zgarmadi.", tgUserID, role))
	} else {
		userChatID, _ := services.GetChatIDForSubscriber(ctx, tgUserID, role)
		if userChatID != 0 {
			a.sendToApplicant(userChatID, newPass)
		}
		if a.onSubscriptionRenewed != nil {
			a.onSubscriptionRenewed(tgUserID, role)
		}
		a.send(chatID, fmt.Sprintf("✅ Abonement yangilandi (tg_user_id=%d, role=%s). Yangi parol Zayafka orqali foydalanuvchiga yuborildi.", tgUserID, role))
	}
}

func (a *AdderBot) handleResetPassword(chatID int64, args string) {
	args = strings.TrimSpace(args)
	if args == "" {
		a.send(chatID, "Ishlatish: /reset_password <restaurant_id>\nrestaurant_id = filial (location) ID raqami.")
		return
	}
	var branchLocationID int64
	if _, err := fmt.Sscanf(args, "%d", &branchLocationID); err != nil || branchLocationID <= 0 {
		a.send(chatID, "❌ restaurant_id musbat raqam bo'lishi kerak.")
		return
	}
	ctx := context.Background()
	newPass, primaryTgUserID, err := services.ResetBranchAdminPassword(ctx, branchLocationID)
	if err != nil {
		a.send(chatID, "❌ "+err.Error())
		return
	}
	userChatID, _ := services.GetChatIDForSubscriber(ctx, primaryTgUserID, services.UserRoleRestaurantAdmin)
	if userChatID != 0 {
		a.sendToApplicant(userChatID, "🔄 Yangi parol: "+newPass+"\n(Superadmin parolni yangiladi.)")
	}
	locName, _ := services.GetLocationName(ctx, branchLocationID)
	a.send(chatID, fmt.Sprintf("✅ Parol yangilandi (filial_id=%d %s). Yangi parol Zayafka orqali yuborildi.", branchLocationID, locName))
}

func (a *AdderBot) handleSubInfo(chatID int64, args string) {
	parts := strings.Fields(args)
	if len(parts) < 2 {
		a.send(chatID, "Ishlatish: /sub_info <tg_user_id> <role>")
		return
	}
	var tgUserID int64
	if _, err := fmt.Sscanf(parts[0], "%d", &tgUserID); err != nil || tgUserID <= 0 {
		a.send(chatID, "❌ tg_user_id raqam bo'lishi kerak.")
		return
	}
	role := strings.ToLower(parts[1])
	if role != services.UserRoleRestaurantAdmin && role != services.UserRoleDriver {
		a.send(chatID, "❌ role: restaurant_admin yoki driver")
		return
	}
	ctx := context.Background()
	sub, err := services.GetSubscription(ctx, tgUserID, role)
	if err != nil {
		a.send(chatID, "❌ Abonement topilmadi.")
		return
	}
	msg := fmt.Sprintf("📋 tg_user_id=%d role=%s\nstatus=%s\nstart=%s\nexpires=%s",
		sub.TgUserID, sub.Role, sub.Status,
		sub.StartAt.Format("2006-01-02"), sub.ExpiresAt.Format("2006-01-02"))
	if sub.LastPaymentAt != nil {
		msg += "\nlast_payment=" + sub.LastPaymentAt.Format("2006-01-02")
	}
	a.send(chatID, msg)
}

func (a *AdderBot) handlePause(chatID int64, args string) {
	parts := strings.Fields(args)
	if len(parts) < 2 {
		a.send(chatID, "Ishlatish: /pause <tg_user_id> <role>")
		return
	}
	var tgUserID int64
	if _, err := fmt.Sscanf(parts[0], "%d", &tgUserID); err != nil || tgUserID <= 0 {
		a.send(chatID, "❌ tg_user_id raqam bo'lishi kerak.")
		return
	}
	role := strings.ToLower(parts[1])
	if role != services.UserRoleRestaurantAdmin && role != services.UserRoleDriver {
		a.send(chatID, "❌ role: restaurant_admin yoki driver")
		return
	}
	ctx := context.Background()
	if err := services.PauseSubscription(ctx, tgUserID, role); err != nil {
		a.send(chatID, "❌ "+err.Error())
		return
	}
	a.send(chatID, fmt.Sprintf("✅ Abonement to'xtatildi (tg_user_id=%d, role=%s).", tgUserID, role))
}

func (a *AdderBot) handleUnpause(chatID int64, args string) {
	parts := strings.Fields(args)
	if len(parts) < 2 {
		a.send(chatID, "Ishlatish: /unpause <tg_user_id> <role>")
		return
	}
	var tgUserID int64
	if _, err := fmt.Sscanf(parts[0], "%d", &tgUserID); err != nil || tgUserID <= 0 {
		a.send(chatID, "❌ tg_user_id raqam bo'lishi kerak.")
		return
	}
	role := strings.ToLower(parts[1])
	if role != services.UserRoleRestaurantAdmin && role != services.UserRoleDriver {
		a.send(chatID, "❌ role: restaurant_admin yoki driver")
		return
	}
	ctx := context.Background()
	if err := services.UnpauseSubscription(ctx, tgUserID, role); err != nil {
		a.send(chatID, "❌ "+err.Error())
		return
	}
	a.send(chatID, fmt.Sprintf("✅ Abonement davom ettirildi (tg_user_id=%d, role=%s).", tgUserID, role))
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
			} else if addFlow.Step == "password" {
				a.send(chatID, "🔑 Send the unique password for this branch admin.")
			} else if addFlow.Step == "order_lang" {
				kb := tgbotapi.NewInlineKeyboardMarkup(
					tgbotapi.NewInlineKeyboardRow(
						tgbotapi.NewInlineKeyboardButtonData("O'zbek — buyurtmalar o'zbekcha", "adder:branch_lang:uz"),
						tgbotapi.NewInlineKeyboardButtonData("Русский — заказы на русском", "adder:branch_lang:ru"),
					),
				)
				a.sendWithInline(chatID, "📩 In which language should this admin receive order notifications? Tap a button below.", kb)
			}
			return
		}
		a.sendAdminPanel(chatID, userID)
		return
	}

	// Not logged in: password only (application form is in Zayavka bot)
	a.send(chatID, adderLoginPrompt)
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
	role := a.getRole(userID)
	if role == "branch" {
		ctx := context.Background()
		locID, _ := services.GetAdminLocationID(ctx, userID)
		if locID != 0 {
			active, _ := services.LocationHasActiveSubscription(ctx, locID)
			if !active {
				services.MarkExpiredForBranch(ctx, locID)
				a.clearLoggedIn(userID)
				primaryID, _ := services.GetPrimaryAdminUserID(ctx, locID)
				a.sendExpiredUserAndNotifySuperadmin(chatID, primaryID, services.UserRoleRestaurantAdmin, adderSubscriptionExpiredMsg)
				return
			}
			primaryID, _ := services.GetPrimaryAdminUserID(ctx, locID)
			if primaryID != 0 {
				if within, warn := services.SubscriptionExpiresWithinDays(ctx, primaryID, services.UserRoleRestaurantAdmin, 3); within && warn != "" {
					a.send(chatID, warn)
				}
			}
		}
	}
	a.stateMu.RLock()
	locID := a.activeLocation[userID]
	a.stateMu.RUnlock()

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

	// Expired subscription: user sends renewal request to superadmin
	if data == "exp_contact" {
		// Caller is the expired user (branch admin in adder)
		a.SendRenewalRequestToSuperadmin(userID, services.UserRoleRestaurantAdmin)
		a.send(chatID, "✅ So'rovingiz superadmin ga yuborildi. Tez orada yangilash tugmasi orqali parol yuboriladi.")
		return
	}
	// Expired subscription: superadmin renews (rotate password, send to location admin / driver)
	if strings.HasPrefix(data, "exp_renew:") {
		rest := strings.TrimPrefix(data, "exp_renew:")
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) != 2 || a.superAdminID == 0 || userID != a.superAdminID {
			return
		}
		var tgUserID int64
		if _, err := fmt.Sscanf(parts[0], "%d", &tgUserID); err != nil || tgUserID <= 0 {
			a.send(chatID, "❌ Noto'g'ri format.")
			return
		}
		role := parts[1]
		if role != services.UserRoleRestaurantAdmin && role != services.UserRoleDriver {
			a.send(chatID, "❌ Noto'g'ri role.")
			return
		}
		ctx := context.Background()
		newPass, err := services.RenewSubscription(ctx, tgUserID, role, 1, userID, nil, "")
		if err != nil {
			a.send(chatID, "❌ "+err.Error())
			return
		}
		if role == services.UserRoleRestaurantAdmin {
			// Reactivate only; no new password
			a.stateMu.Lock()
			delete(a.expiredNotified, fmt.Sprintf("%d:%s", tgUserID, role))
			a.stateMu.Unlock()
			if a.onSubscriptionRenewed != nil {
				a.onSubscriptionRenewed(tgUserID, role)
			}
			a.send(chatID, "✅ Abonement yangilandi. Parol o'zgarmadi.")
		} else {
			subChatID, _ := services.GetChatIDForSubscriber(ctx, tgUserID, role)
			if subChatID != 0 {
				a.sendToApplicant(subChatID, newPass)
			}
			a.stateMu.Lock()
			delete(a.expiredNotified, fmt.Sprintf("%d:%s", tgUserID, role))
			a.stateMu.Unlock()
			if a.onSubscriptionRenewed != nil {
				a.onSubscriptionRenewed(tgUserID, role)
			}
			if subChatID != 0 {
				a.send(chatID, "✅ Yangilandi. Yangi parol Zayafka orqali foydalanuvchiga yuborildi.")
			} else {
				a.send(chatID, fmt.Sprintf("✅ Yangilandi. Yangi parol (qo'lda yuboring): %s", newPass))
			}
		}
		return
	}

	if data == "apply_start" {
		a.send(chatID, "📋 Ariza yuborish uchun Zayavka botidan foydalaning.")
		return
	}

	// Admin panel: require login and active subscription (applications/approve only in Zayavka).
	if ok, msg := a.requireAdminLogin(chatID, userID); !ok {
		a.send(chatID, msg)
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
	case strings.HasPrefix(data, "adder:branch_lang:"):
		// Language choice for branch admin order notifications (after password step)
		langCode := strings.TrimPrefix(data, "adder:branch_lang:")
		if langCode != "uz" && langCode != "ru" {
			return
		}
		a.stateMu.Lock()
		ab := a.addBranchAdmin[userID]
		a.stateMu.Unlock()
		if ab == nil || ab.Step != "order_lang" || ab.PendingPasswordHash == "" {
			a.send(chatID, "❌ Session expired. Please start again from Add Branch Admin.")
			return
		}
		ctx := context.Background()
		if err := services.AddBranchAdmin(ctx, ab.LocationID, ab.PendingAdminID, userID, ab.PendingPasswordHash, langCode); err != nil {
			a.send(chatID, "❌ Failed to add branch admin: "+err.Error())
		} else {
			a.send(chatID, fmt.Sprintf("✅ Branch admin (user ID %d) added. Order notifications will be in %s.", ab.PendingAdminID, map[string]string{"uz": "Uzbek", "ru": "Russian"}[langCode]))
		}
		a.stateMu.Lock()
		delete(a.addBranchAdmin, userID)
		a.stateMu.Unlock()
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
	case strings.HasPrefix(data, "adder:loc_lang:"):
		// Language for new location's branch admin order notifications
		langCode := strings.TrimPrefix(data, "adder:loc_lang:")
		if langCode != "uz" && langCode != "ru" {
			return
		}
		a.stateMu.Lock()
		st := a.locState[userID]
		a.stateMu.Unlock()
		if st == nil || st.Step != "order_lang" || st.PendingPasswordHash == "" {
			a.send(chatID, "❌ Session expired. Please start again from Add Fast Food Location.")
			return
		}
		ctx := context.Background()
		locID, err := services.CreateLocationWithAdmin(ctx, st.Name, st.Lat, st.Lon, st.PendingAdminID, userID, st.PendingPasswordHash, langCode)
		if err != nil {
			a.send(chatID, "Failed to save location + admin: "+err.Error())
			log.Printf("failed to create location with admin (name=%q): %v", st.Name, err)
		} else {
			a.send(chatID, fmt.Sprintf("✅ Saved fast food location \"%s\" (id %d) and assigned admin. Order notifications will be in %s.", st.Name, locID, map[string]string{"uz": "Uzbek", "ru": "Russian"}[langCode]))
		}
		a.stateMu.Lock()
		delete(a.locState, userID)
		a.activeLocation[userID] = locID
		a.stateMu.Unlock()
		a.clearLoggedIn(userID)
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
		a.stateMu.Lock()
		a.addBranchAdmin[userID] = &addBranchAdminState{
			LocationID:          ab.LocationID,
			PendingAdminID:      ab.PendingAdminID,
			PendingPasswordHash: passwordHash,
			Step:                "order_lang",
		}
		a.stateMu.Unlock()
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("O'zbek — buyurtmalar o'zbekcha", "adder:branch_lang:uz"),
				tgbotapi.NewInlineKeyboardButtonData("Русский — заказы на русском", "adder:branch_lang:ru"),
			),
		)
		a.sendWithInline(msg.Chat.ID, "📩 In which language should this admin receive order notifications?\n\nQaysi tilda ushbu admin buyurtma xabarlarini olsin?", kb)
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
		st.PendingPasswordHash = passwordHash
		st.Step = "order_lang"
		a.stateMu.Lock()
		a.locState[userID] = st
		a.stateMu.Unlock()
		kb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("O'zbek — buyurtmalar o'zbekcha", "adder:loc_lang:uz"),
				tgbotapi.NewInlineKeyboardButtonData("Русский — заказы на русском", "adder:loc_lang:ru"),
			),
		)
		a.sendWithInline(msg.Chat.ID, "📩 In which language should this admin receive order notifications?\n\nQaysi tilda ushbu admin buyurtma xabarlarini olsin?", kb)
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
