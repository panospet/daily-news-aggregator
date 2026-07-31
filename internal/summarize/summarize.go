package summarize

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/panospet/htmx-daily-news/internal/gemini"
	"github.com/panospet/htmx-daily-news/internal/models"
)

// Service handles AI-powered article summarization.
type Service struct {
	Gemini gemini.Caller
}

// Summarize generates a summary for a group of articles about a topic.
func (s *Service) Summarize(topic string, articles []models.Article) (models.AISummaryResult, error) {
	var sb strings.Builder

	// 50 words per source, max 500
	wordLimit := len(articles) * 50
	if wordLimit > 500 {
		wordLimit = 500
	}
	if wordLimit < 50 {
		wordLimit = 50
	}

	template := fmt.Sprintf(`Είσαι βοηθός σύνοψης ειδήσεων. Θέμα: "%s"

Παρακάτω είναι άρθρα που καλύπτουν αυτό το θέμα. 
1. Γράψε μια σύντομη περίληψη (έως %d λέξεις, στα ΕΛΛΗΝΙΚΑ) που να εξηγεί τι συνέβη.
2. Επίσης, γράψε έναν κατάλληλο, ελκυστικό και σύντομο τίτλο θεματικής (Topic) στα ΕΛΛΗΝΙΚΑ για αυτό το γκρουπ ειδήσεων, ο οποίος να αντικατοπτρίζει σωστά το περιεχόμενο.

Return JSON: {"summary": "Περίληψη...", "topic": "Ελληνικός Τίτλος Θέματος"}

Άρθρα:
`, topic, wordLimit)
	sb.WriteString(template)

	for i, a := range articles {
		sb.WriteString(fmt.Sprintf("[%d] Title: %s\n   Source: %s (%s)\n   URL: %s\n\n", i+1, a.Title, a.Feed, a.Category, a.URL))
	}

	jsonStr, err := s.Gemini.Call(sb.String())
	if err != nil {
		return models.AISummaryResult{}, fmt.Errorf("summarize call failed: %w", err)
	}

	type geminiSummaryResponse struct {
		Summary string `json:"summary"`
		Topic   string `json:"topic"`
	}

	var parsed geminiSummaryResponse
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		// Fallback parse if Gemini failed to return expected keys
		var backup models.AISummaryResult
		if backupErr := json.Unmarshal([]byte(jsonStr), &backup); backupErr == nil && backup.Summary != "" {
			return backup, nil
		}
		return models.AISummaryResult{}, fmt.Errorf("parse summary: %w", err)
	}

	return models.AISummaryResult{
		Summary: parsed.Summary,
		Sources: []string{parsed.Topic},
	}, nil
}
