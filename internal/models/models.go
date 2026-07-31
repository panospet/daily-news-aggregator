package models

import "time"

// Article represents a single news entry from the Miniflux database.
type Article struct {
	ID        int64
	Title     string
	URL       string
	Published time.Time
	Feed      string
	Category  string
	TimeAgo   string
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
}

// AICategoryGroup collects topic groups under a display category.
type AICategoryGroup struct {
	Category string    `json:"category"`
	Groups   []AIGroup `json:"groups"`
	Count    int       `json:"count"`
}

// AIDashboardData is the AI-grouped view of all articles.
type AIDashboardData struct {
	Categories []AICategoryGroup `json:"categories"`
	Total      int               `json:"total"`
	Generated  string            `json:"generated"`
}

// AISummaryResult contains the AI-generated summary for a topic group.
type AISummaryResult struct {
	Summary string   `json:"summary"`
	Sources []string `json:"sources"`
}
