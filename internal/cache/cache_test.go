package cache

import (
	"testing"
	"time"
)

func TestNewArticleCache(t *testing.T) {
	c := NewArticleCache(nil, 2*time.Minute)
	if c.interval != 2*time.Minute {
		t.Errorf("expected 2m interval, got %v", c.interval)
	}
}

func TestNewAICache(t *testing.T) {
	c := NewAICache(nil, 60*time.Minute)
	if c.interval != 60*time.Minute {
		t.Errorf("expected 60m interval, got %v", c.interval)
	}
	if c.refreshing.Load() {
		t.Error("expected refreshing flag to start false")
	}
}

// GetGroupIDs short-circuits on empty input without touching Redis.
func TestGroupMapping_GetGroupIDs_EmptyInput(t *testing.T) {
	m := &GroupMapping{}
	res, err := m.GetGroupIDs(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != nil {
		t.Errorf("expected nil result, got %v", res)
	}
}
