package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/models"
	"github.com/redis/go-redis/v9"
)

// ---------- Summary cache ----------
//
// Summaries are expensive LLM calls. This cache stores a generated summary
// keyed by the stable identity of the source material so repeated requests
// for an unchanged group/article do not hit the LLM again.
//
// Keys deliberately mirror the identity used elsewhere in the stack:
//   - groups: category + group_id (group_id is derived from sorted article IDs)
//   - articles: article_id
// Any change to group membership changes the key, so stale summaries are
// not served when the underlying article set changes.

type SummaryCache struct {
	client *redis.Client
	ttl    time.Duration
}

func NewSummaryCache(client *redis.Client, ttl time.Duration) *SummaryCache {
	return &SummaryCache{client: client, ttl: ttl}
}

func (c *SummaryCache) groupKey(category, groupID string) string {
	return fmt.Sprintf("summary_cache:group:%s:%s", category, groupID)
}

func (c *SummaryCache) articleKey(articleID int64) string {
	return fmt.Sprintf("summary_cache:article:%d", articleID)
}

func (c *SummaryCache) GetGroup(category, groupID string) (models.AISummaryResult, bool, error) {
	if c == nil || c.client == nil || groupID == "" {
		return models.AISummaryResult{}, false, nil
	}
	val, err := c.client.Get(context.Background(), c.groupKey(category, groupID)).Result()
	if err == redis.Nil {
		return models.AISummaryResult{}, false, nil
	}
	if err != nil {
		return models.AISummaryResult{}, false, err
	}
	if val == "" {
		return models.AISummaryResult{}, false, nil
	}

	var result models.AISummaryResult
	if err := json.Unmarshal([]byte(val), &result); err != nil {
		// Drop corrupt entries so they do not poison every later request.
		c.client.Del(context.Background(), c.groupKey(category, groupID))
		return models.AISummaryResult{}, false, nil
	}
	return result, true, nil
}

func (c *SummaryCache) SetGroup(category, groupID string, result models.AISummaryResult) error {
	if c == nil || c.client == nil || groupID == "" {
		return nil
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.client.Set(context.Background(), c.groupKey(category, groupID), b, c.ttl).Err()
}

func (c *SummaryCache) GetArticle(articleID int64) (models.AISummaryResult, bool, error) {
	if c == nil || c.client == nil || articleID == 0 {
		return models.AISummaryResult{}, false, nil
	}
	val, err := c.client.Get(context.Background(), c.articleKey(articleID)).Result()
	if err == redis.Nil {
		return models.AISummaryResult{}, false, nil
	}
	if err != nil {
		return models.AISummaryResult{}, false, err
	}
	if val == "" {
		return models.AISummaryResult{}, false, nil
	}

	var result models.AISummaryResult
	if err := json.Unmarshal([]byte(val), &result); err != nil {
		c.client.Del(context.Background(), c.articleKey(articleID))
		return models.AISummaryResult{}, false, nil
	}
	return result, true, nil
}

func (c *SummaryCache) SetArticle(articleID int64, result models.AISummaryResult) error {
	if c == nil || c.client == nil || articleID == 0 {
		return nil
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.client.Set(context.Background(), c.articleKey(articleID), b, c.ttl).Err()
}
