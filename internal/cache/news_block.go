package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewsBlockSource is one source article backing a news item.
type NewsBlockSource struct {
	Newspaper string `json:"newspaper"`
	Title     string `json:"title"`
	URL       string `json:"url"`
}

// NewsBlockItem is one speech-ready news item for the audio briefing.
type NewsBlockItem struct {
	Topic       string            `json:"topic"`
	Category    string            `json:"category"`
	SummaryText string            `json:"summary_text"`
	Sources     []NewsBlockSource `json:"sources,omitempty"`
}

// NewsBlock is a pre-generated, read-only news block for one briefing slot.
type NewsBlock struct {
	Slot      string          `json:"slot"`
	Generated time.Time       `json:"generated"`
	Items     []NewsBlockItem `json:"items"`
}

// NewsBlockStore persists news blocks in Redis under briefing_news:<slot>.
type NewsBlockStore struct {
	client *redis.Client
	ttl    time.Duration
}

func NewNewsBlockStore(client *redis.Client, ttl time.Duration) *NewsBlockStore {
	return &NewsBlockStore{client: client, ttl: ttl}
}

func (s *NewsBlockStore) key(slot string) string {
	return "briefing_news:" + slot
}

func (s *NewsBlockStore) Get(slot string) (NewsBlock, bool, error) {
	if s == nil || s.client == nil || slot == "" {
		return NewsBlock{}, false, nil
	}
	val, err := s.client.Get(context.Background(), s.key(slot)).Result()
	if err == redis.Nil {
		return NewsBlock{}, false, nil
	}
	if err != nil {
		return NewsBlock{}, false, err
	}
	if val == "" {
		return NewsBlock{}, false, nil
	}

	var block NewsBlock
	if err := json.Unmarshal([]byte(val), &block); err != nil {
		// Drop corrupt entries so they do not poison later reads.
		s.client.Del(context.Background(), s.key(slot))
		return NewsBlock{}, false, nil
	}
	return block, true, nil
}

func (s *NewsBlockStore) Set(slot string, block NewsBlock) error {
	if s == nil || s.client == nil || slot == "" {
		return nil
	}
	b, err := json.Marshal(block)
	if err != nil {
		return err
	}
	ttl := s.ttl
	if ttl <= 0 {
		ttl = 14 * time.Hour
	}
	return s.client.Set(context.Background(), s.key(slot), b, ttl).Err()
}
