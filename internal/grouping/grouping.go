package grouping

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/database"
	"github.com/panospet/daily-news-aggregator/internal/gemini"
	"github.com/panospet/daily-news-aggregator/internal/models"
)

// Service handles AI-powered article grouping.
type Service struct {
	Gemini gemini.Caller
	Repo   *database.Repository
	// BaseURL of the Python AI clustering service, e.g. http://127.0.0.1:8001
	AIServiceURL string
}

// GroupArticles calls the Python AI service to cluster articles.
// Falls back to flat grouping if the service fails.
func (s *Service) GroupArticles(allArticles []models.Article) ([]models.AICategoryGroup, error) {
	tTotal := time.Now()

	// Collect article IDs and fetch pre-computed embeddings
	var articleIDs []int64
	for _, a := range allArticles {
		articleIDs = append(articleIDs, a.ID)
	}

	var precomputed map[int64][]float32
	if s.Repo != nil {
		var err error
		precomputed, err = s.Repo.GetEmbeddings(articleIDs)
		if err != nil {
			log.Printf("[clustering] Failed to fetch pre-computed embeddings: %v", err)
		} else {
			log.Printf("[clustering] Found %d/%d pre-computed embeddings", len(precomputed), len(articleIDs))
		}
	}

	// Build request (embeddings are NOT sent in the payload to keep it small;
	// the AI service computes them on the fly, and we store them afterwards for search)
	reqPayload := map[string]interface{}{"articles": allArticles}

	reqBody, err := json.Marshal(reqPayload)
	if err != nil {
		return s.fallbackFlat(allArticles), nil
	}

	client := &http.Client{Timeout: 1800 * time.Second}
	resp, err := client.Post(s.AIServiceURL+"/cluster", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		log.Printf("[clustering] AI service call failed: %v", err)
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("[clustering] AI service call failed with status: %d", resp.StatusCode)
		return nil, fmt.Errorf("AI service returned status %d", resp.StatusCode)
	}

	var data models.AIDashboardData
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Printf("[clustering] failed to unmarshal AI service response: %v", err)
		return s.fallbackFlat(allArticles), nil
	}

	log.Printf("[clustering] Remote AI clustering completed in %v", time.Since(tTotal))

	// Compute and store embeddings for articles that were missing them
	if s.Repo != nil {
		var missing []models.Article
		for _, a := range allArticles {
			if _, ok := precomputed[a.ID]; !ok {
				missing = append(missing, a)
			}
		}
		if len(missing) > 0 {
			go s.computeAndStoreEmbeddings(missing)
		}
	}

	return data.Categories, nil
}

// computeAndStoreEmbeddings calls the AI embed endpoint for articles missing embeddings
// and stores them in Postgres. Runs in a background goroutine.
func (s *Service) computeAndStoreEmbeddings(articles []models.Article) {
	log.Printf("[embeddings] Computing embeddings for %d new articles", len(articles))

	reqBody, err := json.Marshal(map[string]interface{}{"articles": articles})
	if err != nil {
		log.Printf("[embeddings] Failed to marshal request: %v", err)
		return
	}

	client := &http.Client{Timeout: 1800 * time.Second}
	resp, err := client.Post(s.AIServiceURL+"/embed", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		log.Printf("[embeddings] Failed to call embed endpoint: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[embeddings] Embed endpoint returned status %d", resp.StatusCode)
		return
	}

	var result struct {
		Embeddings map[string][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[embeddings] Failed to decode embed response: %v", err)
		return
	}

	// Convert string keys back to int64
	embeddings := make(map[int64][]float32, len(result.Embeddings))
	for idStr, emb := range result.Embeddings {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		embeddings[id] = emb
	}

	if err := s.Repo.SaveEmbeddings(embeddings); err != nil {
		log.Printf("[embeddings] Failed to save embeddings: %v", err)
	} else {
		log.Printf("[embeddings] Saved %d new embeddings to Postgres", len(embeddings))
	}
}

func (s *Service) fallbackFlat(allArticles []models.Article) []models.AICategoryGroup {
	catMap := make(map[string][]models.Article)
	for _, a := range allArticles {
		catMap[a.Category] = append(catMap[a.Category], a)
	}
	preferredOrder := []string{"international", "news", "sports", "music", "politics", "random", "panionara"}
	return FlatGroupResult(catMap, preferredOrder)
}
