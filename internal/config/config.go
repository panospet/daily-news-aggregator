package config

import (
	"os"
	"strconv"
)

// Config holds all application configuration values.
type Config struct {
	DBHost       string
	DBPort       int
	DBName       string
	DBUser       string
	DBPass       string
	HTTPPort     int
	CommandCodeAPIKey string
	FallbackAPIKey string
	AuthUsername string
	AuthPassword string
	AIServiceURL string
	RedisAddr    string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() Config {
	return Config{
		DBHost:       EnvStr("MF_DB_HOST", "127.0.0.1"),
		DBPort:       EnvInt("MF_DB_PORT", 5433),
		DBName:       EnvStr("MF_DB_NAME", "miniflux"),
		DBUser:       EnvStr("MF_DB_USER", "external_reader"),
		DBPass:       EnvStr("MF_DB_PASSWORD", ""),
		HTTPPort:     EnvInt("PORT", 8080),
		CommandCodeAPIKey: EnvStr("COMMAND_CODE_API_KEY", ""),
		FallbackAPIKey:      EnvStr("GEMINI_API_KEY", ""),
		AuthUsername: EnvStr("AUTH_USERNAME", ""),
		AuthPassword: EnvStr("AUTH_PASSWORD", ""),
		AIServiceURL: EnvStr("AI_SERVICE_URL", "http://127.0.0.1:8001"),
		RedisAddr:    EnvStr("REDIS_ADDR", "127.0.0.1:6379"),
	}
}

// EnvStr returns the value of an environment variable, or the default if unset.
func EnvStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// EnvInt returns the integer value of an environment variable, or the default if unset or invalid.
func EnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}
