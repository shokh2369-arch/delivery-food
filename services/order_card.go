package services

import (
	"fmt"
	"strconv"

	"food-telegram/lang"
	"food-telegram/models"
)

// OrderCardButton is one inline button (text + callback_data or url).
type OrderCardButton struct {
	Text         string
	CallbackData string
	URL          string // if set, use as URL button instead of callback
}

// OrderCardContent is the text and optional inline keyboard for an order card.
type OrderCardContent struct {
	Text    string
	Buttons [][]OrderCardButton
}

func statusLabelAdmin(langCode string, status string) string {
	switch status {
	case OrderStatusNew:
		return lang.T(langCode, "adm_status_new")
	case OrderStatusPreparing:
		return lang.T(langCode, "adm_status_preparing")
	case OrderStatusReady:
		return lang.T(langCode, "adm_status_ready")
	case OrderStatusAssigned:
		return lang.T(langCode, "adm_status_assigned")
	case OrderStatusPickedUp:
		return lang.T(langCode, "adm_status_picked_up")
	case OrderStatusDelivering:
		return lang.T(langCode, "adm_status_delivering")
	case OrderStatusCompleted:
		return lang.T(langCode, "adm_status_completed")
	case OrderStatusRejected:
		return lang.T(langCode, "adm_status_rejected")
	default:
		return status
	}
}

// BuildAdminCard returns full card text and inline keyboard for admin audience.
// If driver is assigned, includes driver info (name/phone/car). Buttons: Send to Delivery / Customer Pickup when ready and no type; Mark Completed when pickup and ready; no completed when delivery and driver assigned.
func BuildAdminCard(o *models.Order, driver *Driver, adminLang string) OrderCardContent {
	if adminLang == "" {
		adminLang = lang.Uz
	}
	statusLabel := statusLabelAdmin(adminLang, o.Status)
	text := fmt.Sprintf(lang.T(adminLang, "adm_order_id"), o.ID) + "\n\n"
	text += fmt.Sprintf(lang.T(adminLang, "adm_total"), o.ItemsTotal) + "\n"
	text += fmt.Sprintf(lang.T(adminLang, "adm_status"), statusLabel)
	if driver != nil {
		text += "\n\n" + lang.T(adminLang, "adm_driver_accepted")
		if driver.Phone != "" {
			text += "\nPhone: " + driver.Phone
		}
		if driver.CarPlate != "" {
			text += "\nCar: " + driver.CarPlate
		}
	} else if o.Status == OrderStatusReady && o.DeliveryType != nil && *o.DeliveryType == "delivery" {
		text += "\n\n⏳ Haydovchi kutilmoqda..."
	}

	var buttons [][]OrderCardButton
	switch o.Status {
	case OrderStatusNew:
		buttons = [][]OrderCardButton{
			{{Text: lang.T(adminLang, "adm_start_preparing"), CallbackData: "order_status:" + strconv.FormatInt(o.ID, 10) + ":" + OrderStatusPreparing}},
			{{Text: lang.T(adminLang, "adm_reject"), CallbackData: "order_status:" + strconv.FormatInt(o.ID, 10) + ":" + OrderStatusRejected}},
		}
	case OrderStatusPreparing:
		buttons = [][]OrderCardButton{
			{{Text: lang.T(adminLang, "adm_mark_ready"), CallbackData: "order_status:" + strconv.FormatInt(o.ID, 10) + ":" + OrderStatusReady}},
		}
	case OrderStatusReady:
		if o.DeliveryType == nil {
			buttons = [][]OrderCardButton{
				{{Text: lang.T(adminLang, "adm_send_delivery"), CallbackData: "delivery_type:" + strconv.FormatInt(o.ID, 10) + ":delivery"},
					{Text: lang.T(adminLang, "adm_customer_pickup"), CallbackData: "delivery_type:" + strconv.FormatInt(o.ID, 10) + ":pickup"}},
			}
		} else if *o.DeliveryType == "pickup" {
			buttons = [][]OrderCardButton{
				{{Text: lang.T(adminLang, "adm_mark_completed"), CallbackData: "order_status:" + strconv.FormatInt(o.ID, 10) + ":" + OrderStatusCompleted}},
			}
		}
		// delivery chosen: no buttons (driver will accept; no completed button when driver_id set)
	}
	return OrderCardContent{Text: text, Buttons: buttons}
}

// BuildCustomerCard returns full card text and optional Track Driver button when status is delivering.
func BuildCustomerCard(o *models.Order, driver *Driver, trackURL string) OrderCardContent {
	text := fmt.Sprintf("Buyurtma #%d\n\n", o.ID)
	text += fmt.Sprintf("🛒 Mahsulotlar: %d so'm\n", o.ItemsTotal)
	text += fmt.Sprintf("💵 Jami: %d so'm\n\n", o.GrandTotal)
	text += "Holat: "
	switch o.Status {
	case OrderStatusNew:
		text += "Yangi"
	case OrderStatusPreparing:
		text += "Tayyorlanmoqda"
	case OrderStatusReady:
		text += "Tayyor"
	case OrderStatusAssigned:
		text += "Haydovchi topildi"
	case OrderStatusPickedUp:
		text += "Olib ketildi"
	case OrderStatusDelivering:
		text += "Yo'lda"
	case OrderStatusCompleted:
		text += "Yetkazildi"
	case OrderStatusRejected:
		text += "Rad etildi"
	default:
		text += o.Status
	}
	if driver != nil {
		text += "\n\nHaydovchi"
		if driver.Phone != "" {
			text += "\n📞 " + driver.Phone
		}
		if driver.CarPlate != "" {
			text += "\n🚗 " + driver.CarPlate
		}
	}

	var buttons [][]OrderCardButton
	if o.Status == OrderStatusDelivering && trackURL != "" {
		buttons = [][]OrderCardButton{{{Text: "📍 Track Driver", URL: trackURL, CallbackData: ""}}}
	}
	return OrderCardContent{Text: text, Buttons: buttons}
}

// BuildDriverCard returns full card text and next-action buttons for driver.
func BuildDriverCard(o *models.Order, driverLang string) OrderCardContent {
	if driverLang == "" {
		driverLang = lang.Uz
	}
	text := fmt.Sprintf(lang.T(driverLang, "dr_active_header"), o.ID, o.ItemsTotal, o.DeliveryFee, o.GrandTotal, lang.T(driverLang, "dr_status_accepted"))
	switch o.Status {
	case OrderStatusAssigned:
		text = fmt.Sprintf(lang.T(driverLang, "dr_active_header"), o.ID, o.ItemsTotal, o.DeliveryFee, o.GrandTotal, lang.T(driverLang, "dr_status_accepted"))
	case OrderStatusPickedUp:
		text = fmt.Sprintf(lang.T(driverLang, "dr_active_header"), o.ID, o.ItemsTotal, o.DeliveryFee, o.GrandTotal, lang.T(driverLang, "dr_status_picked"))
	case OrderStatusDelivering:
		text = fmt.Sprintf(lang.T(driverLang, "dr_active_header"), o.ID, o.ItemsTotal, o.DeliveryFee, o.GrandTotal, lang.T(driverLang, "dr_status_delivering"))
	case OrderStatusCompleted:
		text = fmt.Sprintf(lang.T(driverLang, "dr_active_header_done"), o.ID, o.ItemsTotal, o.DeliveryFee, o.GrandTotal)
		return OrderCardContent{Text: text, Buttons: nil}
	default:
		text = fmt.Sprintf(lang.T(driverLang, "dr_active_header"), o.ID, o.ItemsTotal, o.DeliveryFee, o.GrandTotal, o.Status)
	}

	var buttons [][]OrderCardButton
	switch o.Status {
	case OrderStatusAssigned:
		buttons = [][]OrderCardButton{
			{{Text: lang.T(driverLang, "dr_mark_collected"), CallbackData: fmt.Sprintf("driver_status:%d:%s", o.ID, OrderStatusPickedUp)}},
		}
	case OrderStatusPickedUp:
		buttons = [][]OrderCardButton{
			{{Text: lang.T(driverLang, "dr_start_delivering"), CallbackData: fmt.Sprintf("driver_status:%d:%s", o.ID, OrderStatusDelivering)}},
		}
	case OrderStatusDelivering:
		buttons = [][]OrderCardButton{
			{{Text: lang.T(driverLang, "dr_order_completed_btn"), CallbackData: fmt.Sprintf("driver_status:%d:%s", o.ID, OrderStatusCompleted)}},
		}
	}
	buttons = append(buttons, []OrderCardButton{{Text: lang.T(driverLang, "dr_back"), CallbackData: "driver:back"}})
	return OrderCardContent{Text: text, Buttons: buttons}
}
