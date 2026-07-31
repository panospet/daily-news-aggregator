package config

import (
	"os"
	"strconv"
)

// CatDisplayName maps internal Miniflux category names to Greek display names.
var CatDisplayName = map[string]string{
	"international": "ειδήσεις Κόσμος",
	"news":          "ειδήσεις Ελλάδα",
	"sports":        "αθλητικές ειδήσεις",
	"music":         "μουσικά νέα",
	"politics":      "ειδήσεις Ελλάδα",
	"random":        "διάφορες ειδήσεις",
	"panionara":     "αθλητικές ειδήσεις",
}

// Config holds all application configuration values.
type Config struct {
	DBHost       string
	DBPort       int
	DBName       string
	DBUser       string
	DBPass       string
	SSHHost      string
	SSHRemote    int
	SSHTunnel    int
	HTTPPort     int
	GeminiAPIKey string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() Config {
	return Config{
		DBHost:       EnvStr("MF_DB_HOST", "127.0.0.1"),
		DBPort:       EnvInt("MF_DB_PORT", 5433),
		DBName:       EnvStr("MF_DB_NAME", "miniflux"),
		DBUser:       EnvStr("MF_DB_USER", "external_reader"),
		DBPass:       EnvStr("MF_DB_PASSWORD", ""),
		SSHHost:      EnvStr("MF_SSH_HOST", ""),
		SSHRemote:    EnvInt("MF_SSH_REMOTE_PORT", 5435),
		SSHTunnel:    EnvInt("MF_SSH_TUNNEL_PORT", 5433),
		HTTPPort:     EnvInt("PORT", 8080),
		GeminiAPIKey: EnvStr("GEMINI_API_KEY", ""),
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
