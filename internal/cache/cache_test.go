package cache

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/models"
	"github.com/redis/go-redis/v9"
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

// deadClient points at a closed port so refresh() cannot read a warm entry and
// falls through to the rebuild path. Keeps these tests free of a Redis server.
func deadClient() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  50 * time.Millisecond,
		ReadTimeout:  50 * time.Millisecond,
		WriteTimeout: 50 * time.Millisecond,
		MaxRetries:   -1,
	})
}

// The refresher must always poll at least as often as the threshold it rebuilds
// against, or a stale entry can sit for a whole extra interval.
func TestRefreshCheckInterval(t *testing.T) {
	cases := []struct{ interval, want time.Duration }{
		{60 * time.Minute, 10 * time.Minute}, // interval / 6
		{6 * time.Hour, time.Hour},           // interval / 6
		{12 * time.Minute, 5 * time.Minute},  // floor applies
		{time.Minute, time.Minute},           // floor clamped to the interval
	}
	for _, tc := range cases {
		got := refreshCheckInterval(tc.interval)
		if got != tc.want {
			t.Errorf("refreshCheckInterval(%v) = %v, want %v", tc.interval, got, tc.want)
		}
		if got <= 0 || got > tc.interval {
			t.Errorf("refreshCheckInterval(%v) = %v: must be > 0 and <= the interval", tc.interval, got)
		}
	}
}

// A background refresher must rebuild without any page view, and must stop when
// its context is cancelled — otherwise every restart leaks a goroutine that
// keeps clustering forever.
func TestStartBackgroundRefresh_RebuildsWithoutPageViewAndStopsOnCancel(t *testing.T) {
	c := NewAICache(deadClient(), 60*time.Minute)
	c.checkEvery = 25 * time.Millisecond // test cadence; would be 10m in production

	var calls atomic.Int32
	fetch := func(*sql.DB) (*models.AIDashboardData, error) {
		calls.Add(1)
		return &models.AIDashboardData{Total: 1}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.StartBackgroundRefresh(ctx, nil, fetch, 0)

	// Repeated rebuilds, with nobody calling Get().
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := calls.Load(); n < 2 {
		t.Fatalf("expected repeated background rebuilds, got %d", n)
	}

	cancel()
	time.Sleep(60 * time.Millisecond) // let an in-flight tick settle
	settled := calls.Load()
	time.Sleep(200 * time.Millisecond)
	if got := calls.Load(); got != settled {
		t.Errorf("refresher kept running after cancel: %d -> %d", settled, got)
	}
}
