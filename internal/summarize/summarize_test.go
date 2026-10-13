package summarize

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/llm"
	"github.com/panospet/daily-news-aggregator/internal/models"
)

// mockLLM is a test double for llm.Caller.
type mockLLM struct {
	response string
	err      error
}

func (m *mockLLM) Call(prompt string, schema *llm.Schema) (string, bool, error) {
	return m.response, false, m.err
}

func (m *mockLLM) CallGrouping(prompt string) (string, bool, error) {
	return m.response, false, m.err
}

func (m *mockLLM) CallStreamContext(ctx context.Context, prompt string, schema *llm.Schema, onDelta func(string) error) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if onDelta != nil {
		if err := onDelta(m.response); err != nil {
			return "", err
		}
	}
	return m.response, nil
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
	mock := &mockLLM{
		response: `{"summary": "Περίληψη ειδήσεων [1]", "topic": "Θέμα Τεστ"}`,
	}
	svc := &Service{LLM: mock}

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

func TestSummarize_LLMError(t *testing.T) {
	mock := &mockLLM{
		err: fmt.Errorf("API error"),
	}
	svc := &Service{LLM: mock}

	_, err := svc.Summarize("Topic", []models.Article{sourceArticle("A")})
	if err == nil {
		t.Error("expected error when Gemini fails")
	}
}

func TestSummarize_WordLimit(t *testing.T) {
	mock := &mockLLM{
		response: `{"summary": "OK [1]", "topic": "T"}`,
	}
	svc := &Service{LLM: mock}

	result, err := svc.Summarize("Topic", []models.Article{sourceArticle("Single")})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Summary != "OK" {
		t.Errorf("expected OK, got %q", result.Summary)
	}
}

func TestSummarize_AllStubs_ReturnsError(t *testing.T) {
	mock := &mockLLM{response: `{"summary": "x", "topic": "y"}`}
	svc := &Service{LLM: mock}

	stub := models.Article{Title: "teaser only", Feed: "F", Content: "too short"}
	if _, err := svc.Summarize("Topic", []models.Article{stub}); err == nil {
		t.Error("expected error when every article is a contentless stub")
	}
}

func TestSummarize_StubsDropped_UsableKept(t *testing.T) {
	var promptLen int
	mock := &countingMock{resp: `{"summary": "s [1]", "topic": "t"}`, seen: &promptLen}
	svc := &Service{LLM: mock}

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

func (m *countingMock) Call(prompt string, schema *llm.Schema) (string, bool, error) {
	*m.seen++
	m.lastPrompt = prompt
	return m.resp, false, m.err
}
func (m *countingMock) CallGrouping(prompt string) (string, bool, error) {
	return m.resp, false, m.err
}
func (m *countingMock) CallStreamContext(ctx context.Context, prompt string, schema *llm.Schema, onDelta func(string) error) (string, error) {
	*m.seen++
	m.lastPrompt = prompt
	if m.err != nil {
		return "", m.err
	}
	if onDelta != nil {
		if err := onDelta(m.resp); err != nil {
			return "", err
		}
	}
	return m.resp, nil
}

// streamingMock hands its canned model output over ONE BYTE AT A TIME — the worst case
// the partial-JSON decoder has to survive, since every frame can end mid-escape or
// mid-UTF-8-character.
type streamingMock struct {
	response string
	err      error
}

func (m *streamingMock) Call(prompt string, schema *llm.Schema) (string, bool, error) {
	return m.response, false, m.err
}
func (m *streamingMock) CallGrouping(prompt string) (string, bool, error) {
	return m.response, false, m.err
}
func (m *streamingMock) CallStreamContext(ctx context.Context, prompt string, schema *llm.Schema, onDelta func(string) error) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	for i := 0; i < len(m.response); i++ {
		if err := onDelta(m.response[i : i+1]); err != nil {
			return "", err
		}
	}
	return m.response, nil
}

// recordingSink captures what a reader would have been shown.
type recordingSink struct {
	chunks []string
	resets int
}

func (r *recordingSink) Delta(text string) error {
	r.chunks = append(r.chunks, text)
	return nil
}
func (r *recordingSink) Reset() error {
	r.resets++
	r.chunks = nil
	return nil
}

func TestSummarizeStream_DecodesCleanText(t *testing.T) {
	mock := &streamingMock{response: `{"summary": "Πρώτη παράγραφος με στοιχεία [1].\n\nΔεύτερη παράγραφος με λεπτομέρειες [2].", "topic": "Θέμα Τεστ"}`}
	svc := &Service{LLM: mock}
	sink := &recordingSink{}

	result, err := svc.SummarizeStreamContext(context.Background(), "Θέμα", []models.Article{
		sourceArticle("Article 1"), sourceArticle("Article 2"),
	}, sink)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	streamed := strings.Join(sink.chunks, "")
	if !strings.Contains(streamed, "Πρώτη παράγραφος") || !strings.Contains(streamed, "Δεύτερη παράγραφος") {
		t.Errorf("streamed text incomplete: %q", streamed)
	}
	// The reader must never see JSON furniture or escape sequences.
	for _, junk := range []string{`{`, `"summary"`, `\n`, `\\`} {
		if strings.Contains(streamed, junk) {
			t.Errorf("raw JSON leaked into the stream (%q): %q", junk, streamed)
		}
	}
	if !strings.Contains(streamed, "\n\n") {
		t.Errorf("escaped newlines should arrive as real paragraph breaks: %q", streamed)
	}
	if !strings.Contains(result.Summary, "Δεύτερη παράγραφος") {
		t.Errorf("verified result missing streamed text: %q", result.Summary)
	}
	if len(result.Sources) != 1 || result.Sources[0] != "Θέμα Τεστ" {
		t.Errorf("expected topic in sources, got %v", result.Sources)
	}
}

func TestSummarizeStream_RejectionResetsReader(t *testing.T) {
	// No citations and a fabricated number: fails verification on both attempts.
	mock := &streamingMock{response: `{"summary": "Κείμενο χωρίς παραπομπές για 500 νεκρούς.", "topic": "T"}`}
	svc := &Service{LLM: mock}
	sink := &recordingSink{}

	if _, err := svc.SummarizeStreamContext(context.Background(), "T", []models.Article{sourceArticle("A")}, sink); err == nil {
		t.Fatal("expected the grounding check to reject this summary")
	}
	// The retry must clear the reader first, otherwise the second attempt's text would
	// be appended to the rejected one.
	if sink.resets == 0 {
		t.Error("reader should be reset when a rejected attempt is regenerated")
	}
	if got := strings.Join(sink.chunks, ""); !strings.Contains(got, "500") {
		t.Errorf("second attempt's text should be what the reader holds, got %q", got)
	}
}

func TestSummarizeStream_AcceptsGroundedNumbers(t *testing.T) {
	art := sourceArticle("A")
	art.Content += " Ο τελικός βαθμός ήταν 3-1 στην χθεσινή αναμέτρηση του πρωταθλήματος."
	mock := &streamingMock{response: `{"summary": "Ο αγώνας τελείωσε 3-1 [1], σύμφωνα με το ρεπορτάζ.", "topic": "T"}`}
	svc := &Service{LLM: mock}
	sink := &recordingSink{}

	result, err := svc.SummarizeStreamContext(context.Background(), "T", []models.Article{art}, sink)
	if err != nil {
		t.Fatalf("grounded number should pass verification, got %v", err)
	}
	if !strings.Contains(result.Summary, "3-1") {
		t.Errorf("streamed summary lost the grounded number: %q", result.Summary)
	}
}

func TestSummarize_RejectsFabricatedNumbers(t *testing.T) {
	mock := &mockLLM{
		response: `{"summary": "Η εταιρεία ανέφερε κέρδη 500 εκατομμυρίων [1].", "topic": "T"}`,
	}
	svc := &Service{LLM: mock}

	_, err := svc.Summarize("Topic", []models.Article{sourceArticle("A")})
	if err == nil || !strings.Contains(err.Error(), "grounding check") {
		t.Errorf("expected grounding rejection for fabricated score, got %v", err)
	}
}

func TestSummarize_RejectsMissingCitations(t *testing.T) {
	mock := &mockLLM{
		response: `{"summary": "Σύνοψη χωρίς καμία παραπομπή σε πηγές.", "topic": "T"}`,
	}
	svc := &Service{LLM: mock}

	if _, err := svc.Summarize("Topic", []models.Article{sourceArticle("A")}); err == nil ||
		!strings.Contains(err.Error(), "grounding check") {
		t.Errorf("expected grounding rejection for missing citations, got %v", err)
	}
}

func TestSummarize_AcceptsGroundedNumbers(t *testing.T) {
	art := sourceArticle("A")
	art.Content += " Ο τελικός βαθμός ήταν 3-1 στην χθεσινή αναμέτρηση του πρωταθλήματος."
	mock := &mockLLM{
		response: `{"summary": "Ο αγώνας τελείωσε 3-1 [1], σύμφωνα με το ρεπορτάζ.", "topic": "T"}`,
	}
	svc := &Service{LLM: mock}

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
