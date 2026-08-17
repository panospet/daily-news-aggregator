package database

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/config"
	"github.com/panospet/daily-news-aggregator/internal/models"
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

	// Create positive feedback table if it doesn't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS positive_feedback (
			id SERIAL PRIMARY KEY,
			category TEXT NOT NULL,
			topic TEXT NOT NULL,
			group_json JSONB NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create positive_feedback table: %w", err)
	}

	// Enable pgvector extension and create embeddings table
	_, err = db.Exec(`CREATE EXTENSION IF NOT EXISTS vector`)
	if err != nil {
		log.Printf("Warning: could not create vector extension: %v", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS ai_embeddings (
			article_id BIGINT PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
			embedding vector(768) NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		log.Printf("Warning: could not create ai_embeddings table: %v", err)
	}

	// Create HNSW index for fast cosine similarity search
	_, err = db.Exec(`
		CREATE INDEX IF NOT EXISTS ai_embeddings_cosine_idx 
		ON ai_embeddings USING hnsw (embedding vector_cosine_ops)
	`)
	if err != nil {
		log.Printf("Warning: could not create HNSW index: %v", err)
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

	loc, _ := time.LoadLocation("Europe/Athens")
	return &models.DashboardData{Categories: groups, Total: len(ordered), Generated: time.Now().In(loc).Format("15:04:05")}, nil
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
		loc, _ := time.LoadLocation("Europe/Athens")
		return t.In(loc).Format("Jan 2 15:04")
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

// SavePositiveFeedback stores feedback about a correctly grouped topic
func (r *Repository) SavePositiveFeedback(category, topic string, groupJson []byte) error {
	_, err := r.DB.Exec(`
		INSERT INTO positive_feedback (category, topic, group_json)
		VALUES ($1, $2, $3)
	`, category, topic, groupJson)
	return err
}

// SaveEmbeddings stores pre-computed embeddings for articles.
// Uses ON CONFLICT to skip articles that already have embeddings.
func (r *Repository) SaveEmbeddings(embeddings map[int64][]float32) error {
	if len(embeddings) == 0 {
		return nil
	}
	tx, err := r.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO ai_embeddings (article_id, embedding)
		VALUES ($1, $2::vector)
		ON CONFLICT (article_id) DO NOTHING
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for articleID, emb := range embeddings {
		// Format as pgvector string: "[0.1,0.2,0.3,...]"
		vecStr := formatVector(emb)
		_, err = stmt.Exec(articleID, vecStr)
		if err != nil {
			log.Printf("Warning: failed to save embedding for article %d: %v", articleID, err)
		}
	}
	return tx.Commit()
}

// GetEmbeddings fetches pre-computed embeddings for given article IDs.
func (r *Repository) GetEmbeddings(articleIDs []int64) (map[int64][]float32, error) {
	if len(articleIDs) == 0 {
		return nil, nil
	}
	result := make(map[int64][]float32)

	// Build placeholder string for IN clause
	placeholders := make([]string, len(articleIDs))
	args := make([]interface{}, len(articleIDs))
	for i, id := range articleIDs {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}

	query := fmt.Sprintf(`
		SELECT article_id, embedding::text
		FROM ai_embeddings
		WHERE article_id IN (%s)
	`, strings.Join(placeholders, ","))

	rows, err := r.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var articleID int64
		var vecStr string
		if err := rows.Scan(&articleID, &vecStr); err != nil {
			continue
		}
		emb, err := parseVector(vecStr)
		if err != nil {
			continue
		}
		result[articleID] = emb
	}
	return result, rows.Err()
}

// SearchByEmbedding finds the closest articles to a query embedding using cosine distance.
func (r *Repository) SearchByEmbedding(queryEmbedding []float32, limit int) ([]int64, error) {
	vecStr := formatVector(queryEmbedding)
	// Use cosine distance threshold to filter out irrelevant results.
	// pgvector <=> returns cosine distance (0 = identical, 2 = opposite).
	// A threshold of 0.40 ensures only semantically relevant articles are returned.
	rows, err := r.DB.Query(`
		SELECT article_id, embedding <=> $1::vector AS dist
		FROM ai_embeddings
		WHERE embedding <=> $1::vector < 0.40
		ORDER BY dist
		LIMIT $2
	`, vecStr, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// formatVector converts a float32 slice to pgvector string format "[0.1,0.2,...]"
func formatVector(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = strconv.FormatFloat(float64(f), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// parseVector converts a pgvector string "[0.1,0.2,...]" to a float32 slice
func parseVector(s string) ([]float32, error) {
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	parts := strings.Split(s, ",")
	result := make([]float32, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, err
		}
		result[i] = float32(f)
	}
	return result, nil
}
