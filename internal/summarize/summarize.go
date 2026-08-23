package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/gemini"
	"github.com/panospet/daily-news-aggregator/internal/models"
)

// Service handles AI-powered article summarization.
type Service struct {
	Gemini    gemini.Caller
	Extractor *Extractor // optional; when set, short articles are enriched before summarizing
}

// Summarize generates a summary for a group of articles about a topic.
//
// Anti-hallucination pipeline:
//  1. (optional) fetch full article text for teaser-stub articles
//  2. drop articles that remain too short to contribute facts
//  3. prompt Gemini to ground every claim in numbered sources and cite them
//  4. verify: reject the summary if it cites no sources or mentions
//     digits that appear in no source (score fabrication guard)
func (s *Service) Summarize(topic string, articles []models.Article) (models.AISummaryResult, error) {
	return s.SummarizeContext(context.Background(), topic, articles)
}

// SummarizeContext is Summarize with a caller-supplied context.
func (s *Service) SummarizeContext(ctx context.Context, topic string, articles []models.Article) (models.AISummaryResult, error) {
	arts := articles

	// 1. Best-effort enrichment of stub articles with real page text.
	if s.Extractor != nil {
		enrichCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		s.Extractor.EnrichGroup(enrichCtx, arts)
		cancel()
	}

	// 2. Drop contentless stubs — they carry titles, not facts.
	usable := make([]models.Article, 0, len(arts))
	for _, a := range arts {
		if len(strings.Fields(a.Content)) >= minSourceWords {
			usable = append(usable, a)
		}
	}
	if len(usable) == 0 {
		return models.AISummaryResult{}, fmt.Errorf("no article has enough content to summarize (all stubs)")
	}

	var sb strings.Builder

	template := fmt.Sprintf(`Είσαι βοηθός σύνοψης ειδήσεων. Θέμα: "%s"

Παρακάτω είναι άρθρα που καλύπτουν αυτό το θέμα, αριθμημένα [1], [2], ...

ΑΥΣΤΗΡΟΙ ΚΑΝΟΝΕΣ ΕΔΑΦΟΥΣ (grounding):
- Χρησιμοποιείς ΜΟΝΟ πληροφορίες που υπάρχουν ρητά στα παρακάτω άρθρα.
- ΑΠΑΓΟΡΕΥΕΤΑΙ να αναφέρεις σκορ, ονόματα, ημερομηνίες ή αριθμούς που δεν εμφανίζονται στα άρθρα.
- Αν τα άρθρα δεν συμφωνούν, αναφέρεις τι λένε τα άρθρα — δεν επιλέγεις εσύ "σωστό" στοιχείο.
- Αν δεν υπάρχει αρκετή πληροφορία για κάτι, το παραλείπεις.
- Μετά από κάθε πρόταση με γεγονότα βάζεις παραπομπή πηγής σε αγκύλη, π.χ. [1] ή [2][5].

1. Γράψε μια εκτενή και αναλυτική περίληψη (στα ΕΛΛΗΝΙΚΑ, 2-3 παραγράφους), με παραπομπές [n].
2. Σύνθεσε ΟΛΕΣ τις μοναδικές πληροφορίες από τα διάφορα άρθρα.
3. Γράψει έναν σύντομο τίτλο θεματικής (Topic) στα ΕΛΛΗΝΙΚΑ, αυστηρά βάσει των άρθρων.

CRITICAL INSTRUCTION: You MUST return a single, valid JSON object on ONE line. Do NOT pretty-print the JSON. For paragraphs, you MUST use escaped newline characters (\n\n) inside the "summary" string. Do not use actual literal newlines.
Return JSON: {"summary": "Περίληψη... [1]\n\n Επόμενη παράγραφος [2]", "topic": "Ελληνικός Τίτλος"}

Άρθρα:
`, topic)
	sb.WriteString(template)

	htmlRegex := regexp.MustCompile(`<[^>]*>`)

	var parsedData []articleData
	totalWords := 0

	for _, a := range usable {
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

	// 4. Verification pass — reject ungrounded output.
	if reason := verifyGrounded(parsed.Summary, parsedData); reason != "" {
		return models.AISummaryResult{}, fmt.Errorf("summary rejected by grounding check: %s", reason)
	}

	return models.AISummaryResult{
		Summary: parsed.Summary,
		Sources: []string{parsed.Topic},
	}, nil
}

// minSourceWords is the minimum content length for an article to count as a
// usable source. Shorter items are teaser stubs: titles without facts.
const minSourceWords = 50

// articleData is one numbered source prepared for the prompt.
type articleData struct {
	Title    string
	Feed     string
	Category string
	URL      string
	Words    []string
}

// citationRe matches [1] / [2][5] / [1,3] style citations.
var citationRe = regexp.MustCompile(`\[\s*(?:\d+\s*(?:,\s*\d+\s*)*)\]`)
var numRe = regexp.MustCompile(`\d+`)

// verifyGrounded returns "" when the summary passes, or a rejection reason.
//
// Checks:
//  1. at least one [n] citation, and every cited n exists
//  2. every multi-digit number appearing in the summary (scores, years,
//     statistics) must also appear in at least one source article
func verifyGrounded(summary string, sources []articleData) string {
	cites := citationRe.FindAllStringSubmatch(summary, -1)
	if len(cites) == 0 {
		return "no [n] citations found"
	}
	nSources := len(sources)
	for _, c := range cites {
		for _, numStr := range numRe.FindAllString(c[0], -1) {
			n := 0
			for _, ch := range numStr {
				n = n*10 + int(ch-'0')
			}
			if n < 1 || n > nSources {
				return fmt.Sprintf("citation [%d] out of range (%d sources)", n, nSources)
			}
		}
	}

	// Numbers that look like facts (2+ digits) must exist in the sources.
	// Single digits are too noisy (list indices, "11άδα" etc).
	summaryNums := map[string]bool{}
	for _, m := range numRe.FindAllString(summary, -1) {
		if len(m) >= 2 {
			summaryNums[m] = true
		}
	}
	if len(summaryNums) == 0 {
		return ""
	}
	sourceText := make([]string, len(sources))
	for i, s := range sources {
		sourceText[i] = s.Title + " " + strings.Join(s.Words, " ")
	}
	for num := range summaryNums {
		found := false
		for _, st := range sourceText {
			if strings.Contains(st, num) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("number %q appears in summary but in no source", num)
		}
	}
	return ""
}
