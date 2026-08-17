package summarize

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/panospet/daily-news-aggregator/internal/gemini"
	"github.com/panospet/daily-news-aggregator/internal/models"
)

// Service handles AI-powered article summarization.
type Service struct {
	Gemini gemini.Caller
}

// Summarize generates a summary for a group of articles about a topic.
func (s *Service) Summarize(topic string, articles []models.Article) (models.AISummaryResult, error) {
	var sb strings.Builder

	template := fmt.Sprintf(`Είσαι βοηθός σύνοψης ειδήσεων. Θέμα: "%s"

Παρακάτω είναι άρθρα που καλύπτουν αυτό το θέμα. 
1. Γράψε μια εκτενή και αναλυτική περίληψη (στα ΕΛΛΗΝΙΚΑ, περίπου 2-3 παραγράφους).
2. Σύνθεσε ΟΛΕΣ τις μοναδικές πληροφορίες από τα διάφορα άρθρα.
3. Γράψε έναν σύντομο τίτλο θεματικής (Topic) στα ΕΛΛΗΝΙΚΑ.

CRITICAL INSTRUCTION: You MUST return a single, valid JSON object on ONE line. Do NOT pretty-print the JSON. For paragraphs, you MUST use escaped newline characters (\n\n) inside the "summary" string. Do not use actual literal newlines.
Return JSON: {"summary": "Περίληψη... \n\n Επόμενη παράγραφος", "topic": "Ελληνικός Τίτλος"}

Άρθρα:
`, topic)
	sb.WriteString(template)

	htmlRegex := regexp.MustCompile(`<[^>]*>`)

	type articleData struct {
		Title    string
		Feed     string
		Category string
		URL      string
		Words    []string
	}

	var parsedData []articleData
	totalWords := 0

	for _, a := range articles {
		plainContent := htmlRegex.ReplaceAllString(a.Content, " ")
		words := strings.Fields(plainContent)
		parsedData = append(parsedData, articleData{
			Title:    a.Title,
			Feed:     a.Feed,
			Category: a.Category,
			URL:      a.URL,
			Words:    words,
		})
		totalWords += len(words)
	}

	// Gemini 2.5 Flash Lite context is 1M tokens (~750k words).
	// We'll consider 500k words as the absolute safety capacity, and 80% of that is 400k.
	const contextCapacity = 500000
	const targetCapacity = int(contextCapacity * 0.8) // 400,000 words

	if totalWords > contextCapacity {
		excessWords := totalWords - targetCapacity
		wordsToCutPerArticle := excessWords / len(parsedData)

		for i := range parsedData {
			if len(parsedData[i].Words) > wordsToCutPerArticle {
				// Cut equally from the end
				parsedData[i].Words = parsedData[i].Words[:len(parsedData[i].Words)-wordsToCutPerArticle]
			} else {
				parsedData[i].Words = []string{} // If it's too short, cut it all
			}
		}
	}

	for i, pd := range parsedData {
		contentStr := strings.Join(pd.Words, " ")
		sb.WriteString(fmt.Sprintf("[%d] Title: %s\n   Source: %s (%s)\n   URL: %s\n   Content: %s\n\n", i+1, pd.Title, pd.Feed, pd.Category, pd.URL, contentStr))
	}

	schema := &gemini.Schema{
		Type: "OBJECT",
		Properties: map[string]gemini.Schema{
			"summary": {Type: "STRING"},
			"topic":   {Type: "STRING"},
		},
		Required: []string{"summary", "topic"},
	}

	jsonStr, err := s.Gemini.Call(sb.String(), schema)
	if err != nil {
		return models.AISummaryResult{}, fmt.Errorf("summarize call failed: %w", err)
	}

	type geminiSummaryResponse struct {
		Summary string `json:"summary"`
		Topic   string `json:"topic"`
	}

	// Extract valid JSON from the response (handles markdown blocks or trailing garbage)
	cleanStr := jsonStr
	startIdx := strings.Index(jsonStr, "{")
	endIdx := strings.LastIndex(jsonStr, "}")
	if startIdx != -1 && endIdx != -1 && endIdx > startIdx {
		cleanStr = jsonStr[startIdx : endIdx+1]
	}

	var parsed geminiSummaryResponse
	dec := json.NewDecoder(strings.NewReader(cleanStr))
	if err := dec.Decode(&parsed); err != nil {
		// Fallback parse if Gemini failed to return expected keys
		var backup models.AISummaryResult
		backupDec := json.NewDecoder(strings.NewReader(cleanStr))
		if backupErr := backupDec.Decode(&backup); backupErr == nil && backup.Summary != "" {
			return backup, nil
		}
		return models.AISummaryResult{}, fmt.Errorf("parse summary: %w (raw: %s)", err, jsonStr)
	}

	return models.AISummaryResult{
		Summary: parsed.Summary,
		Sources: []string{parsed.Topic},
	}, nil
}
