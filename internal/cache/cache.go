package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/database"
	"github.com/panospet/daily-news-aggregator/internal/grouping"
	"github.com/panospet/daily-news-aggregator/internal/models"
	"github.com/redis/go-redis/v9"
)

// ---------- Article-to-Group Mapping (Redis DB 1) ----------

type GroupMapping struct {
	client *redis.Client
}

func NewGroupMapping(addr string) *GroupMapping {
	client := redis.NewClient(&redis.Options{
		Addr: addr,
		DB:   1, // Use Redis DB 1 for group mappings
	})
	return &GroupMapping{client: client}
}

// Save stores article_id -> group_id mappings. Replaces all existing mappings.
func (m *GroupMapping) Save(mapping map[int64]string) error {
	if len(mapping) == 0 {
		return nil
	}
	ctx := context.Background()
	pipe := m.client.Pipeline()

	// Clear old mappings
	pipe.Del(ctx, "article_groups")

	// Store new mappings
	fields := make(map[string]interface{}, len(mapping))
	for articleID, groupID := range mapping {
		fields[strconv.FormatInt(articleID, 10)] = groupID
	}
	pipe.HSet(ctx, "article_groups", fields)
	pipe.Expire(ctx, "article_groups", 2*time.Hour) // slightly longer than ai_cache TTL

	_, err := pipe.Exec(ctx)
	return err
}

// GetGroupIDs returns group_id for each given article_id.
func (m *GroupMapping) GetGroupIDs(articleIDs []int64) (map[int64]string, error) {
	if len(articleIDs) == 0 {
		return nil, nil
	}
	ctx := context.Background()
	fields := make([]string, len(articleIDs))
	for i, id := range articleIDs {
		fields[i] = strconv.FormatInt(id, 10)
	}

	vals, err := m.client.HMGet(ctx, "article_groups", fields...).Result()
	if err != nil {
		return nil, err
	}

	result := make(map[int64]string)
	for i, v := range vals {
		if v != nil {
			result[articleIDs[i]] = v.(string)
		}
	}
	return result, nil
}

type RedisWrapper[T any] struct {
	Data T         `json:"data"`
	Ts   time.Time `json:"ts"`
}

// ---------- Article Cache (flat, non-AI) ----------

type ArticleCache struct {
	client   *redis.Client
	interval time.Duration
}

func NewArticleCache(client *redis.Client, interval time.Duration) *ArticleCache {
	return &ArticleCache{client: client, interval: interval}
}

func (c *ArticleCache) Get(repo *database.Repository, retryFn func(hours int) (*models.DashboardData, error)) (*models.DashboardData, error) {
	ctx := context.Background()
	val, err := c.client.Get(ctx, "article_cache").Result()
	if err == nil {
		var wrapper RedisWrapper[*models.DashboardData]
		if jsonErr := json.Unmarshal([]byte(val), &wrapper); jsonErr == nil {
			if time.Since(wrapper.Ts) < c.interval {
				return wrapper.Data, nil
			}
		}
	}

	data, err := retryFn(6)
	if err != nil {
		if val != "" {
			var wrapper RedisWrapper[*models.DashboardData]
			if jsonErr := json.Unmarshal([]byte(val), &wrapper); jsonErr == nil {
				log.Printf("DB query failed, serving stale data: %v", err)
				return wrapper.Data, nil
			}
		}
		return nil, err
	}

	wrapper := RedisWrapper[*models.DashboardData]{Data: data, Ts: time.Now()}
	bytes, _ := json.Marshal(wrapper)
	c.client.Set(ctx, "article_cache", bytes, 0)
	return data, nil
}

func (c *ArticleCache) Invalidate() {
	c.client.Del(context.Background(), "article_cache")
}

// ---------- AI Cache ----------

type FetchFunc func() (*models.AIDashboardData, error)

type AICache struct {
	client       *redis.Client
	interval     time.Duration
	refreshMutex sync.Mutex
	refreshing   atomic.Bool
}

func NewAICache(client *redis.Client, interval time.Duration) *AICache {
	return &AICache{client: client, interval: interval}
}

func (c *AICache) Get(db *sql.DB, fetchFresh func(db *sql.DB) (*models.AIDashboardData, error)) (*models.AIDashboardData, error) {
	ctx := context.Background()
	val, err := c.client.Get(ctx, "ai_cache").Result()
	if err == nil {
		var wrapper RedisWrapper[*models.AIDashboardData]
		if jsonErr := json.Unmarshal([]byte(val), &wrapper); jsonErr == nil {
			if time.Since(wrapper.Ts) < c.interval {
				return wrapper.Data, nil
			}
			go c.refresh(db, fetchFresh)
			return wrapper.Data, nil
		}
	}

	c.refreshMutex.Lock()
	defer c.refreshMutex.Unlock()

	val, err = c.client.Get(ctx, "ai_cache").Result()
	if err == nil {
		var wrapper RedisWrapper[*models.AIDashboardData]
		if jsonErr := json.Unmarshal([]byte(val), &wrapper); jsonErr == nil {
			if time.Since(wrapper.Ts) < c.interval {
				return wrapper.Data, nil
			}
		}
	}

	// If we reach here, the cache is completely empty.
	// Trigger background refresh and return an error so the caller can serve fallback data.
	go c.refresh(db, fetchFresh)
	return nil, fmt.Errorf("ai cache empty, background refresh triggered")
}

func (c *AICache) refresh(db *sql.DB, fetchFresh func(db *sql.DB) (*models.AIDashboardData, error)) {
	if !c.refreshing.CompareAndSwap(false, true) {
		return
	}
	defer c.refreshing.Store(false)

	ctx := context.Background()
	val, err := c.client.Get(ctx, "ai_cache").Result()
	if err == nil {
		var wrapper RedisWrapper[*models.AIDashboardData]
		if jsonErr := json.Unmarshal([]byte(val), &wrapper); jsonErr == nil {
			if time.Since(wrapper.Ts) < c.interval {
				return
			}
		}
	}

	log.Println("AI cache stale — refreshing in background")
	newData, err := fetchFresh(db)
	if err != nil {
		log.Printf("AI background refresh failed, keeping stale: %v", err)
		return
	}

	wrapper := RedisWrapper[*models.AIDashboardData]{Data: newData, Ts: time.Now()}
	bytes, _ := json.Marshal(wrapper)

	c.refreshMutex.Lock()
	c.client.Set(ctx, "ai_cache", bytes, 0)
	c.refreshMutex.Unlock()

	log.Printf("AI cache refreshed — %d articles in %d categories", newData.Total, len(newData.Categories))
}

func (c *AICache) refreshBlocking(db *sql.DB, fetchFresh func(db *sql.DB) (*models.AIDashboardData, error)) (*models.AIDashboardData, error) {
	newData, err := fetchFresh(db)
	if err != nil {
		return nil, err
	}
	wrapper := RedisWrapper[*models.AIDashboardData]{Data: newData, Ts: time.Now()}
	bytes, _ := json.Marshal(wrapper)
	c.client.Set(context.Background(), "ai_cache", bytes, 0)
	return newData, nil
}

func (c *AICache) Invalidate() {
	c.client.Del(context.Background(), "ai_cache")
}

func (c *AICache) UpdateTopic(category string, groupID string, newTopic string) {
	ctx := context.Background()
	c.refreshMutex.Lock()
	defer c.refreshMutex.Unlock()

	val, err := c.client.Get(ctx, "ai_cache").Result()
	if err != nil {
		return
	}

	var wrapper RedisWrapper[*models.AIDashboardData]
	if err := json.Unmarshal([]byte(val), &wrapper); err != nil {
		return
	}

	for catIdx, cg := range wrapper.Data.Categories {
		if strings.EqualFold(cg.Category, category) {
			for gi, g := range cg.Groups {
				if g.GroupID == groupID {
					wrapper.Data.Categories[catIdx].Groups[gi].Topic = newTopic
					break
				}
			}
			break
		}
	}

	bytes, _ := json.Marshal(wrapper)
	c.client.Set(ctx, "ai_cache", bytes, 0)
}

func FetchFresh(db *sql.DB, repo *database.Repository, groupSvc *grouping.Service,
	retryFn func(hours int) (*models.DashboardData, error), groupMap *GroupMapping) func(db *sql.DB) (*models.AIDashboardData, error) {

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
			loc, _ := time.LoadLocation("Europe/Athens")
			return &models.AIDashboardData{Categories: []models.AICategoryGroup{}, Total: 0, Generated: time.Now().In(loc).Format("15:04:05")}, nil
		}

		t0 := time.Now()
		groups, err := groupSvc.GroupArticles(allArticles)
		if err != nil {
			return nil, err
		}
		log.Printf("[timing] fetchFresh: AI grouping done in %v — %d categories with %d groups total",
			time.Since(t0).Round(time.Second), len(groups), grouping.CountCatGroups(groups))

		grouping.AssignGroupIDs(groups)

		// Build article -> group mapping for Redis DB 1
		if groupMap != nil {
			mapping := make(map[int64]string)
			for _, catGrp := range groups {
				for _, grp := range catGrp.Groups {
					for _, art := range grp.Articles {
						mapping[art.ID] = grp.GroupID
					}
				}
			}
			if err := groupMap.Save(mapping); err != nil {
				log.Printf("Warning: failed to save article-group mapping: %v", err)
			} else {
				log.Printf("[groupMap] Saved %d article-to-group mappings in Redis DB 1", len(mapping))
			}
		}

		total := 0
		tagFreq := make(map[string]int)
		for _, catGrp := range groups {
			total += catGrp.Count
			for _, grp := range catGrp.Groups {
				for _, tag := range grp.Tags {
					tagFreq[tag]++
				}
			}
		}

		var topTags []models.TagCount
		for tag, count := range tagFreq {
			topTags = append(topTags, models.TagCount{Tag: tag, Count: count})
		}
		sort.Slice(topTags, func(i, j int) bool {
			if topTags[i].Count == topTags[j].Count {
				return topTags[i].Tag < topTags[j].Tag
			}
			return topTags[i].Count > topTags[j].Count
		})

		loc, _ := time.LoadLocation("Europe/Athens")
		return &models.AIDashboardData{
			Categories: groups,
			Total:      total,
			Generated:  time.Now().In(loc).Format("15:04:05"),
			TopTags:    topTags,
		}, nil
	}
}
