# Login protection: soft throttling (full spec and logic)

## Overview

- **Goal:** Reduce brute-force risk by slowing repeated failed attempts. No hard block/lockout (no “5 tries → 15 min block”).
- **Mechanism:** Soft cooldown only. After each failed login we set a short wait; the user must wait that many seconds before the next attempt is processed. Cooldown grows with consecutive failures (exponential backoff) and is capped at 30 seconds.
- **UX:** User always sees the same wait message: *"⏳ Iltimos {N} soniya kutib qayta urinib ko'ring."* (Please wait N seconds and try again.)
- **Security:** Plaintext passwords are never logged.

---

## 1. Database: `login_throttle`

**Migration:** `migrations/022_login_throttle.sql`

| Column           | Type        | Description |
|------------------|-------------|-------------|
| `tg_user_id`     | BIGINT      | Telegram user ID |
| `role`           | TEXT        | One of: `superadmin`, `restaurant_admin`, `driver` |
| `fail_count`     | INT         | Consecutive failed attempts (default 0) |
| `last_failed_at` | TIMESTAMPTZ | When the last failure happened (nullable) |
| `cooldown_until` | TIMESTAMPTZ | End of current cooldown (nullable) |
| `updated_at`     | TIMESTAMPTZ | Last row update |

- **PRIMARY KEY:** `(tg_user_id, role)` — throttle is per user and per role.
- **CHECK:** `role IN ('superadmin', 'restaurant_admin', 'driver')`.
- **Index:** `idx_login_throttle_cooldown` on `cooldown_until` WHERE `cooldown_until IS NOT NULL` (for queries that care about active cooldowns).

---

## 2. Throttle policy (logic)

### Constants (in `services/login_throttle.go`)

- **Roles:** `superadmin`, `restaurant_admin`, `driver`.
- **Cap:** `ThrottleCooldownCapSeconds = 30`.

### Cooldown formula

- **On each failed login:**
  - Increment `fail_count` for that `(tg_user_id, role)`.
  - Set:
    - `cooldown_seconds = min(30, 2^fail_count)`
    - `cooldown_until = now() + cooldown_seconds`
  - So: 1st fail → 2 s, 2nd → 4 s, 3rd → 8 s, 4th → 16 s, 5th+ → 30 s (capped).

- **On successful login:**
  - Set `fail_count = 0`, `last_failed_at = NULL`, `cooldown_until = NULL` for that `(tg_user_id, role)`.

- **On every login attempt (before checking password):**
  - If there is a row and `cooldown_until > now()`:
    - Do **not** process the login (do not verify password).
    - Respond with: **"⏳ Iltimos {N} soniya kutib qayta urinib ko'ring."** where `N = ceil(seconds until cooldown_until)` (rounded up, so at least 1).
  - If no row or `cooldown_until` is null or in the past:
    - Proceed to normal password check; on failure call `RecordLoginFailed`, on success call `RecordLoginSuccess`.

There is **no** other blocking (no “locked out for 15 minutes” or similar).

### Service API (`services/login_throttle.go`)

| Function | Purpose |
|----------|--------|
| `LoginThrottleWaitSeconds(ctx, tgUserID, role)` | Returns seconds to wait (0 if no cooldown). Used to decide whether to block the attempt and what `N` to show. |
| `RecordLoginFailed(ctx, tgUserID, role)` | Increment `fail_count`, set `cooldown_until = now() + min(30, 2^fail_count)` seconds. |
| `RecordLoginSuccess(ctx, tgUserID, role)` | Reset `fail_count`, clear `cooldown_until` (and `last_failed_at`). |
| `CooldownSecondsForFailCount(failCount int)` | Pure function `min(30, 2^failCount)` for tests. |

Wait seconds are computed as `time.Until(cooldown_until).Seconds() + 1` (integer, rounded up).

---

## 3. Where throttle is applied

### Adder bot (`bot/adder.go`)

Password-based login is throttled in these places (each path checks throttle **before** comparing/verifying password):

1. **Superadmin LOGIN password** (user is superadmin and has LOGIN password configured):
   - Before comparing `text == a.login`: throttle for role `superadmin`. If in cooldown → send wait message and skip.
   - On match: `RecordLoginSuccess(..., ThrottleRoleSuperadmin)`.
   - On no match (wrong password): `RecordLoginFailed(..., ThrottleRoleSuperadmin)`.
   - Used in: (a) main text handler when `userID == a.superAdminID` and `a.login != ""`, (b) rejected-app branch for superadmin, (c) final “Superadmin or branch admin” block.

2. **Restaurant admin (branch) credential login** (approved credential or credential row exists, or rejected-app / legacy branch):
   - Before `VerifyCredential` or `AuthenticateBranchAdmin`: throttle for role `restaurant_admin`. If in cooldown → send wait message and skip.
   - On success: `RecordLoginSuccess(..., ThrottleRoleRestaurantAdmin)`.
   - On failure (wrong password or reject path): `RecordLoginFailed(..., ThrottleRoleRestaurantAdmin)`.
   - Used in: (a) `hasCred || credExists` block (VerifyCredential), (b) rejected-app branch (AuthenticateBranchAdmin), (c) final “branch admin” branch (AuthenticateBranchAdmin).

No plaintext password is logged anywhere in these flows.

### Driver bot (`bot/driver.go`)

- **Driver credential login** (user is an approved driver but not yet logged in):
  - Before `VerifyCredential(ctx, userID, UserRoleDriver, text)`: throttle for role `driver`. If `LoginThrottleWaitSeconds(..., ThrottleRoleDriver) > 0` → send wait message and skip.
  - On success: `RecordLoginSuccess(ctx, userID, ThrottleRoleDriver)` then send driver panel.
  - On failure: `RecordLoginFailed(ctx, userID, ThrottleRoleDriver)` then “Noto'g'ri parol” + login prompt.

No plaintext password is logged.

---

## 4. User-facing message

Single message for all throttled attempts (both bots):

- **Uzbek:** `"⏳ Iltimos %d soniya kutib qayta urinib ko'ring."`
- **Meaning:** “Please wait N seconds and try again.”

`%d` is the number of seconds (at least 1) until the cooldown ends.

---

## 5. What we do *not* do

- No hard block or lockout (e.g. no “5 tries → 15 minutes blocked”).
- No logging of plaintext passwords.
- Throttle is per `(tg_user_id, role)`; failing as superadmin does not throttle the same user’s driver or restaurant_admin attempt.

---

## 6. Tests

**File:** `services/login_throttle_test.go`

- **Unit:** `TestCooldownSecondsForFailCount` — checks `min(30, 2^failCount)` for `failCount` 0..10 (e.g. 1, 2, 4, 8, 16, 30, 30, …).
- **Integration:** `TestLoginThrottle_Integration` (skipped when `-short` or no DB):
  - Success resets cooldown (wait = 0).
  - One failed attempt sets cooldown (wait > 0).
  - Wait does not exceed 30 seconds.
  - After cooldown expires, wait becomes 0.
  - Success after failure resets (wait = 0).
  - Multiple failures keep cooldown capped at 30 seconds.

Run:

- Unit (no DB): `go test ./services/... -run CooldownSecondsForFailCount -v`
- With DB: `go test ./services/... -run LoginThrottle -v` (integration runs if DB pool is set).

---

## 7. File reference

| Item | Path |
|------|------|
| Migration | `migrations/022_login_throttle.sql` |
| Throttle logic & API | `services/login_throttle.go` |
| Throttle tests | `services/login_throttle_test.go` |
| Adder bot usage | `bot/adder.go` (superadmin + restaurant_admin) |
| Driver bot usage | `bot/driver.go` (driver) |

Old rate-limit helpers (`RecordLoginAttempt`, `CountRecentFailedAttempts`, `LoginRateLimitCount`) in `services/user_credentials.go` are **not** used for login blocking anymore; they can be removed in a later cleanup if nothing else uses them.
