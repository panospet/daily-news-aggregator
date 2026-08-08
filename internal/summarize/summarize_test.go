package summarize

import (
	"fmt"
	"testing"

	"github.com/panospet/htmx-daily-news/internal/models"
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

func TestSummarize_Success(t *testing.T) {
	mock := &mockGemini{
		response: `{"summary": "Περίληψη ειδήσεων", "topic": "Θέμα Τεστ"}`,
	}
	svc := &Service{Gemini: mock}

	result, err := svc.Summarize("Test Topic", []models.Article{
		{Title: "Article 1", Feed: "Feed1", Category: "news", URL: "http://example.com/1"},
		{Title: "Article 2", Feed: "Feed2", Category: "news", URL: "http://example.com/2"},
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

	_, err := svc.Summarize("Topic", []models.Article{{Title: "A"}})
	if err == nil {
		t.Error("expected error when Gemini fails")
	}
}

func TestSummarize_WordLimit(t *testing.T) {
	mock := &mockGemini{
		response: `{"summary": "OK", "topic": "T"}`,
	}
	svc := &Service{Gemini: mock}

	// Single article should get wordLimit = 50
	articles := []models.Article{{Title: "Single"}}
	result, err := svc.Summarize("Topic", articles)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "OK" {
		t.Errorf("expected OK, got %q", result.Summary)
	}
}
