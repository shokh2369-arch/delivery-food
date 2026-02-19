# Order status notifications (customer)

When a restaurant admin changes order status (preparing / ready / completed), the customer receives a Telegram message in Uzbek.

## Message templates

Variables: `{{orderId}}`, order summary (Jami: X UZS).

| Status      | Text |
|------------|------|
| **preparing** | Sizning buyurtmangiz #{{orderId}} tayyorlanmoqda. Tez orada yetkaziladi. + Jami + "Keyingi qadam: tayyor bo'lganda sizga xabar beramiz." |
| **ready**     | Sizning buyurtmangiz #{{orderId}} hozir tayyor — yetkazib beruvchi olib ketishga tayyorlanmoqda. + Jami + "Keyingi qadam: yetkazib berish." |
| **completed** | Buyurtmangiz #{{orderId}} yetkazildi va yakunlandi. Yoqsa baho qoldiring! Rahmat. + Jami |

All messages include a short order summary (Jami: X UZS) and, for preparing/ready, the expected next step.

## Flow

1. Admin taps inline button on order card (e.g. "Start Preparing") → callback `order_status:{orderId}:{newStatus}`.
2. Server loads admin by Telegram user ID, ensures they are the branch admin for a location, and that `order.location_id == admin.location_id`.
3. `UpdateOrderStatus(orderId, newStatus, adminLocationID, actorID)` runs in a transaction: validates transition (new→preparing→ready→completed), updates `orders.status`, inserts `order_status_history`.
4. After commit: admin message is edited and callback is answered with "✅ Status updated."
5. Customer notification: if status is preparing/ready/completed, check de-dup (no same order+status in last 30s), then send Telegram message and insert into `messages` with `meta: { channel, sent_via: "order_status_notify", order_id, status }`.

## How to test

### Admin updates

1. Run bot and message bot (MESSAGE_TOKEN set). Create an order from a customer account (select location that has a branch admin).
2. As the **restaurant admin** (Telegram user that is branch admin for that location), open the chat with the message bot; you should see the order card with [Start Preparing] [Mark Ready].
3. Click **Start Preparing** → callback is handled, status becomes PREPARING, buttons change to [Mark Ready]. Customer receives: "Sizning buyurtmangiz #… tayyorlanmoqda..."
4. Click **Mark Ready** → status READY, button [Mark Completed]. Customer receives ready message.
5. Click **Mark Completed** → status COMPLETED, no buttons. Customer receives "Buyurtmangiz #… yetkazildi va yakunlandi..."
6. **Security:** As another user (not that branch admin), try to trigger the same callback (e.g. by forwarding or using a different bot token) → should get "Unauthorized" or "order does not belong to your restaurant" in callback answer.
7. **Invalid transition:** Try to send callback `order_status:ID:completed` when order is still `new` → callback answer should show "invalid status transition".

### Customer receives message

- Use a test customer account. Place an order for a location that has a branch admin. Then, as that branch admin, change status to preparing/ready/completed. The customer chat must receive the corresponding Uzbek message with order ID and total.
- **De-dup:** Change the same order to "preparing" twice within 30 seconds; the customer should receive only one "tayyorlanmoqda" message. After 30s, a second notification for the same status can be sent.

### Unit tests

```bash
go test ./services/... -v -run "TestValidStatusTransition|TestCustomerMessageForOrderStatus"
```

- `TestValidStatusTransition`: allowed transitions (new→preparing, preparing→ready, ready→completed) and invalid/skip transitions.
- `TestCustomerMessageForOrderStatus`: message contains order ID, total, and (for completed) Uzbek "yetkazildi".

### Integration test (optional)

With a test DB, run a full flow: create order, call `UpdateOrderStatus`, assert `orders.status` and one new row in `order_status_history` with correct `from_status`, `to_status`, `actor_id`. Mock or stub the Telegram send and assert `SendMessage` was called with the customer chat ID and the expected Uzbek text.

## DB

- **order_status_history:** `order_id`, `from_status`, `to_status`, `actor_id` (Telegram user ID), `created_at`.
- **messages:** outbound messages with `role = 'system/outbound'`, `meta` JSONB `{ "channel": "telegram", "sent_via": "order_status_notify", "order_id", "status" }`.

Apply migrations: `go run . migrate` (includes 013_order_status_history, 014_messages).
