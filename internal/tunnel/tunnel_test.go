package tunnel

import (
	"testing"

	"github.com/panospet/htmx-daily-news/internal/config"
)

func TestOpen_NoSSHHost_ReturnsNil(t *testing.T) {
	tun := &Tunnel{}
	cfg := config.Config{SSHHost: ""}

	err := tun.Open(cfg)
	if err != nil {
		t.Errorf("expected nil error for empty SSHHost, got %v", err)
	}
}

func TestClose_NilCmd_NoPanic(t *testing.T) {
	tun := &Tunnel{}
	// Should not panic
	tun.Close()
}
