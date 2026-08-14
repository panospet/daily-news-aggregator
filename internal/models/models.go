package models

import "time"

// Article represents a single news entry from the Miniflux database.
type Article struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Published time.Time `json:"published_at"`
	Feed      string    `json:"feed"`
	Category  string    `json:"category"`
	Content   string    `json:"content"`
	TimeAgo   string    `json:"time_ago"`
}

// CategoryGroup holds articles in a single Miniflux category.
type CategoryGroup struct {
	Category string
	Articles []Article
	Count    int
}

// DashboardData is the flat (non-AI) view of all articles.
type DashboardData struct {
	Categories []CategoryGroup
	Total      int
	Generated  string
}

// AIGroup is a topic cluster within a category.
type AIGroup struct {
	Topic    string    `json:"topic"`
	Articles []Article `json:"articles"`
	Count    int       `json:"count"`
	GroupID  string    `json:"group_id,omitempty"`
	Category string    `json:"category,omitempty"`
	Tags     []string  `json:"tags,omitempty"`
}

// AICategoryGroup collects topic groups under a display category.
type AICategoryGroup struct {
	Category string    `json:"category"`
	Groups   []AIGroup `json:"groups"`
	Count    int       `json:"count"`
}

// TopGroup pairs a topic group with its display category for the
// flat "Top Stories" homepage grid.
type TopGroup struct {
	Category string
	Group    AIGroup
}

// TagCount represents a tag and its frequency.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// AIDashboardData is the AI-grouped view of all articles.
type AIDashboardData struct {
	Categories []AICategoryGroup `json:"categories"`
	Total      int               `json:"total"`
	Generated  string            `json:"generated"`
	IsFallback bool              `json:"is_fallback,omitempty"`
	TopTags    []TagCount        `json:"top_tags,omitempty"`
	TopGroups  []TopGroup        `json:"-"`
	View       string            `json:"-"`
	Query      string            `json:"-"`
}

// AISummaryResult contains the AI-generated summary for a topic group.
type AISummaryResult struct {
	Summary string   `json:"summary"`
	Sources []string `json:"sources"`
}
