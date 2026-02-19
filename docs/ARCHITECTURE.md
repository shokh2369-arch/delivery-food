# Food Telegram Bot - Complete Architecture & Features Overview

## 📋 Table of Contents
1. [Project Overview](#project-overview)
2. [Architecture](#architecture)
3. [Database Schema](#database-schema)
4. [Features](#features)
5. [Bot Flows](#bot-flows)
6. [Code Structure](#code-structure)
7. [Configuration](#configuration)
8. [Deployment](#deployment)

---

## 🎯 Project Overview

A multi-bot Telegram food delivery system with:
- **Customer Bot** (TOKEN): Order placement, menu browsing, location selection
- **Admin Adder Bot** (ADDER_TOKEN): Restaurant admin panel for menu/location management
- **Message Bot** (MESSAGE_TOKEN): Order notifications to restaurant admins

**Language**: Go 1.21+  
**Database**: PostgreSQL (pgx/v5)  
**Telegram API**: go-telegram-bot-api/v5

---

## 🏗️ Architecture

### High-Level Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Telegram Platform                         │
├─────────────────────────────────────────────────────────────┤
│                                                              │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐    │
│  │ Customer Bot │  │  Adder Bot   │  │ Message Bot  │    │
│  │   (TOKEN)    │  │(ADDER_TOKEN) │  │(MESSAGE_TOKEN│    │
│  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘    │
│         │                  │                  │             │
└─────────┼──────────────────┼──────────────────┼─────────────┘
          │                  │                  │
          ▼                  ▼                  ▼
┌─────────────────────────────────────────────────────────────┐
│                    Application Layer                         │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐   │
│  │   Bot    │  │  Adder   │  │ Services │  │  Models  │   │
│  │ Handlers │  │  Handlers│  │  Layer   │  │          │   │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬────┘   │
└───────┼──────────────┼──────────────┼─────────────┼────────┘
        │              │              │             │
        └──────────────┴──────────────┴─────────────┘
                           │
                           ▼
        ┌──────────────────────────────────┐
        │      PostgreSQL Database          │
        │  - orders                        │
        │  - menu_items                    │
        │  - locations                     │
        │  - branch_admins                 │
        │  - carts, checkouts              │
        │  - order_status_history          │
        │  - messages                      │
        └──────────────────────────────────┘
```

### Component Responsibilities

**Main Bot (`bot/bot.go`)**
- Customer interactions (menu, cart, checkout, orders)
- Location sharing and selection
- Order creation and notifications
- Admin commands (`/override`, `/stats`, `/promote`, etc.)

**Adder Bot (`bot/adder.go`)**
- Restaurant admin authentication (big admin + branch admins)
- Menu item management (add/delete food/drink/dessert)
- Location management (add/change/delete restaurants)
- Branch admin management (add/change admin per location)

**Message Bot (`bot/bot.go` - `startOrderStatusCallbacks`)**
- Receives order status callbacks from restaurant admins
- Updates order status (preparing → ready → completed)
- Sends customer notifications (via main bot TOKEN)

**Services Layer (`services/`)**
- Business logic separated from bot handlers
- Database operations abstracted
- Reusable across bots

---

## 🗄️ Database Schema

### Core Tables

#### `orders`
- **Purpose**: Customer orders with delivery info
- **Key Fields**: `id`, `user_id`, `chat_id`, `status` (new/preparing/ready/completed), `location_id` (FK), `items_total`, `delivery_fee`, `grand_total`, `lat`, `lon`, `distance_km`
- **Status Flow**: `new` → `preparing` → `ready` → `completed`
- **Indexes**: `created_at`, `status`, `location_id`

#### `menu_items`
- **Purpose**: Food/drink/dessert items
- **Key Fields**: `id`, `category` (food/drink/dessert), `name`, `price`, `location_id` (nullable, NULL = global)
- **Indexes**: `category`, `location_id`

#### `locations`
- **Purpose**: Restaurant branches (fast food locations)
- **Key Fields**: `id`, `name`, `lat`, `lon`, `created_at`
- **Usage**: Each location can have menu items and one branch admin

#### `branch_admins`
- **Purpose**: Restaurant admins (one per location)
- **Key Fields**: `id`, `branch_location_id` (FK, UNIQUE), `admin_user_id`, `password_hash` (bcrypt), `promoted_by`, `promoted_at`
- **Constraint**: One admin per location (`UNIQUE(branch_location_id)`)
- **Indexes**: `branch_location_id`, `admin_user_id`

#### `carts`
- **Purpose**: User shopping carts (temporary)
- **Key Fields**: `user_id` (PK), `items` (JSONB), `items_total`
- **Lifecycle**: Created on add-to-cart, deleted on checkout

#### `checkouts`
- **Purpose**: Checkout state (between cart and order)
- **Key Fields**: `user_id` (PK), `items` (JSONB), `items_total`, `phone`
- **Lifecycle**: Created before phone request, deleted after order creation

#### `user_locations`
- **Purpose**: User's selected restaurant location
- **Key Fields**: `user_id` (PK), `location_id` (FK)
- **Usage**: Determines which menu items to show

#### `order_status_history`
- **Purpose**: Audit trail of order status changes
- **Key Fields**: `id`, `order_id` (FK), `from_status`, `to_status`, `actor_id` (Telegram user ID), `created_at`
- **Index**: `order_id`

#### `messages`
- **Purpose**: Outbound system messages (order notifications)
- **Key Fields**: `id`, `chat_id`, `role` (system/outbound), `content`, `meta` (JSONB), `created_at`
- **Meta Format**: `{ "channel": "telegram", "sent_via": "order_status_notify", "order_id": 123, "status": "preparing" }`
- **Indexes**: `chat_id`, `created_at`, `meta` (for de-dup queries)

### Relationships

```
locations (1) ──< (many) menu_items
locations (1) ──< (1) branch_admins
locations (1) ──< (many) orders
orders (1) ──< (many) order_status_history
users (1) ──< (1) carts
users (1) ──< (1) checkouts
users (1) ──< (1) user_locations
```

---

## ✨ Features

### 1. Customer Features (Main Bot)

#### Order Placement Flow
1. **Start** → User shares location (required)
2. **Location Selection** → Choose restaurant branch (with distance calculation)
3. **Menu Browsing** → Categories: Food 🍽, Drinks 🥤, Desserts 🍰
4. **Add to Cart** → Inline buttons, cart persists in DB
5. **Checkout** → Review cart, confirm, share phone number
6. **Order Creation** → Order saved with `status = 'new'`, linked to location
7. **Confirmation** → Customer receives confirmation message

#### Location Features
- **Distance Calculation**: Haversine formula for nearest restaurants
- **Location Suggestions**: Paginated list with distance (km)
- **Manual Selection**: List all locations without distance
- **User Location Persistence**: Selected location stored in `user_locations`

#### Cart Management
- **Persistent Cart**: Stored in PostgreSQL (`carts` table)
- **Add Items**: Inline buttons per menu item
- **View Cart**: Shows items, quantities, total
- **Cart State**: Survives bot restarts

### 2. Restaurant Admin Features (Adder Bot)

#### Authentication
- **Big Admin** (ADMIN_ID): Uses `LOGIN` password from `.env`
- **Branch Admin**: Uses unique password (bcrypt) assigned per location
- **Session Management**: Logged out after each operation (security)

#### Menu Management (Branch Admins Only)
- **Add Items**: Food / Drink / Dessert (name → price flow)
- **List/Delete Items**: View items by category, delete with inline buttons
- **Location Scoped**: Items belong to admin's location (no global items for branch admins)

#### Location Management (Big Admin Only)
- **Add Location**: Name → Telegram location → Admin user ID → Unique password
- **Select Location**: Choose location to manage
- **Change Admin**: Remove current admin, assign new one with password
- **Delete Location**: Removes location + menu items + admin bindings

#### Security
- **One Admin Per Location**: Enforced by DB constraint
- **Location Isolation**: Branch admin can only manage their own location's menu
- **Password Uniqueness**: No two branch admins can have the same password

### 3. Order Status Management

#### Status Flow
```
new (waiting) → preparing → ready → completed
```

#### Restaurant Admin Actions
- **Start Preparing**: Changes status to `preparing`
- **Mark Ready**: Changes status to `ready`
- **Mark Completed**: Changes status to `completed`

#### Customer Notifications (Uzbek)
- **Preparing**: "Sizning buyurtmangiz #X tayyorlanmoqda. Tez orada yetkaziladi."
- **Ready**: "Sizning buyurtmangiz #X hozir tayyor — yetkazib beruvchi olib ketishga tayyorlanmoqda."
- **Completed**: "Buyurtmangiz #X yetkazildi va yakunlandi. Yoqsa baho qoldiring! Rahmat."

#### Features
- **Status History**: All changes logged in `order_status_history`
- **De-duplication**: Same status notification not sent within 30 seconds
- **Message Persistence**: All customer notifications saved in `messages` table
- **Security**: Admin can only update orders for their own restaurant (`order.location_id == admin.location_id`)

### 4. Admin Commands (Main Bot)

#### `/override <order_id> <new_fee> [note]`
- Override delivery fee for an order
- Requires `ADMIN_ID` (big admin)
- Updates `orders.delivery_fee`, `grand_total`, audit fields

#### `/stats [date]`
- Daily statistics: orders count, items revenue, delivery revenue, grand total, overrides count
- Default: today's date
- Requires `ADMIN_ID`

#### `/promote <branch_location_id> <admin_user_id> <password>`
- Add branch admin to a location with unique password
- Requires `ADMIN_ID` or existing branch admin
- Password must be unique across all branch admins

#### `/list_admins <branch_location_id>`
- List all admins for a branch
- Shows admin user IDs, promoted by, promoted at

#### `/remove_admin <branch_location_id> <admin_user_id>`
- Remove branch admin from a location
- Requires `ADMIN_ID` or branch admin of that location

---

## 🔄 Bot Flows

### Customer Order Flow

```
User → /start
  ↓
Share Location (required)
  ↓
Select Restaurant Location
  ↓
Browse Menu (Categories)
  ↓
Add Items to Cart (inline buttons)
  ↓
View Cart → Confirm
  ↓
Share Phone Number
  ↓
Order Created (status: 'new')
  ↓
Order Card Sent to Restaurant Admin (via MESSAGE_TOKEN)
  ↓
[Admin Updates Status]
  ↓
Customer Receives Status Notification (via TOKEN)
```

### Restaurant Admin Flow (Adder Bot)

```
Admin → /start (ADDER_TOKEN bot)
  ↓
Enter Password (LOGIN for big admin, unique password for branch admin)
  ↓
Admin Panel
  ├─ Branch Admin: Add/List/Delete Menu Items (their location only)
  └─ Big Admin:
      ├─ Select Location
      ├─ Add Location (name → location → admin ID → password)
      ├─ Change Admin (remove old, add new with password)
      └─ Delete Location
  ↓
After Each Operation → Logged Out (must re-enter password)
```

### Order Status Update Flow

```
Restaurant Admin → Clicks Button on Order Card
  ↓
Callback: order_status:{orderId}:{newStatus}
  ↓
Validate Admin Identity (GetAdminLocationID)
  ↓
Load Order from DB
  ↓
Validate: order.location_id == admin.location_id
  ↓
Validate Status Transition (new→preparing→ready→completed)
  ↓
Transaction:
  ├─ UPDATE orders SET status = newStatus
  └─ INSERT order_status_history (from_status, to_status, actor_id)
  ↓
Commit Transaction
  ↓
Edit Admin Message (update status line + buttons)
  ↓
Answer Callback Query ("✅ Status updated.")
  ↓
Check De-dup (same order+status in last 30s?)
  ↓
If Not Duplicate:
  ├─ Send Customer Notification (via TOKEN)
  └─ Save to messages table (meta: order_id, status, sent_via)
```

---

## 📁 Code Structure

```
food-telegram/
├── main.go                    # Entry point, migration command
├── migrations_embed.go        # Embedded SQL migrations
├── go.mod                     # Dependencies
│
├── bot/
│   ├── bot.go                 # Main customer bot handlers
│   └── adder.go               # Restaurant admin bot handlers
│
├── services/
│   ├── order.go               # Order creation, status updates, history
│   ├── menu.go                # Menu item CRUD
│   ├── cart.go                # Cart management
│   ├── location.go             # Location CRUD, distance calculation
│   ├── branch_admin.go         # Branch admin CRUD, authentication
│   ├── location_with_admin.go  # Create location + assign admin atomically
│   ├── user_location.go        # User location selection
│   ├── messages.go             # Outbound message persistence, de-dup
│   └── order_test.go          # Unit tests
│
├── models/
│   ├── order.go                # Order models, CreateOrderInput
│   ├── menu.go                 # MenuItem, categories
│   └── location.go             # Location model
│
├── db/
│   └── db.go                  # PostgreSQL connection pool
│
├── config/
│   └── config.go              # Configuration loading (.env)
│
├── migrations/
│   ├── 001_orders_delivery.sql
│   ├── 002_orders_phone.sql
│   ├── 003_menu_items.sql
│   ├── 004_carts.sql
│   ├── 005_checkouts.sql
│   ├── 006_locations.sql
│   ├── 007_user_locations.sql
│   ├── 008_menu_items_location.sql
│   ├── 009_branch_admins.sql
│   ├── 010_branch_admin_password.sql
│   ├── 011_one_admin_per_location.sql
│   ├── 012_orders_location_and_status.sql
│   ├── 013_order_status_history.sql
│   └── 014_messages.sql
│
└── docs/
    ├── ORDER_STATUS_NOTIFY.md # Order status notification docs
    └── ARCHITECTURE.md         # This file
```

### Key Design Patterns

**Service Layer Pattern**
- Business logic in `services/` package
- Database operations abstracted
- Reusable across multiple bots

**Repository Pattern** (implicit)
- Services act as repositories
- Direct DB access via `db.Pool`
- No separate repository layer

**State Machine Pattern**
- Order status transitions enforced
- Cart → Checkout → Order flow
- Admin operation flows (name → price, location → admin → password)

**Observer Pattern** (implicit)
- Order status changes trigger customer notifications
- Admin actions trigger UI updates

---

## ⚙️ Configuration

### Environment Variables (`.env`)

```env
# Database
DB_HOST=localhost
DB_PORT=5432
DB_USER=postgres
DB_PASSWORD=your_password
DB_NAME=delivery

# Telegram Bots
TOKEN=1234567890:ABC...              # Main customer bot
ADDER_TOKEN=1234567890:XYZ...        # Restaurant admin bot
MESSAGE_TOKEN=1234567890:DEF...      # Order notifications bot

# Admin
ADMIN_ID=123456789                   # Big admin Telegram user ID
LOGIN=your_admin_password            # Big admin password for adder bot

# Optional
AUTO_MIGRATE=1                       # Auto-run migrations on startup
```

### Configuration Structure

```go
type Config struct {
    DB       DBConfig       // PostgreSQL connection
    Telegram TelegramConfig // Bot tokens
    Delivery DeliveryConfig // Delivery fee rate
}
```

---

## 🚀 Deployment

### Prerequisites
- Go 1.21+
- PostgreSQL 12+
- Telegram Bot Tokens (from @BotFather)

### Setup Steps

1. **Clone & Install**
   ```bash
   git clone <repo>
   cd food-telegram
   go mod download
   ```

2. **Database Setup**
   ```bash
   # Create database
   createdb delivery
   
   # Configure .env
   cp .env.example .env
   # Edit .env with your DB credentials
   ```

3. **Run Migrations**
   ```bash
   go run . migrate
   ```

4. **Start Bot**
   ```bash
   go run .
   # Or build:
   go build -o food-telegram .
   ./food-telegram
   ```

### Production Considerations

- **Auto-Migration**: Set `AUTO_MIGRATE=1` for automatic migrations
- **Logging**: Uses Go `log` package (consider structured logging)
- **Error Handling**: Errors logged, callbacks answered with error messages
- **Concurrency**: Goroutines for adder bot and message bot callbacks
- **Database Pool**: pgxpool handles connection pooling
- **State Management**: In-memory maps with mutexes for user state (not persistent)

---

## 🔐 Security Features

1. **Admin Authentication**
   - Big admin: Password from `.env`
   - Branch admins: Unique bcrypt passwords (one per location)

2. **Authorization**
   - Branch admin can only manage their own location's menu
   - Order updates validated: `order.location_id == admin.location_id`
   - Admin commands require `ADMIN_ID` or branch admin role

3. **Password Security**
   - Passwords hashed with bcrypt (cost: DefaultCost)
   - Password uniqueness enforced (no duplicates)
   - Session logout after each operation

4. **Data Validation**
   - Status transitions validated (no skipping states)
   - Order ownership checked before updates
   - All inputs validated (user IDs, order IDs, etc.)

---

## 📊 Key Metrics & Monitoring

### Database Tables for Analytics
- `orders`: Order volume, revenue (`items_total`, `delivery_fee`, `grand_total`)
- `order_status_history`: Status change frequency, admin activity
- `messages`: Notification delivery tracking
- `branch_admins`: Admin management audit trail

### Admin Commands for Monitoring
- `/stats`: Daily revenue and order counts
- `/list_admins`: Admin assignments per location

---

## 🧪 Testing

### Unit Tests
```bash
go test ./services/... -v
```

**Coverage**:
- `TestValidStatusTransition`: Status transition validation
- `TestCustomerMessageForOrderStatus`: Message template generation

### Integration Testing
- Requires test database
- Mock Telegram API or use test tokens
- Test full order flow: cart → checkout → order → status updates

---

## 🔄 Future Enhancements (Not Implemented)

- Driver assignment and notifications
- Payment integration
- Order rating/review system
- Multi-language support (currently Uzbek)
- Order cancellation flow
- Delivery tracking
- Admin dashboard (web interface)
- Analytics and reporting dashboard

---

## 📝 Notes

- **Language**: UI messages primarily in Uzbek
- **Currency**: UZS (Uzbekistani Som)
- **Distance**: Kilometers (Haversine formula)
- **Time Zone**: Server timezone (no explicit timezone handling)
- **State Management**: User state (carts, locations) stored in PostgreSQL, not Redis
- **Scalability**: Single-instance deployment (no distributed state)

---

**Last Updated**: 2026-02-19  
**Version**: Based on migrations 001-014
