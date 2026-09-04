package summarize

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/gemini"
	"github.com/panospet/daily-news-aggregator/internal/models"
)

// mockGemini is a test double for gemini.Caller.
type mockGemini struct {
	response string
	err      error
}

func (m *mockGemini) Call(prompt string, schema *gemini.Schema) (string, error) {
	return m.response, m.err
}

func (m *mockGemini) CallGrouping(prompt string) (string, error) {
	return m.response, m.err
}

// sourceArticle builds an article whose Content clears minSourceWords.
func sourceArticle(title string) models.Article {
	sentence := "Η ομάδα ανακοίνωσε σήμερα τη νέα της απόφαση για το πρωτάθλημα και τους παίκτες. "
	return models.Article{
		Title:    title,
		Feed:     "Feed1",
		Category: "news",
		Content:  strings.Repeat(sentence, 4), // ~60 words
	}
}

func TestSummarize_Success(t *testing.T) {
	mock := &mockGemini{
		response: `{"summary": "Περίληψη ειδήσεων [1]", "topic": "Θέμα Τεστ"}`,
	}
	svc := &Service{Gemini: mock}

	result, err := svc.Summarize("Test Topic", []models.Article{
		sourceArticle("Article 1"),
		sourceArticle("Article 2"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "Περίληψη ειδήσεων" {
		t.Errorf("expected Greek summary, got %q", result.Summary)
	}
	if len(result.Sources) != 1 || result.Sources[0] != "Θέμα Τεστ" {
		t.Errorf("expected topic in sources, got %v", result.Sources)
	}
}

func TestSummarize_GeminiError(t *testing.T) {
	mock := &mockGemini{
		err: fmt.Errorf("API error"),
	}
	svc := &Service{Gemini: mock}

	_, err := svc.Summarize("Topic", []models.Article{sourceArticle("A")})
	if err == nil {
		t.Error("expected error when Gemini fails")
	}
}

func TestSummarize_WordLimit(t *testing.T) {
	mock := &mockGemini{
		response: `{"summary": "OK [1]", "topic": "T"}`,
	}
	svc := &Service{Gemini: mock}

	result, err := svc.Summarize("Topic", []models.Article{sourceArticle("Single")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "OK" {
		t.Errorf("expected OK, got %q", result.Summary)
	}
}

func TestSummarize_AllStubs_ReturnsError(t *testing.T) {
	mock := &mockGemini{response: `{"summary": "x", "topic": "y"}`}
	svc := &Service{Gemini: mock}

	stub := models.Article{Title: "teaser only", Feed: "F", Content: "too short"}
	if _, err := svc.Summarize("Topic", []models.Article{stub}); err == nil {
		t.Error("expected error when every article is a contentless stub")
	}
}

func TestSummarize_StubsDropped_UsableKept(t *testing.T) {
	var promptLen int
	mock := &countingMock{resp: `{"summary": "s [1]", "topic": "t"}`, seen: &promptLen}
	svc := &Service{Gemini: mock}

	arts := []models.Article{sourceArticle("real"), {Title: "stub", Content: "tiny"}}
	if _, err := svc.Summarize("Topic", arts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(mock.lastPrompt, "Title: stub") {
		t.Error("stub article should have been dropped from the prompt")
	}
	if !strings.Contains(mock.lastPrompt, "Title: real") {
		t.Error("usable article missing from prompt")
	}
}

type countingMock struct {
	resp       string
	err        error
	seen       *int
	lastPrompt string
}

func (m *countingMock) Call(prompt string, schema *gemini.Schema) (string, error) {
	*m.seen++
	m.lastPrompt = prompt
	return m.resp, m.err
}
func (m *countingMock) CallGrouping(prompt string) (string, error) { return m.resp, m.err }

func TestSummarize_RejectsFabricatedNumbers(t *testing.T) {
	mock := &mockGemini{
		response: `{"summary": "Η εταιρεία ανέφερε κέρδη 500 εκατομμυρίων [1].", "topic": "T"}`,
	}
	svc := &Service{Gemini: mock}

	_, err := svc.Summarize("Topic", []models.Article{sourceArticle("A")})
	if err == nil || !strings.Contains(err.Error(), "grounding check") {
		t.Errorf("expected grounding rejection for fabricated score, got %v", err)
	}
}

func TestSummarize_RejectsMissingCitations(t *testing.T) {
	mock := &mockGemini{
		response: `{"summary": "Σύνοψη χωρίς καμία παραπομπή σε πηγές.", "topic": "T"}`,
	}
	svc := &Service{Gemini: mock}

	if _, err := svc.Summarize("Topic", []models.Article{sourceArticle("A")}); err == nil ||
		!strings.Contains(err.Error(), "grounding check") {
		t.Errorf("expected grounding rejection for missing citations, got %v", err)
	}
}

func TestSummarize_AcceptsGroundedNumbers(t *testing.T) {
	art := sourceArticle("A")
	art.Content += " Ο τελικός βαθμός ήταν 3-1 στην χθεσινή αναμέτρηση του πρωταθλήματος."
	mock := &mockGemini{
		response: `{"summary": "Ο αγώνας τελείωσε 3-1 [1], σύμφωνα με το ρεπορτάζ.", "topic": "T"}`,
	}
	svc := &Service{Gemini: mock}

	if _, err := svc.Summarize("Topic", []models.Article{art}); err != nil {
		t.Errorf("grounded number should pass verification, got %v", err)
	}
}

func TestExtractTextFromHTML_DropsScriptsAndNav(t *testing.T) {
	h := `<html><head><style>.a{color:red}</style></head><body>
		<nav><a href="/">Αρχική</a> | <a href="/sports">Αθλητικά</a></nav>
		<article><p>` + strings.Repeat("Αυτή είναι μια δοκιμαστική πρόταση με αρκετό μήκος για φιλτράρισμα. ", 5) + `</p>
		<script>var x = 1;</script></article></body></html>`
	got := ExtractTextFromHTML([]byte(h), "text/html; charset=utf-8")
	if strings.Contains(got, "var x") || strings.Contains(got, ".a{") {
		t.Errorf("script/style leaked into text: %q", got)
	}
	if !strings.Contains(got, "δοκιμαστική πρόταση") {
		t.Errorf("article body lost: %q", got)
	}
	if strings.Contains(got, "Αρχική") {
		t.Errorf("short nav junk should be filtered: %q", got)
	}
}

func TestEnrichGroup_SkipsLongArticles(t *testing.T) {
	e := &Extractor{Client: &http.Client{Timeout: 1 * time.Second}}
	arts := []models.Article{sourceArticle("long enough already")}
	e.EnrichGroup(context.Background(), arts)
	// no fetches attempted; content untouched — nothing to assert beyond no panic
}
