package tunnel

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"sync"
	"time"

	"github.com/panospet/htmx-daily-news/internal/config"
)

// Tunnel manages an SSH port-forwarding tunnel to the database.
type Tunnel struct {
	cmd *exec.Cmd
	mu  sync.Mutex
}

// Open starts an SSH tunnel using the given configuration.
// If SSHHost is empty, it logs a message and returns nil (direct connection).
func (t *Tunnel) Open(cfg config.Config) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if cfg.SSHHost == "" {
		log.Println("No SSH host configured — connecting directly to DB")
		return nil
	}

	_ = exec.Command("pkill", "-f", fmt.Sprintf("ssh.*-L %d:", cfg.SSHTunnel)).Run()
	time.Sleep(500 * time.Millisecond)

	cmd := exec.Command("ssh",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "IdentityFile=/root/.ssh/id_rsa",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-F", "/dev/null",
		"-L", fmt.Sprintf("%d:127.0.0.1:%d", cfg.SSHTunnel, cfg.SSHRemote),
		"-N",
		cfg.SSHHost,
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("SSH tunnel start failed: %w", err)
	}
	t.cmd = cmd
	// Wait for tunnel to be ready
	time.Sleep(3 * time.Second)
	// Verify tunnel is actually listening
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.SSHTunnel), 5*time.Second); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("SSH tunnel not listening after start: %w", err)
	}
	log.Printf("SSH tunnel open: localhost:%d → %s:%d", cfg.SSHTunnel, cfg.SSHHost, cfg.SSHRemote)
	return nil
}

// Close kills the SSH tunnel process.
func (t *Tunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = exec.Command("pkill", "-f", fmt.Sprintf("ssh.*-L %d:", 5433)).Run()
}
