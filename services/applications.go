package services

import (
	"context"
	"errors"
	"fmt"

	"food-telegram/db"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	ApplicationTypeRestaurantAdmin = "restaurant_admin"
	ApplicationTypeDriver          = "driver"
	ApplicationStatusPending       = "pending"
	ApplicationStatusApproved      = "approved"
	ApplicationStatusRejected      = "rejected"
)

type Application struct {
	ID          string
	Type        string
	TgUserID    int64
	ChatID      int64
	FullName    string
	Phone       string
	Language    string
	Status      string
	ReviewedBy  *int64
	ReviewedAt  *string
	RejectReason *string
}

type ApplicationRestaurantDetails struct {
	ApplicationID  string
	RestaurantName string
	Lat            float64
	Lon            float64
	Address        *string
}

type ApplicationDriverDetails struct {
	ApplicationID string
	CarPlate      *string
	CarModel      *string
}

// CreateApplicationRestaurant creates a pending restaurant_admin application. Returns error if user already has pending or approved.
func CreateApplicationRestaurant(ctx context.Context, tgUserID, chatID int64, fullName, phone, lang, restaurantName string, lat, lon float64, address *string) (appID string, err error) {
	var id string
	err = db.Pool.QueryRow(ctx, `
		INSERT INTO applications (type, tg_user_id, chat_id, full_name, phone, language, status)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE NOT EXISTS (
			SELECT 1 FROM applications a
			WHERE a.tg_user_id = $2 AND a.type = $1 AND (a.status = 'pending' OR a.status = 'approved')
		)
		RETURNING id::text`,
		ApplicationTypeRestaurantAdmin, tgUserID, chatID, fullName, phone, coalesceLang(lang), ApplicationStatusPending,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("sizda allaqachon ariza mavjud yoki tasdiqlangan")
		}
		return "", err
	}
	_, err = db.Pool.Exec(ctx, `
		INSERT INTO application_restaurant_details (application_id, restaurant_name, lat, lon, address)
		VALUES ($1, $2, $3, $4, $5)`,
		id, restaurantName, lat, lon, address,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// CreateApplicationDriver creates a pending driver application.
func CreateApplicationDriver(ctx context.Context, tgUserID, chatID int64, fullName, phone, lang string, carPlate, carModel *string) (appID string, err error) {
	var id string
	err = db.Pool.QueryRow(ctx, `
		INSERT INTO applications (type, tg_user_id, chat_id, full_name, phone, language, status)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE NOT EXISTS (
			SELECT 1 FROM applications a
			WHERE a.tg_user_id = $2 AND a.type = $1 AND (a.status = 'pending' OR a.status = 'approved')
		)
		RETURNING id::text`,
		ApplicationTypeDriver, tgUserID, chatID, fullName, phone, coalesceLang(lang), ApplicationStatusPending,
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("sizda allaqachon ariza mavjud yoki tasdiqlangan")
		}
		return "", err
	}
	_, err = db.Pool.Exec(ctx, `
		INSERT INTO application_driver_details (application_id, car_plate, car_model)
		VALUES ($1, $2, $3)`,
		id, carPlate, carModel,
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

func coalesceLang(s string) string {
	if s == "ru" {
		return "ru"
	}
	return "uz"
}

// GetApplicationByID returns application with type-specific details.
func GetApplicationByID(ctx context.Context, id string) (*Application, *ApplicationRestaurantDetails, *ApplicationDriverDetails, error) {
	var app Application
	err := db.Pool.QueryRow(ctx, `
		SELECT id::text, type, tg_user_id, chat_id, COALESCE(full_name,''), COALESCE(phone,''), language, status,
		       reviewed_by, reviewed_at::text, reject_reason
		FROM applications WHERE id = $1`,
		id,
	).Scan(&app.ID, &app.Type, &app.TgUserID, &app.ChatID, &app.FullName, &app.Phone, &app.Language, &app.Status,
		&app.ReviewedBy, &app.ReviewedAt, &app.RejectReason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, err
	}
	var rest *ApplicationRestaurantDetails
	var drv *ApplicationDriverDetails
	if app.Type == ApplicationTypeRestaurantAdmin {
		var r ApplicationRestaurantDetails
		r.ApplicationID = id
		err = db.Pool.QueryRow(ctx, `SELECT restaurant_name, lat, lon, address FROM application_restaurant_details WHERE application_id = $1`, id).
			Scan(&r.RestaurantName, &r.Lat, &r.Lon, &r.Address)
		if err == nil {
			rest = &r
		}
	} else {
		var d ApplicationDriverDetails
		d.ApplicationID = id
		err = db.Pool.QueryRow(ctx, `SELECT car_plate, car_model FROM application_driver_details WHERE application_id = $1`, id).
			Scan(&d.CarPlate, &d.CarModel)
		if err == nil {
			drv = &d
		}
	}
	return &app, rest, drv, nil
}

// GetUserApplicationStatus returns status of the latest application for (tgUserID, type), or "" if none.
func GetUserApplicationStatus(ctx context.Context, tgUserID int64, appType string) (status string, err error) {
	err = db.Pool.QueryRow(ctx, `
		SELECT status FROM applications WHERE tg_user_id = $1 AND type = $2 ORDER BY created_at DESC LIMIT 1`,
		tgUserID, appType,
	).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return status, nil
}

// ListPendingApplications returns up to limit pending applications (newest first).
func ListPendingApplications(ctx context.Context, limit int) ([]Application, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.Pool.Query(ctx, `
		SELECT id::text, type, tg_user_id, chat_id, COALESCE(full_name,''), COALESCE(phone,''), language, status,
		       reviewed_by, reviewed_at::text, reject_reason
		FROM applications WHERE status = $1 ORDER BY created_at DESC LIMIT $2`,
		ApplicationStatusPending, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []Application
	for rows.Next() {
		var a Application
		if err := rows.Scan(&a.ID, &a.Type, &a.TgUserID, &a.ChatID, &a.FullName, &a.Phone, &a.Language, &a.Status,
			&a.ReviewedBy, &a.ReviewedAt, &a.RejectReason); err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

// ApproveApplication generates password, upserts user_credentials, for restaurant creates location+branch_admin, marks app approved, returns plain password.
func ApproveApplication(ctx context.Context, applicationID string, superadminTgID int64) (plainPassword string, err error) {
	app, rest, drv, err := GetApplicationByID(ctx, applicationID)
	if err != nil || app == nil || app.Status != ApplicationStatusPending {
		return "", fmt.Errorf("application not found or not pending")
	}
	plainPassword, err = GenerateSecurePassword()
	if err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	if app.Type == ApplicationTypeRestaurantAdmin && rest != nil {
		var locID int64
		err = db.Pool.QueryRow(ctx, `
			INSERT INTO locations (name, lat, lon) VALUES ($1, $2, $3) RETURNING id`,
			rest.RestaurantName, rest.Lat, rest.Lon,
		).Scan(&locID)
		if err != nil {
			return "", fmt.Errorf("create location: %w", err)
		}
		err = AddBranchAdmin(ctx, locID, app.TgUserID, superadminTgID, string(hash), app.Language)
		if err != nil {
			return "", fmt.Errorf("add branch admin: %w", err)
		}
	}
	if app.Type == ApplicationTypeDriver && drv != nil {
		_, err = RegisterDriver(ctx, app.TgUserID, app.ChatID)
		if err != nil {
			return "", fmt.Errorf("register driver: %w", err)
		}
		if drv.CarPlate != nil && *drv.CarPlate != "" {
			driver, _ := GetDriverByTgUserID(ctx, app.TgUserID)
			if driver != nil {
				_ = UpdateDriverCar(ctx, driver.ID, *drv.CarPlate)
			}
		}
	}

	_, err = db.Pool.Exec(ctx, `
		INSERT INTO user_credentials (tg_user_id, role, password_hash, is_active, updated_at)
		VALUES ($1, $2, $3, true, now())
		ON CONFLICT (tg_user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash, role = EXCLUDED.role, is_active = true, updated_at = now()`,
		app.TgUserID, app.Type, string(hash),
	)
	if err != nil {
		return "", fmt.Errorf("upsert user_credentials: %w", err)
	}

	if app.Type == ApplicationTypeRestaurantAdmin {
		if err := CreateSubscription(ctx, app.TgUserID, app.Type, 1); err != nil {
			return "", fmt.Errorf("create subscription: %w", err)
		}
	}
	// Drivers: no subscription

	_, err = db.Pool.Exec(ctx, `
		UPDATE applications SET status = $1, reviewed_by = $2, reviewed_at = now(), updated_at = now() WHERE id = $3`,
		ApplicationStatusApproved, superadminTgID, applicationID,
	)
	if err != nil {
		return "", fmt.Errorf("mark approved: %w", err)
	}
	return plainPassword, nil
}

// AddDriverDirect adds a driver without an application: creates driver record (chat_id=0) and credential. No subscription for drivers. Superadmin shares the returned password with the driver if using password auth; drivers normally self-register (no password).
func AddDriverDirect(ctx context.Context, tgUserID int64) (plainPassword string, err error) {
	plainPassword, err = GenerateSecurePassword()
	if err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	if _, err = RegisterDriver(ctx, tgUserID, 0); err != nil {
		return "", fmt.Errorf("register driver: %w", err)
	}
	_, err = db.Pool.Exec(ctx, `
		INSERT INTO user_credentials (tg_user_id, role, password_hash, is_active, updated_at)
		VALUES ($1, $2, $3, true, now())
		ON CONFLICT (tg_user_id) DO UPDATE SET password_hash = EXCLUDED.password_hash, role = EXCLUDED.role, is_active = true, updated_at = now()`,
		tgUserID, ApplicationTypeDriver, string(hash),
	)
	if err != nil {
		return "", fmt.Errorf("upsert user_credentials: %w", err)
	}
	// Drivers: no subscription
	return plainPassword, nil
}

func ptrStr(s *string) string {
	if s != nil {
		return *s
	}
	return ""
}

// RejectApplication marks application rejected and sets reason.
func RejectApplication(ctx context.Context, applicationID string, superadminTgID int64, reason string) error {
	res, err := db.Pool.Exec(ctx, `
		UPDATE applications SET status = $1, reviewed_by = $2, reviewed_at = now(), reject_reason = $3, reject_in_progress_by = NULL, updated_at = now()
		WHERE id = $4 AND status = $5`,
		ApplicationStatusRejected, superadminTgID, reason, applicationID, ApplicationStatusPending,
	)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("application not found or not pending")
	}
	return nil
}

// SetRejectInProgress sets reject_in_progress_by for the application so the next superadmin message is treated as reject reason (restart-safe). Only updates if status is pending. Returns true if a row was updated.
func SetRejectInProgress(ctx context.Context, applicationID string, superadminTgID int64) (bool, error) {
	res, err := db.Pool.Exec(ctx, `
		UPDATE applications SET reject_in_progress_by = $2, updated_at = now()
		WHERE id = $1 AND status = $3`,
		applicationID, superadminTgID, ApplicationStatusPending,
	)
	if err != nil {
		return false, err
	}
	return res.RowsAffected() == 1, nil
}

// GetApplicationIDByRejectInProgressBy returns the application ID for which this superadmin is in reject-reason flow, or "" if none.
func GetApplicationIDByRejectInProgressBy(ctx context.Context, superadminTgID int64) (string, error) {
	var id string
	err := db.Pool.QueryRow(ctx, `SELECT id::text FROM applications WHERE reject_in_progress_by = $1`, superadminTgID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

// ClearRejectInProgress clears reject_in_progress_by for the given application (e.g. after reject flow is abandoned or completed elsewhere).
func ClearRejectInProgress(ctx context.Context, applicationID string) error {
	_, err := db.Pool.Exec(ctx, `UPDATE applications SET reject_in_progress_by = NULL, updated_at = now() WHERE id = $1`, applicationID)
	return err
}

// MarkApprovedRestaurantAdminRejectedIfNoCredential sets the user's latest approved restaurant_admin application to rejected when they have no credential (e.g. branch was deleted). Allows them to re-apply via Zayavka. Returns true if an application was updated.
func MarkApprovedRestaurantAdminRejectedIfNoCredential(ctx context.Context, tgUserID int64) (bool, error) {
	res, err := db.Pool.Exec(ctx, `
		UPDATE applications SET status = $1, reject_reason = $2, updated_at = now()
		WHERE id = (
			SELECT id FROM applications
			WHERE tg_user_id = $3 AND type = $4 AND status = $5
			ORDER BY created_at DESC LIMIT 1
		)`,
		ApplicationStatusRejected, "Filial o'chirilgan yoki parol olib tashlangan", tgUserID, ApplicationTypeRestaurantAdmin, ApplicationStatusApproved,
	)
	if err != nil {
		return false, err
	}
	return res.RowsAffected() == 1, nil
}

// UpdateDriverCar updates driver car_plate (drivers table has car_plate only).
func UpdateDriverCar(ctx context.Context, driverID string, carPlate string) error {
	_, err := db.Pool.Exec(ctx, `UPDATE drivers SET car_plate = $1, updated_at = now() WHERE id = $2`, carPlate, driverID)
	return err
}
