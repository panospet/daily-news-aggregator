package cache

import (
	"testing"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/models"
)

func TestArticleCache_Invalidate(t *testing.T) {
	c := NewArticleCache(5 * time.Minute)
	c.ts = time.Now()
	c.data = &models.DashboardData{Total: 10}

	c.Invalidate()

	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ts.IsZero() {
		t.Error("expected ts to be zero after invalidation")
	}
}

func TestAICache_Invalidate(t *testing.T) {
	c := NewAICache(30 * time.Minute)
	c.ts = time.Now()
	c.data = &models.AIDashboardData{Total: 5}

	c.Invalidate()

	c.Mu.RLock()
	defer c.Mu.RUnlock()
	if !c.ts.IsZero() {
		t.Error("expected ts to be zero after invalidation")
	}
}

func TestNewArticleCache(t *testing.T) {
	c := NewArticleCache(10 * time.Minute)
	if c.interval != 10*time.Minute {
		t.Errorf("expected 10m interval, got %v", c.interval)
	}
	if c.data != nil {
		t.Error("expected nil initial data")
	}
}

func TestNewAICache(t *testing.T) {
	c := NewAICache(60 * time.Minute)
	if c.interval != 60*time.Minute {
		t.Errorf("expected 60m interval, got %v", c.interval)
	}
	if c.data != nil {
		t.Error("expected nil initial data")
	}
}
