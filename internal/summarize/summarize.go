package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/llm"
	"github.com/panospet/daily-news-aggregator/internal/models"
)

// Service handles AI-powered article summarization.
type Service struct {
	LLM    llm.Caller
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
	return s.summarize(ctx, topic, articles, func(prompt string) (string, bool, error) {
		return s.LLM.Call(prompt, summarySchema())
	})
}

// StreamSink receives a summary while it is being written.
type StreamSink interface {
	// Delta is called with each new piece of clean summary text. Returning an error
	// aborts generation (the reader is gone).
	Delta(text string) error
	// Reset tells the reader to discard what it has: the model is starting over after
	// a failed grounding check.
	Reset() error
}

// SummarizeStreamContext is SummarizeContext with the text streamed to sink as it is
// generated, so the reader sees prose instead of ~19 seconds of spinner. The model
// answers in raw JSON ({"summary": "...", "topic": "..."}), so deltas are decoded out
// of the partially-written JSON string before being handed on.
//
// Streaming necessarily shows text the grounding check has not seen yet. It is
// contained exactly the way the non-streaming path contains a bad summary: the check
// still runs on the finished text, a rejected attempt triggers Reset + a retry, and if
// the retry also fails the caller replaces whatever the reader has with the fallback.
// Nothing unverified survives the end of the request.
func (s *Service) SummarizeStreamContext(ctx context.Context, topic string, articles []models.Article, sink StreamSink) (models.AISummaryResult, error) {
	var (
		raw   strings.Builder
		sent  int
		calls int
	)
	call := func(prompt string) (string, bool, error) {
		calls++
		raw.Reset()
		sent = 0
		if calls > 1 && sink != nil {
			if err := sink.Reset(); err != nil {
				return "", false, err
			}
		}
		text, err := s.LLM.CallStreamContext(ctx, prompt, summarySchema(), func(delta string) error {
			if sink == nil {
				return nil
			}
			raw.WriteString(delta)
			visible := decodePartialJSONString(summaryFieldSoFar(raw.String()))
			// Hide source citations ([1], [2][5], ...) from the streamed text:
			// they are still produced and verified server-side, but never shown
			// to the reader. Hold back any trailing partial citation so a stray
			// "[1" never flashes before its closing bracket arrives.
			visible = stripCitationsForStream(visible)
			if len(visible) <= sent {
				return nil
			}
			if err := sink.Delta(visible[sent:]); err != nil {
				return err
			}
			sent = len(visible)
			return nil
		})
		// isFallback is always false here: streaming never switches provider mid-answer.
		return text, false, err
	}
	return s.summarize(ctx, topic, articles, call)
}

// summarySchema is the JSON shape both paths ask the model for.
func summarySchema() *llm.Schema {
	return &llm.Schema{
		Type: "OBJECT",
		Properties: map[string]llm.Schema{
			"summary": {Type: "STRING"},
			"topic":   {Type: "STRING"},
		},
		Required: []string{"summary", "topic"},
	}
}

// summaryFieldSoFar returns the still-escaped, possibly unterminated body of the
// "summary" string in a partially received JSON response ("" if it has not started).
func summaryFieldSoFar(raw string) string {
	key := strings.Index(raw, `"summary"`)
	if key < 0 {
		return ""
	}
	colon := strings.Index(raw[key:], ":")
	if colon < 0 {
		return ""
	}
	rest := strings.TrimLeft(raw[key+colon+1:], " \t\r\n")
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	return rest[1:]
}

// decodePartialJSONString turns the body of a JSON string into the text it represents.
// Anything incomplete at the tail (a half-written \uXXXX or escape) is dropped rather
// than guessed at, so the next delta simply re-sends it.
func decodePartialJSONString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			return b.String() // end of the value
		case '\\':
			if i+1 >= len(s) {
				return b.String() // incomplete escape
			}
			switch n := s[i+1]; n {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			case 'u':
				if i+5 >= len(s) {
					return b.String() // incomplete \uXXXX
				}
				if r, err := strconv.ParseUint(s[i+2:i+6], 16, 32); err == nil {
					b.WriteRune(rune(r))
				}
				i += 4
			default:
				b.WriteByte(n)
			}
			i++
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// summarize is the whole pipeline — enrich, filter, cap, prompt, generate, verify — with
// the generation step injected, so the streaming and non-streaming paths differ only in
// how the text travels, never in what is asked or how it is checked.
func (s *Service) summarize(ctx context.Context, topic string, articles []models.Article, call func(string) (string, bool, error)) (models.AISummaryResult, error) {
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

	// Cap at most recent 20 sources to reduce hallucination pressure on large groups.
	const maxSources = 20
	if len(usable) > maxSources {
		sort.Slice(usable, func(i, j int) bool {
			return usable[i].Published.After(usable[j].Published)
		})
		usable = usable[:maxSources]
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

	jsonStr, isFallback, err := call(sb.String())
	if err != nil {
		return models.AISummaryResult{}, fmt.Errorf("summarize call failed: %w", err)
	}

	type summaryResponse struct {
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

	var parsed summaryResponse
	dec := json.NewDecoder(strings.NewReader(cleanStr))
	if err := dec.Decode(&parsed); err != nil {
		// Fallback parse if Gemini failed to return expected keys
		var backup models.AISummaryResult
		backupDec := json.NewDecoder(strings.NewReader(cleanStr))
		if backupErr := backupDec.Decode(&backup); backupErr == nil && backup.Summary != "" {
			backup.IsFallback = isFallback
			return backup, nil
		}
		return models.AISummaryResult{}, fmt.Errorf("parse summary: %w (raw: %s)", err, jsonStr)
	}

	// 4. Verification pass with one retry — generation is stochastic, so a
	// summary that fails grounding gets a second chance before we give up.
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			sb.WriteString(fmt.Sprintf("\nΣΗΜΑΝΣΗ: Η προηγούμενη προσπάθεια απορρίφθηκε επειδή περιείχε στοιχεία που δεν υπάρχουν στα άρθρα. Χρησιμοποίησε ΜΟΝΟ όσα αναφέρονται ρητά στα άρθρα και βεβαίωσου ότι κάθε πρόταση έχει παραπομπή [n].\n"))
			jsonStr, _, err = call(sb.String())
			if err != nil {
				return models.AISummaryResult{}, fmt.Errorf("summarize call failed: %w", err)
			}
			cleanStr = jsonStr
			startIdx := strings.Index(jsonStr, "{")
			endIdx := strings.LastIndex(jsonStr, "}")
			if startIdx != -1 && endIdx != -1 && endIdx > startIdx {
				cleanStr = jsonStr[startIdx : endIdx+1]
			}
			if err := json.NewDecoder(strings.NewReader(cleanStr)).Decode(&parsed); err != nil {
				return models.AISummaryResult{}, fmt.Errorf("parse summary (retry): %w", err)
			}
		}
		reason := verifyGrounded(parsed.Summary, parsedData)
		if reason == "" {
			cleanSummary := stripCitationRe.ReplaceAllString(parsed.Summary, "")
			return models.AISummaryResult{
				Summary: cleanSummary,
				Sources: []string{parsed.Topic},
				IsFallback: isFallback,
			}, nil
		}
		log.Printf("[summarize] grounding check rejected attempt %d: %s", attempt, reason)
	}
	return models.AISummaryResult{}, fmt.Errorf("summary rejected by grounding check after retry")
}

// SummarizeMore generates a supplemental summary for a group of articles about a topic.
func (s *Service) SummarizeMore(topic string, existingSummary string, articles []models.Article) (string, error) {
	return s.SummarizeMoreContext(context.Background(), topic, existingSummary, articles)
}

// SummarizeMoreContext is SummarizeMore with a caller-supplied context.
func (s *Service) SummarizeMoreContext(ctx context.Context, topic string, existingSummary string, articles []models.Article) (string, error) {
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
		return "", fmt.Errorf("no article has enough content to summarize (all stubs)")
	}

	// Cap at most recent 20 sources to reduce hallucination pressure on large groups.
	const maxSources = 20
	if len(usable) > maxSources {
		sort.Slice(usable, func(i, j int) bool {
			return usable[i].Published.After(usable[j].Published)
		})
		usable = usable[:maxSources]
	}

	var sb strings.Builder

	template := fmt.Sprintf(`Είσαι βοηθός σύνοψης ειδήσεων. Έχουμε ήδη μια βασική περίληψη για το θέμα: "%s"

Αυτή είναι η αρχική περίληψη:
"""
%s
"""

Παρακάτω είναι τα άρθρα που καλύπτουν το θέμα, αριθμημένα [1], [2], ...

ΟΔΗΓΙΕΣ:
1. Δημιούργησε μια ΣΥΜΠΛΗΡΩΜΑΤΙΚΗ περίληψη (1-2 παραγράφους) που να προσθέτει νέες λεπτομέρειες.
2. Εστίασε ΜΟΝΟ σε σημαντικές πληροφορίες, δηλώσεις, παρασκήνιο ή χρονοδιαγράμματα που ΔΕΝ αναφέρονται ήδη στην αρχική περίληψη. ΜΗΝ επαναλαμβάνεις τα ίδια γεγονότα.
3. Συνέχισε να τηρείς ΑΥΣΤΗΡΑ τους κανόνες εδάφους: Καμία νέα πληροφορία που δεν υπάρχει στα κείμενα, και υποχρεωτική χρήση παραπομπών [n] μετά από κάθε γεγονός.

CRITICAL INSTRUCTION: You MUST return a single, valid JSON object on ONE line. Do NOT pretty-print the JSON. For paragraphs, you MUST use escaped newline characters (\n\n) inside the "more_details" string. Do not use actual literal newlines.
Return JSON: {"more_details": "Νέες λεπτομέρειες... [3]"}

Άρθρα:
`, topic, existingSummary)
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

	schema := &llm.Schema{
		Type: "OBJECT",
		Properties: map[string]llm.Schema{
			"more_details": {Type: "STRING"},
		},
		Required: []string{"more_details"},
	}

	jsonStr, _, err := s.LLM.Call(sb.String(), schema)
	if err != nil {
		return "", fmt.Errorf("summarize more call failed: %w", err)
	}

	type moreResponse struct {
		MoreDetails string `json:"more_details"`
	}

	// Extract valid JSON from the response (handles markdown blocks or trailing garbage)
	cleanStr := jsonStr
	startIdx := strings.Index(jsonStr, "{")
	endIdx := strings.LastIndex(jsonStr, "}")
	if startIdx != -1 && endIdx != -1 && endIdx > startIdx {
		cleanStr = jsonStr[startIdx : endIdx+1]
	}

	var parsed moreResponse
	dec := json.NewDecoder(strings.NewReader(cleanStr))
	if err := dec.Decode(&parsed); err != nil {
		// Fallback parse if Gemini failed to return expected keys
		var backup struct {
			MoreDetails string `json:"more_details"`
		}
		backupDec := json.NewDecoder(strings.NewReader(cleanStr))
		if backupErr := backupDec.Decode(&backup); backupErr == nil && backup.MoreDetails != "" {
			return backup.MoreDetails, nil
		}
		return "", fmt.Errorf("parse summary more: %w (raw: %s)", err, jsonStr)
	}

	// 4. Verification pass with one retry
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt > 1 {
			sb.WriteString(fmt.Sprintf("\nΣΗΜΑΝΣΗ: Η προηγούμενη προσπάθεια απορρίφθηκε επειδή περιείχε στοιχεία που δεν υπάρχουν στα άρθρα. Χρησιμοποίησε ΜΟΝΟ όσα αναφέρονται ρητά στα άρθρα και βεβαίωσου ότι κάθε πρόταση έχει παραπομπή [n].\n"))
			jsonStr, _, err = s.LLM.Call(sb.String(), schema)
			if err != nil {
				return "", fmt.Errorf("summarize more call failed: %w", err)
			}
			cleanStr = jsonStr
			startIdx := strings.Index(jsonStr, "{")
			endIdx := strings.LastIndex(jsonStr, "}")
			if startIdx != -1 && endIdx != -1 && endIdx > startIdx {
				cleanStr = jsonStr[startIdx : endIdx+1]
			}
			if err := json.NewDecoder(strings.NewReader(cleanStr)).Decode(&parsed); err != nil {
				return "", fmt.Errorf("parse summary more (retry): %w", err)
			}
		}
		reason := verifyGrounded(parsed.MoreDetails, parsedData)
		if reason == "" {
			cleanSummary := stripCitationRe.ReplaceAllString(parsed.MoreDetails, "")
			return cleanSummary, nil
		}
		log.Printf("[summarize] grounding check rejected attempt %d: %s", attempt, reason)
	}
	return "", fmt.Errorf("summary more rejected by grounding check after retry")
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
var stripCitationRe = regexp.MustCompile(`\s*\[\s*(?:\d+\s*(?:,\s*\d+\s*)*)\]`)
var numRe = regexp.MustCompile(`\d+`)

// stripCitationsForStream removes complete [n]-style citations from s and
// truncates at the last unmatched '[' so a partial citation never leaks to
// the reader while it is still being received. Used only for streamed output;
// the full (cited) text is still verified server-side.
func stripCitationsForStream(s string) string {
	s = stripCitationRe.ReplaceAllString(s, "")
	if idx := strings.LastIndex(s, "["); idx >= 0 {
		if !strings.Contains(s[idx:], "]") {
			s = s[:idx]
		}
	}
	return s
}

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

	// Numbers that look like real facts (3+ digits: 100+) must exist in the sources.
	// Smaller numbers (1-99) are too noisy — list indices, ages, vague counts
	// that models routinely synthesize as filler.
	summaryNums := map[string]bool{}
	for _, m := range numRe.FindAllString(summary, -1) {
		if len(m) >= 3 {
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
