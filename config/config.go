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
	Login       string // admin password for adder bot
}

type DeliveryConfig struct {
	RatePerKm int64
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
			Login:        getEnv("LOGIN", ""),
		},
		Delivery: DeliveryConfig{
			RatePerKm: 2000,
		},
	}, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
