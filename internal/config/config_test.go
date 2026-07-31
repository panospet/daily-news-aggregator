package config

import (
	"testing"
)

func TestEnvStr_Default(t *testing.T) {
	val := EnvStr("NONEXISTENT_TEST_KEY_12345", "fallback")
	if val != "fallback" {
		t.Errorf("expected fallback, got %q", val)
	}
}

func TestEnvStr_Override(t *testing.T) {
	t.Setenv("TEST_ENV_STR_KEY", "override_value")
	val := EnvStr("TEST_ENV_STR_KEY", "default")
	if val != "override_value" {
		t.Errorf("expected override_value, got %q", val)
	}
}

func TestEnvInt_Default(t *testing.T) {
	val := EnvInt("NONEXISTENT_TEST_KEY_12345", 42)
	if val != 42 {
		t.Errorf("expected 42, got %d", val)
	}
}

func TestEnvInt_Override(t *testing.T) {
	t.Setenv("TEST_ENV_INT_KEY", "9090")
	val := EnvInt("TEST_ENV_INT_KEY", 8080)
	if val != 9090 {
		t.Errorf("expected 9090, got %d", val)
	}
}

func TestEnvInt_InvalidFallsBack(t *testing.T) {
	t.Setenv("TEST_ENV_INT_KEY_BAD", "not_a_number")
	val := EnvInt("TEST_ENV_INT_KEY_BAD", 7777)
	if val != 7777 {
		t.Errorf("expected 7777 for invalid int, got %d", val)
	}
}

func TestLoad_Defaults(t *testing.T) {
	// Clear any env vars that might interfere
	t.Setenv("MF_DB_HOST", "")
	t.Setenv("PORT", "")
	t.Setenv("GEMINI_API_KEY", "")

	cfg := Load()
	if cfg.DBHost != "127.0.0.1" {
		t.Errorf("expected default DBHost 127.0.0.1, got %q", cfg.DBHost)
	}
	if cfg.HTTPPort != 8080 {
		t.Errorf("expected default HTTPPort 8080, got %d", cfg.HTTPPort)
	}
}

func TestLoad_WithEnvOverrides(t *testing.T) {
	t.Setenv("MF_DB_HOST", "db.example.com")
	t.Setenv("PORT", "3000")
	t.Setenv("GEMINI_API_KEY", "test-key-123")

	cfg := Load()
	if cfg.DBHost != "db.example.com" {
		t.Errorf("expected db.example.com, got %q", cfg.DBHost)
	}
	if cfg.HTTPPort != 3000 {
		t.Errorf("expected 3000, got %d", cfg.HTTPPort)
	}
	if cfg.GeminiAPIKey != "test-key-123" {
		t.Errorf("expected test-key-123, got %q", cfg.GeminiAPIKey)
	}
}
