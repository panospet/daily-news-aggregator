package cache

import (
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/panospet/htmx-daily-news/internal/database"
	"github.com/panospet/htmx-daily-news/internal/grouping"
	"github.com/panospet/htmx-daily-news/internal/models"
)

// ---------- Article Cache (flat, non-AI) ----------

// ArticleCache caches flat dashboard data with a configurable TTL.
type ArticleCache struct {
	mu       sync.RWMutex
	data     *models.DashboardData
	ts       time.Time
	interval time.Duration
}

// NewArticleCache creates a new article cache with the given TTL.
func NewArticleCache(interval time.Duration) *ArticleCache {
	return &ArticleCache{interval: interval}
}

// Get returns cached data or fetches fresh data using the repository and tunnel retry logic.
func (c *ArticleCache) Get(repo *database.Repository, retryFn func(hours int) (*models.DashboardData, error)) (*models.DashboardData, error) {
	c.mu.RLock()
	if c.data != nil && time.Since(c.ts) < c.interval {
		d := c.data
		c.mu.RUnlock()
		return d, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.data != nil && time.Since(c.ts) < c.interval {
		return c.data, nil
	}

	data, err := retryFn(6)
	if err != nil {
		if c.data != nil {
			log.Printf("DB query failed, serving stale data: %v", err)
			return c.data, nil
		}
		return nil, err
	}
	c.data = data
	c.ts = time.Now()
	return data, nil
}

// Invalidate marks the cache as expired.
func (c *ArticleCache) Invalidate() {
	c.mu.Lock()
	c.ts = time.Time{}
	c.mu.Unlock()
}

// ---------- AI Cache ----------

// FetchFunc is the function type for fetching fresh AI dashboard data.
type FetchFunc func() (*models.AIDashboardData, error)

// AICache caches AI-grouped dashboard data with a configurable TTL.
type AICache struct {
	Mu       sync.RWMutex
	data     *models.AIDashboardData
	ts       time.Time
	interval time.Duration
}

// NewAICache creates a new AI cache with the given TTL.
func NewAICache(interval time.Duration) *AICache {
	return &AICache{interval: interval}
}

// Get returns cached AI data, triggering background refresh if stale.
func (c *AICache) Get(db *sql.DB, fetchFresh func(db *sql.DB) (*models.AIDashboardData, error)) (*models.AIDashboardData, error) {
	c.Mu.RLock()
	if c.data != nil && time.Since(c.ts) < c.interval {
		d := c.data
		c.Mu.RUnlock()
		return d, nil
	}
	// Cache expired or empty. If we have stale data, return it immediately
	// and start a background refresh.
	if c.data != nil {
		stale := c.data
		c.Mu.RUnlock()
		// Background refresh (non-blocking)
		go c.refresh(db, fetchFresh)
		return stale, nil
	}
	c.Mu.RUnlock()

	// No stale data available — do a blocking fetch (cold start)
	c.Mu.Lock()
	// Double-check
	if c.data != nil {
		c.Mu.Unlock()
		return c.data, nil
	}
	c.Mu.Unlock()

	return c.refreshBlocking(db, fetchFresh)
}

// refresh fetches fresh AI data in the background and updates the cache.
func (c *AICache) refresh(db *sql.DB, fetchFresh func(db *sql.DB) (*models.AIDashboardData, error)) {
	c.Mu.Lock()
	// Another goroutine already refreshed it
	if c.data != nil && time.Since(c.ts) < c.interval {
		c.Mu.Unlock()
		return
	}
	c.Mu.Unlock()

	log.Println("AI cache stale — refreshing in background")
	newData, err := fetchFresh(db)
	if err != nil {
		log.Printf("AI background refresh failed, keeping stale: %v", err)
		return
	}

	c.Mu.Lock()
	c.data = newData
	c.ts = time.Now()
	c.Mu.Unlock()
	log.Printf("AI cache refreshed — %d articles in %d categories", newData.Total, len(newData.Categories))
}

// refreshBlocking does a cold-start blocking fetch (no stale data to serve).
func (c *AICache) refreshBlocking(db *sql.DB, fetchFresh func(db *sql.DB) (*models.AIDashboardData, error)) (*models.AIDashboardData, error) {
	newData, err := fetchFresh(db)
	if err != nil {
		return nil, err
	}
	c.Mu.Lock()
	c.data = newData
	c.ts = time.Now()
	c.Mu.Unlock()
	return newData, nil
}

// Invalidate marks the AI cache as expired.
func (c *AICache) Invalidate() {
	c.Mu.Lock()
	c.ts = time.Time{}
	c.Mu.Unlock()
}

// FetchFresh queries the DB and runs AI grouping.
func FetchFresh(db *sql.DB, repo *database.Repository, groupSvc *grouping.Service,
	retryFn func(hours int) (*models.DashboardData, error)) func(db *sql.DB) (*models.AIDashboardData, error) {

	return func(db *sql.DB) (*models.AIDashboardData, error) {
		log.Printf("[timing] fetchFresh: starting DB query")
		dd, err := retryFn(12)
		if err != nil {
			return nil, err
		}
		log.Printf("[timing] fetchFresh: DB query done — %d categories", len(dd.Categories))

		var allArticles []models.Article
		for _, cat := range dd.Categories {
			allArticles = append(allArticles, cat.Articles...)
		}
		log.Printf("[timing] fetchFresh: %d total articles", len(allArticles))

		if len(allArticles) == 0 {
			return &models.AIDashboardData{Categories: []models.AICategoryGroup{}, Total: 0, Generated: time.Now().Format("15:04:05")}, nil
		}

		t0 := time.Now()
		groups, err := groupSvc.GroupArticles(allArticles)
		if err != nil {
			return nil, err
		}
		log.Printf("[timing] fetchFresh: AI grouping done in %v — %d categories with %d groups total",
			time.Since(t0).Round(time.Second), len(groups), grouping.CountCatGroups(groups))

		// Assign stable group IDs
		grouping.AssignGroupIDs(groups)

		total := 0
		for _, g := range groups {
			total += g.Count
		}

		return &models.AIDashboardData{Categories: groups, Total: total, Generated: time.Now().Format("15:04:05")}, nil
	}
}
