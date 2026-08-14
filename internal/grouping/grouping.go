package grouping

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/gemini"
	"github.com/panospet/daily-news-aggregator/internal/models"
)

// Service handles AI-powered article grouping.
type Service struct {
	Gemini gemini.Caller
}

// GroupArticles calls the Python AI service to cluster articles.
// Falls back to flat grouping if the service fails.
func (s *Service) GroupArticles(allArticles []models.Article) ([]models.AICategoryGroup, error) {
	tTotal := time.Now()

	reqBody, err := json.Marshal(map[string]interface{}{"articles": allArticles})
	if err != nil {
		return s.fallbackFlat(allArticles), nil
	}

	client := &http.Client{Timeout: 1200 * time.Second}
	resp, err := client.Post("http://127.0.0.1:8001/cluster", "application/json", bytes.NewBuffer(reqBody))
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
	return data.Categories, nil
}

func (s *Service) fallbackFlat(allArticles []models.Article) []models.AICategoryGroup {
	catMap := make(map[string][]models.Article)
	for _, a := range allArticles {
		catMap[a.Category] = append(catMap[a.Category], a)
	}
	preferredOrder := []string{"international", "news", "sports", "music", "politics", "random", "panionara"}
	return FlatGroupResult(catMap, preferredOrder)
}

