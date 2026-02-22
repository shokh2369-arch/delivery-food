# Zayafka bot — full logic

## Overview

**Zayafka** (Ariza) is the **application-form-only** Telegram bot. Users submit **restaurant admin** applications here. New-application notifications are sent so the **superadmin** can **Approve** or **Reject** in the **Adder** bot (when Zayafka is run with Adder). Password and rejection messages are sent to the applicant **via Zayafka** so they see them in the bot where they applied.

- **Config:** `ZAYAFKA` token in env. Optional: run together with Adder (`ADDER_TOKEN`); then new-application message is sent **from Adder API** so Approve/Reject callbacks are handled in Adder.
- **Application type:** Restaurant admin only (driver applications use the Driver bot, not Zayafka).
- **File:** `bot/zayafka.go`. Services: `services/applications.go`.

---

## 1. Startup and wiring

- **main.go:** If `ZAYAFKA` is set, `NewZayafkaBot(cfg, adminID, adderAPI)` is called. `adderAPI` is Adder’s bot API when Adder is also started.
- **ZayafkaBot** holds:
  - `api` — Zayafka bot’s own API (receives updates, sends messages in Zayafka).
  - `adderAPI` — when set, new-application notification is sent **via Adder** so the message appears in Adder and callbacks `app_approve:` / `app_reject:` are handled in **Adder**, not in Zayafka.
- **Adder** gets Zayafka API via `SetZayafkaAPI(zayafka.GetAPI())`. Adder uses it to send **password** and **rejection** messages to applicants in Zayafka.
- **Renewal:** `zayafka.SetOnExpRenew(adder.HandleExpRenewFromZayafka)` — when superadmin taps “Yangilash” in Zayafka (expired subscription), Adder runs renewal and sends new password to the user (prefer Zayafka).

---

## 2. Application status (DB)

- **Table:** `applications` (+ `application_restaurant_details` for restaurant apps).
- **Statuses:** `pending` → superadmin reviews; `approved` → credential created, user can log in to Adder; `rejected` → user can re-apply (e.g. “Qayta ariza”).
- **GetUserApplicationStatus(ctx, tgUserID, ApplicationTypeRestaurantAdmin)** returns the latest application status for that user (or `""` if none).
- **Restriction:** One pending or approved restaurant_admin application per user; creating another returns error “sizda allaqachon ariza mavjud yoki tasdiqlangan”.

---

## 3. Commands and entry points

| Command / action | Behavior |
|------------------|----------|
| `/start` | If approved → “Siz allaqachon tasdiqlangansiz. Admin panelga kirish uchun Qo'shuvchi botida parolingizni yuboring.” Else → inline “📋 Ariza yuborish”. |
| `/apply` | Status gate: pending → “Arizangiz ko'rib chiqilmoqda.”; approved → same as start. Else → start apply flow (step `full_name`). |
| `/cancel` | Cancel apply flow, remove reply keyboard, “Bekor qilindi. /start yoki /apply bilan qaytadan boshlang.” |

---

## 4. Apply flow (restaurant admin) — step logic

State: **per-user** `zayafkaApplyState` in `applyRestaurant[userID]`: `Step`, `FullName`, `Phone`, `RestaurantName`, `Lat`, `Lon`.

1. **full_name**  
   - User sends text → stored as `FullName`, step → `phone`.  
   - Reply keyboard: “📱 Raqamni ulashish” + “❌ Bekor qilish”.

2. **phone**  
   - User sends **contact** (shared phone) → stored as `Phone`, step → `restaurant_name`; keyboard removed; “🏪 Restoran nomini yuboring:” with inline “❌ Bekor qilish”.  
   - Or user sends **text** → stored as `Phone`, same next step.

3. **restaurant_name**  
   - User sends text → stored as `RestaurantName`, step → `location`.  
   - Reply keyboard: “📍 Lokatsiyani ulashish” + “❌ Bekor qilish”.

4. **location**  
   - User sends **location** (lat/lon) → stored, step → `confirm`.  
   - Summary: “Ism / Tel / Restoran / Lokatsiya. Tasdiqlaysizmi?” + inline “✅ Arizani yuborish”.  
   - If user sends text on this step → “Iltimos, lokatsiyani tugma orqali yuboring.”

5. **confirm**  
   - Only **inline** “✅ Arizani yuborish” submits (text on confirm step just re-sends the confirm prompt).

**Submit (apply_confirm callback):**  
- `CreateApplicationRestaurant(ctx, userID, chatID, FullName, Phone, "uz", RestaurantName, Lat, Lon, nil)` → creates **pending** application and restaurant details.  
- Clear state.  
- “✅ Arizangiz qabul qilindi. Superadmin tekshiradi.”  
- If `superAdminID != 0`: **notify superadmin** (see below).

**Cancel:**  
- Text “❌ Bekor qilish” or inline “adder_cancel” → clear state, remove keyboard, “Bekor qilindi…”.

---

## 5. Notifying superadmin (new application)

- **notifyAdminNewApplication(appID, fullName, phone, restaurantName, lat, lon)**  
  - Message to `superAdminID`: “🆕 **Yangi restoran arizasi**” + name, phone, restaurant, coords.  
  - Inline: “✅ Tasdiqlash (parol yuboriladi)” → `app_approve:appID`, “❌ Rad etish” → `app_reject:appID`.  
  - **If `adderAPI != nil`:** message is sent with **Adder’s API** so it appears in **Adder** and the next update (button tap) goes to **Adder**.  
  - **If `adderAPI == nil`:** message is sent with Zayafka’s API; Zayafka handles `app_approve` / `app_reject` in its own callback handler.

---

## 6. Approve (✅ Tasdiqlash)

- **When Adder is used** (notification was sent via Adder): Adder’s callback handles `app_approve:appID`.  
- **When Zayafka handles it** (`adderAPI == nil`): Zayafka’s `handleCallback` does the same logic.

**Logic (same in both):**  
- `services.ApproveApplication(ctx, appID, userID)`:
  - Creates **location** (restaurant name, lat, lon).
  - **AddBranchAdmin**: links user to location, stores bcrypt password hash.
  - Upserts **user_credentials** (tg_user_id, role=restaurant_admin, hash, is_active=true).
  - **CreateSubscription** (e.g. 1 month).
  - Marks application **approved**.
  - Returns **plain password** (one-time).
- Send to **applicant** (`app.ChatID`): “✅ Tasdiqlandi. Parolingiz: {password}. Qo'shuvchi botida parolni yuboring.”  
  - Adder uses **Zayafka API** (`sendToApplicant`) so the user gets it in Zayafka.
- Send to superadmin: “✅ Ariza tasdiqlandi. Parol arizachiga yuborildi.”

---

## 7. Reject (❌ Rad etish) and reject-reason flow

- **When Adder is used:** Adder’s callback handles `app_reject:appID`: stores `rejectReasonAppID[userID] = appID`, asks “Sabab yuboring (yoki /skip standart sabab uchun):”.  
- **When Zayafka handles it:** Zayafka does the same: `rejectReasonAppID[userID] = appID`, same prompt.

**Reject-reason step (superadmin only):**  
- Any **text** from superadmin (or `/skip` / empty → reason = “Sabab ko'rsatilmadi.”):  
  - `services.RejectApplication(ctx, appID, userID, reason)` → status = `rejected`, reject_reason saved.  
  - To **applicant** (via Zayafka when Adder has zayafkaAPI): “Sizning so'rovnomangizda xatolik/to'lov qilinmaganligi bor. @nonfindable ga bog'laning” + inline “📋 Qayta ariza” → `apply_show_previous:app.ID`.  
  - To superadmin: “✅ Ariza rad etildi.”  
  - Clear `rejectReasonAppID[userID]`.

---

## 8. Callbacks (inline buttons)

| Callback data | Who | Behavior |
|---------------|-----|----------|
| `apply_start` | Any | Same as `/apply`: status gate, then start apply flow (step full_name). |
| `adder_cancel` | Any | Cancel apply flow, “Bekor qilindi…”. |
| `apply_confirm` | User in confirm step | Create application (pending), notify superadmin, “Arizangiz qabul qilindi…”. |
| `apply_show_previous:prevID` | Rejected user | Load previous application by ID; show summary; “✅ Arizani yuborish” → `apply_resubmit:prevID`, “📋 Yangi ariza” → `apply_start`. |
| `apply_resubmit:prevID` | Same user | Create **new** pending application with same data via `CreateApplicationRestaurant(..., prev app data)`; “Arizangiz qayta qabul qilindi.”; notify superadmin. |
| `app_approve:appID` | Superadmin | Handled in **Adder** when notification was from Adder; else in Zayafka. Approve, send password to applicant (via Zayafka), confirm to superadmin. |
| `app_reject:appID` | Superadmin | Handled in Adder or Zayafka. Ask reject reason; on next message → RejectApplication, notify applicant (via Zayafka) with “Qayta ariza”. |
| `exp_renew:tgUserID:role` | Superadmin | Only in Zayafka. Calls `onExpRenew(tgUserID, role, chatID)` → Adder’s `HandleExpRenewFromZayafka` (renew subscription, send new password to user via Zayafka). |

---

## 9. Status gating (after apply flow)

If the user is **not** in the apply flow and sends any message:

- **pending:** “⏳ Arizangiz ko'rib chiqilmoqda.”  
- **rejected:** “❌ Ariza rad etildi. Qayta topshirish: /apply” + inline “📋 Qayta ariza”.  
- **approved:** “✅ Siz allaqachon tasdiqlangansiz. Admin panelga kirish uchun Qo'shuvchi botida parolingizni yuboring.”  
- Else: “📋 Ariza yuborish uchun /apply bosing.”

---

## 10. Adder ↔ Zayafka integration summary

| Direction | Usage |
|-----------|--------|
| Zayafka → Adder | New-application notification sent **via Adder API** when `adderAPI` set → superadmin sees message in Adder; Approve/Reject callbacks handled in Adder. |
| Adder → Zayafka | Adder holds `zayafkaAPI`. When approving: password sent to applicant **via Zayafka**. When rejecting: rejection text + “Qayta ariza” sent **via Zayafka**. Renewal new password also sent via Zayafka when possible. |
| Renewal in Zayafka | Superadmin sees expired-subscription card in Zayafka with “Yangilash”; callback `exp_renew:tgUserID:role` → Adder’s `HandleExpRenewFromZayafka`; new password delivered to user via Zayafka. |

---

## 11. Services used (applications.go)

| Function | Purpose |
|----------|--------|
| **CreateApplicationRestaurant** | Insert `applications` (pending) + `application_restaurant_details`. Fails if user already has pending/approved restaurant_admin. Returns app ID. |
| **GetApplicationByID** | Load application + restaurant (or driver) details by ID. |
| **GetUserApplicationStatus** | Latest status for (tgUserID, appType). |
| **ApproveApplication** | Create location, AddBranchAdmin, user_credentials, subscription; mark app approved; return plain password. |
| **RejectApplication** | Set status=rejected, reviewed_by, reject_reason. |

---

## 12. File reference

| Item | Path |
|------|------|
| Zayafka bot | `bot/zayafka.go` |
| Adder (approve/reject, sendToApplicant, reject reason) | `bot/adder.go` |
| Application services | `services/applications.go` |
| Config (ZAYAFKA token) | `config/config.go` |
| Startup wiring | `main.go` |

Adder’s `/apply` responds with “📋 Ariza yuborish uchun Zayafka botidan foydalaning.” so all restaurant applications go through Zayafka.
