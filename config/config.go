package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	DB       DBConfig
	Telegram TelegramConfig
	Delivery DeliveryConfig
}

type DBConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
}

type TelegramConfig struct {
	Token         string
	AdderToken    string
	ZayafkaToken  string // token for application-form-only bot (ariza)
	MessageToken  string // token for sending order notifications to admin
	DriverToken   string // token for driver bot
	Login         string // admin password for adder bot
	SuperadminID  int64  // Telegram ID for superadmin (/applications); 0 = use ADMIN_ID from env at runtime
}

type DeliveryConfig struct {
	BaseFee             int64   // start price (e.g. 5000 sum)
	RatePerKm           int64   // per km (e.g. 4000 sum)
	DriverJobsRadius    float64 // radius in km for driver jobs search
	DriverPushRadiusKm  float64 // radius in km for pushing READY orders to nearby drivers (default 5)
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	port, _ := strconv.Atoi(getEnv("DB_PORT", "5432"))

	return &Config{
		DB: DBConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     port,
			User:     getEnv("DB_USER", "postgres"),
			Password: getEnv("DB_PASSWORD", ""),
			Database: getEnv("DB_NAME", "delivery"),
		},
		Telegram: TelegramConfig{
			Token:        getEnv("TOKEN", ""),
			AdderToken:   getEnv("ADDER_TOKEN", ""),
			ZayafkaToken: getEnv("ZAYAFKA", ""),
			MessageToken: getEnv("MESSAGE_TOKEN", ""),
			DriverToken:  getEnv("DRIVER_BOT_TOKEN", ""),
			Login:        getEnv("LOGIN", ""),
			SuperadminID: getSuperadminID(),
		},
		Delivery: DeliveryConfig{
			BaseFee:            getBaseFee(),   // 5000 sum start
			RatePerKm:          getRatePerKm(), // 4000 sum per km
			DriverJobsRadius:   getDriverJobsRadius(),
			DriverPushRadiusKm: getDriverPushRadiusKm(),
		},
	}, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getBaseFee() int64 {
	if v := os.Getenv("DELIVERY_BASE_FEE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return 5000 // 5000 sum start price
}

func getRatePerKm() int64 {
	if v := os.Getenv("RATE_PER_KM"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 4000 // 4000 sum per km
}

func getDriverJobsRadius() float64 {
	if v := os.Getenv("DRIVER_JOBS_RADIUS_KM"); v != "" {
		if radius, err := strconv.ParseFloat(v, 64); err == nil && radius > 0 {
			return radius
		}
	}
	// Default 50km for debugging
	return 50.0
}

func getDriverPushRadiusKm() float64 {
	if v := os.Getenv("DRIVER_PUSH_RADIUS_KM"); v != "" {
		if radius, err := strconv.ParseFloat(v, 64); err == nil && radius > 0 {
			return radius
		}
	}
	return 5.0
}

func getSuperadminID() int64 {
	if v := os.Getenv("SUPERADMIN_TG_ID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			return id
		}
	}
	return 0
}
