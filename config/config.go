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
	Token       string
	AdderToken  string
	MessageToken string // token for sending order notifications to admin
	DriverToken string // token for driver bot
	Login       string // admin password for adder bot
}

type DeliveryConfig struct {
	RatePerKm        int64
	DriverJobsRadius float64 // radius in km for driver jobs search
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
			MessageToken: getEnv("MESSAGE_TOKEN", ""),
			DriverToken:  getEnv("DRIVER_BOT_TOKEN", ""),
			Login:        getEnv("LOGIN", ""),
		},
		Delivery: DeliveryConfig{
			RatePerKm:        2000,
			DriverJobsRadius: getDriverJobsRadius(),
		},
	}, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
