# Driver Layer Implementation

## Overview

Driver layer enables Yandex Eats-style delivery assignment: when orders become READY, nearby online drivers can accept and deliver them.

## Database Changes

### New Tables

**`drivers`**
- `id` (UUID PK)
- `tg_user_id` (BIGINT UNIQUE) - Telegram user ID
- `chat_id` (BIGINT) - Telegram chat ID
- `phone` (TEXT nullable)
- `car_plate` (TEXT nullable)
- `status` (TEXT: 'online'/'offline', default 'offline')
- `created_at`, `updated_at`

**`driver_locations`**
- `driver_id` (UUID FK, PK)
- `lat`, `lon` (DOUBLE PRECISION)
- `updated_at` (TIMESTAMPTZ)
- Index on `updated_at` for recent location queries

### Orders Table Alterations

- `driver_id` (UUID nullable FK to drivers.id)
- `assigned_at` (TIMESTAMPTZ nullable)
- Indexes: `(status, location_id)`, `driver_id`, `assigned_at`

## Driver Bot (DRIVER_BOT_TOKEN)

### Commands & Flow

**`/start`**
- Registers driver (creates or updates `drivers` record)
- Shows driver panel with buttons:
  - 🟢 Go Online
  - 🔴 Go Offline
  - 📋 Jobs Near Me
  - 📦 My Active Order

**Go Online**
- Sets `driver.status = 'online'`
- Requests live location sharing (persistent keyboard)
- Updates `driver_locations` on each location message
- Location must be updated within 5 minutes to be considered "recent"

**Go Offline**
- Sets `driver.status = 'offline'`
- Removes location keyboard

**Jobs Near Me**
- Requires: driver is online AND has recent location (≤5 min)
- Queries READY orders (`status='ready'`, `driver_id IS NULL`) within 5km radius
- Shows top 5 orders with:
  - Order ID
  - Distance (km)
  - Total (UZS)
  - [Accept Order #ID] button
- Uses Haversine distance calculation in SQL

**My Active Order**
- Shows the driver's active order (status='ready', assigned to this driver)
- Displays order ID, total
- [Mark Delivered] button

**Accept Order** (`driver_accept:{orderId}`)
- Atomic transaction: `UPDATE orders SET driver_id=$driverId, assigned_at=NOW() WHERE id=$orderId AND status='ready' AND driver_id IS NULL`
- If 0 rows affected → "This order is already taken"
- On success:
  - Notifies customer: "Buyurtmangiz #ID uchun yetkazib beruvchi topildi." (via main bot TOKEN)
  - Notifies branch admin: "Driver assigned to order #ID." (via MESSAGE_TOKEN)
  - Saves messages to `messages` table

**Mark Delivered** (`driver_done:{orderId}`)
- Validates: order is assigned to this driver AND status='ready'
- Transaction:
  - `UPDATE orders SET status='completed'`
  - `INSERT order_status_history` (actor_id = driver's tg_user_id)
- Notifies customer: "Buyurtmangiz #ID yetkazildi va yakunlandi. Rahmat!" (via main bot TOKEN)
- Saves message to `messages` table

## Security & Isolation

- **Driver visibility**: Only sees READY orders with `driver_id IS NULL` within 5km radius
- **Accept race condition**: Atomic UPDATE prevents double assignment (PostgreSQL row-level locking)
- **Completion**: Only assigned driver can complete (`WHERE driver_id = $driverId`)
- **Status validation**: Driver completion requires `status='ready'` (can't skip states)

## Integration Points

### Order Status Flow

```
new → preparing → ready → completed
                    ↑
                    │
              [Driver accepts]
                    │
                    ↓
              driver_id assigned
                    │
                    ↓
              [Driver marks delivered]
                    │
                    ↓
              completed
```

### Branch Admin Behavior (MVP)

- Branch admin can still mark order as "completed" even if driver is assigned
- Both paths valid: driver completion OR admin completion
- Status transition validation ensures correct flow

## Service Functions

**`services/driver.go`**
- `RegisterDriver(ctx, tgUserID, chatID)` - Create/update driver
- `GetDriverByTgUserID(ctx, tgUserID)` - Load driver
- `UpdateDriverStatus(ctx, driverID, status)` - Set online/offline
- `UpdateDriverLocation(ctx, driverID, lat, lon)` - Update location
- `GetDriverLocation(ctx, driverID)` - Get recent location (≤5 min)
- `GetNearbyReadyOrders(ctx, lat, lon, radiusKm, limit)` - Find orders within radius
- `AcceptOrder(ctx, orderID, driverID)` - Atomic assignment (prevents double assign)
- `GetDriverActiveOrder(ctx, driverID)` - Get assigned ready order
- `CompleteDeliveryByDriver(ctx, orderID, driverID, driverTgUserID)` - Mark delivered

## Configuration

Add to `.env`:
```env
DRIVER_BOT_TOKEN=1234567890:ABC...  # Driver bot token from @BotFather
```

## Testing

**Unit Tests** (`services/driver_test.go`)
- `TestAcceptOrderRaceCondition`: Documents atomic UPDATE prevents double assignment
- `TestCompleteDeliveryByDriver`: Documents driver-only completion validation

**Integration Tests** (requires test DB)
- Two drivers compete for same order → only one succeeds
- Driver can only complete their assigned order
- Location-based order filtering works correctly

## Debugging

### Check READY Orders in Database

```sql
-- Count all READY orders without driver
SELECT COUNT(*) FROM orders 
WHERE status = 'ready' AND driver_id IS NULL AND lat IS NOT NULL AND lon IS NOT NULL;

-- List READY orders with coordinates
SELECT id, lat, lon, items_total, grand_total, created_at 
FROM orders 
WHERE status = 'ready' AND driver_id IS NULL AND lat IS NOT NULL AND lon IS NOT NULL
ORDER BY created_at DESC;

-- Check driver locations (recent within 5 min)
SELECT d.tg_user_id, d.status, dl.lat, dl.lon, dl.updated_at
FROM drivers d
LEFT JOIN driver_locations dl ON d.id = dl.driver_id
WHERE d.status = 'online' AND dl.updated_at > now() - interval '5 minutes';
```

### Debug Logs

When driver clicks "Jobs Near Me", logs show:
- `driver_id`, `status`
- Driver location: `lat`, `lon`, `updated_at`
- Radius used (default 5km)
- Count of READY orders before distance filtering
- Count after filtering (within radius)

## Usage Flow

1. **Driver Registration**
   ```
   Driver → /start → Registered (status: offline)
   ```

2. **Go Online**
   ```
   Driver → Go Online → Share Location → Status: online, location tracked
   ```

3. **Find Jobs**
   ```
   Driver → Jobs Near Me → See nearby READY orders → Accept Order #ID
   ```

4. **Accept Order**
   ```
   Driver → Accept → Order assigned → Customer & Admin notified
   ```

5. **Complete Delivery**
   ```
   Driver → My Active Order → Mark Delivered → Order completed → Customer notified
   ```

## Notes

- **Location Updates**: Drivers should update location every 5 minutes to stay visible
- **No Push Notifications**: Drivers check "Jobs Near Me" manually (pull model)
- **Distance Calculation**: Haversine formula in SQL (6371 km Earth radius)
- **Race Condition**: PostgreSQL atomic UPDATE ensures only one driver can accept
- **MVP Scope**: Branch admin can still complete orders (both paths allowed)
