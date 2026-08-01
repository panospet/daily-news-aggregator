package database

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/panospet/htmx-daily-news/internal/config"
	"github.com/panospet/htmx-daily-news/internal/models"
)

// OpenDB creates a database connection pool using the given configuration.
func OpenDB(cfg config.Config) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable connect_timeout=5",
		cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPass, cfg.DBName,
	)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Create negative feedback table if it doesn't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS negative_feedback (
			id SERIAL PRIMARY KEY,
			category TEXT NOT NULL,
			topic TEXT NOT NULL,
			group_json JSONB NOT NULL,
			irrelevant_url TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create negative_feedback table: %w", err)
	}

	return db, nil
}

// Repository wraps a sql.DB to provide article queries.
type Repository struct {
	DB *sql.DB
}

// QueryArticles retrieves articles from the last `hours` hours,
// deduplicating by title, and grouping by Miniflux category.
func (r *Repository) QueryArticles(hours int) (*models.DashboardData, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour)

	rows, err := r.DB.Query(`
		SELECT e.id, e.title, e.url, e.published_at, f.title AS feed, c.title AS category, e.content
		FROM entries e
		JOIN feeds f ON f.id = e.feed_id
		JOIN categories c ON c.id = f.category_id
		WHERE e.published_at >= $1
		  AND (f.disabled = false OR f.disabled IS NULL)
		ORDER BY e.published_at DESC
	`, since)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	type entry struct {
		id        int64
		title     string
		url       string
		published time.Time
		feed      string
		category  string
		content   string
	}
	seen := make(map[string]*entry)
	var ordered []string

	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.id, &e.title, &e.url, &e.published, &e.feed, &e.category, &e.content); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		e.title = strings.TrimSpace(e.title)
		if _, exists := seen[e.title]; !exists {
			seen[e.title] = &e
			ordered = append(ordered, e.title)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iter: %w", err)
	}

	catMap := make(map[string][]models.Article)
	for _, title := range ordered {
		e := seen[title]
		art := models.Article{
			ID:        e.id,
			Title:     e.title,
			URL:       e.url,
			Published: e.published,
			Feed:      e.feed,
			Category:  e.category,
			Content:   e.content,
			TimeAgo:   TimeAgo(e.published),
		}
		catMap[e.category] = append(catMap[e.category], art)
	}

	preferredOrder := []string{"international", "news", "sports", "music", "politics", "random", "panionara"}
	var groups []models.CategoryGroup
	for _, cat := range preferredOrder {
		if arts, ok := catMap[cat]; ok {
			groups = append(groups, models.CategoryGroup{Category: cat, Articles: arts, Count: len(arts)})
			delete(catMap, cat)
		}
	}
	for cat, arts := range catMap {
		groups = append(groups, models.CategoryGroup{Category: cat, Articles: arts, Count: len(arts)})
	}

	return &models.DashboardData{Categories: groups, Total: len(ordered), Generated: time.Now().Format("15:04:05")}, nil
}

// TimeAgo returns a human-readable relative time string.
func TimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		return t.Format("Jan 2 15:04")
	}
}

// SaveNegativeFeedback stores feedback about an irrelevant article in a topic group
func (r *Repository) SaveNegativeFeedback(category, topic string, groupJson []byte, irrelevantUrl string) error {
	_, err := r.DB.Exec(`
		INSERT INTO negative_feedback (category, topic, group_json, irrelevant_url)
		VALUES ($1, $2, $3, $4)
	`, category, topic, groupJson, irrelevantUrl)
	return err
}
