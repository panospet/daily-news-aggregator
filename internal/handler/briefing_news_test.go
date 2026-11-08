package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panospet/daily-news-aggregator/internal/cache"
	"github.com/panospet/daily-news-aggregator/internal/llm"
	"github.com/panospet/daily-news-aggregator/internal/models"
	"github.com/panospet/daily-news-aggregator/internal/summarize"
)

type stubCaller struct {
	reply string
	err   error
}

func (s *stubCaller) Call(prompt string, schema *llm.Schema) (string, bool, error) {
	return s.reply, false, s.err
}

func (s *stubCaller) CallContext(ctx context.Context, prompt string, schema *llm.Schema) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return s.reply, false, s.err
}

func (s *stubCaller) CallGrouping(prompt string) (string, bool, error) {
	return s.reply, false, s.err
}

func (s *stubCaller) CallStreamContext(ctx context.Context, prompt string, schema *llm.Schema, onDelta func(string) error) (string, error) {
	return s.reply, s.err
}

func TestParseHHMM(t *testing.T) {
	got, err := parseHHMM("07:45")
	if err != nil || got != 7*60+45 {
		t.Fatalf("parseHHMM(07:45) = (%d, %v), want (%d, nil)", got, err, 7*60+45)
	}
	if _, err := parseHHMM("7.45"); err == nil {
		t.Fatal("expected error for malformed time 7.45")
	}
	if _, err := parseHHMM("25:00"); err == nil {
		t.Fatal("expected error for out-of-range hour 25:00")
	}
}

func TestHandleBriefingNews_ValidationAndUnavailable(t *testing.T) {
	srv := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/api/briefing-news?slot=invalid", nil)
	w := httptest.NewRecorder()
	srv.HandleBriefingNews(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid slot, got %d", w.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/briefing-news?slot=morning", nil)
	w2 := httptest.NewRecorder()
	srv.HandleBriefingNews(w2, req2)
	if w2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when NewsBlock store is nil, got %d", w2.Code)
	}
}

func TestSummarizeGroup_RespectsVerifiedSummaryAndRejectsFailure(t *testing.T) {
	longContent := strings.Repeat("αυτό είναι ένα πλήρες άρθρο με αρκετές λέξεις για έλεγχο ", 12)
	tg := models.TopGroup{
		Category: "Ελλάδα",
		Group: models.AIGroup{
			GroupID: "g1",
			Topic:   "Αρχικός Τίτλος",
			Articles: []models.Article{
				{ID: 1, Title: "Τίτλος Άρθρου", Feed: "FeedA", Content: longContent},
			},
		},
	}

	// 1. Failing LLM -> summarizeGroup returns false (not bare headline).
	failSrv := &Server{
		Summarizer: &summarize.Service{
			LLM: &stubCaller{err: context.DeadlineExceeded},
		},
	}
	if _, ok := failSrv.summarizeGroup(context.Background(), tg); ok {
		t.Fatal("expected summarizeGroup to fail when LLM fails")
	}

	// 2. Valid grounded LLM response -> returns speech-ready text with [1] stripped.
	okSrv := &Server{
		Summarizer: &summarize.Service{
			LLM: &stubCaller{
				reply: `{"summary":"Η είδηση επιβεβαιώνεται από τις πηγές [1].","topic":"Νέος Τίτλος"}`,
			},
		},
	}
	item, ok := okSrv.summarizeGroup(context.Background(), tg)
	if !ok {
		t.Fatal("expected summarizeGroup to succeed")
	}
	if item.Topic != "Νέος Τίτλος" {
		t.Fatalf("expected topic 'Νέος Τίτλος', got %q", item.Topic)
	}
	if strings.Contains(item.SummaryText, "[1]") {
		t.Fatalf("expected citations stripped, got %q", item.SummaryText)
	}
}

func TestSelectAndOrderBriefingSportsCappedAndLast(t *testing.T) {
	top := []models.TopGroup{
		{Category: "Ελλάδα", Group: models.AIGroup{Topic: "Ολυμπιακός – Ερυθρός Αστέρας 57-62", Articles: []models.Article{{Category: "sports"}, {Category: "sports"}}}},
		{Category: "Ελλάδα", Group: models.AIGroup{Topic: "News 1"}},
		{Category: "Αθλητισμός", Group: models.AIGroup{Topic: "NBA: Ντόντσιτς 53 πόντοι"}},
		{Category: "Κόσμος", Group: models.AIGroup{Topic: "News 2"}},
		{Category: "Ελλάδα", Group: models.AIGroup{Topic: "ΠΑΟΚ: Νέο συμβόλαιο Ζίβκοβιτς"}},
		{Category: "Αθλητισμός", Group: models.AIGroup{Topic: "EuroLeague βαθμολογία"}},
		{Category: "Οικονομία & Ενέργεια", Group: models.AIGroup{Topic: "News 3"}},
		{Category: "Κοινωνία", Group: models.AIGroup{Topic: "News 4"}},
		{Category: "Πολιτική & Διπλωματία", Group: models.AIGroup{Topic: "News 5"}},
		{Category: "Καιρός", Group: models.AIGroup{Topic: "News 6"}},
		{Category: "Τεχνολογία & Επιστήμη", Group: models.AIGroup{Topic: "News 7"}},
		{Category: "Μουσική", Group: models.AIGroup{Topic: "News 8"}},
		{Category: "Ελλάδα", Group: models.AIGroup{Topic: "News 9"}},
	}

	selected := selectBriefingGroups(top, 10, 2)
	if len(selected) != 10 {
		t.Fatalf("expected 10 groups, got %d", len(selected))
	}
	for i := 0; i < 8; i++ {
		if selected[i].Category == sportsCategory {
			t.Fatalf("expected non-sports at index %d, got %+v", i, selected[i])
		}
	}
	for i := 8; i < 10; i++ {
		if selected[i].Category != sportsCategory {
			t.Fatalf("expected sports at index %d, got %+v", i, selected[i])
		}
	}

	items := []cache.NewsBlockItem{
		{Topic: "Ολυμπιακός – Ερυθρός Αστέρας", Category: "Ελλάδα"},
		{Topic: "News A", Category: "Ελλάδα"},
		{Topic: "NBA Finals", Category: "Αθλητισμός"},
		{Topic: "ΠΑΟΚ μεταγραφές", Category: "Ελλάδα"},
		{Topic: "News B", Category: "Κόσμος"},
	}
	ordered := orderAndCapBriefingItems(items, 10, 2)
	if len(ordered) != 4 { // 2 non-sports + 2 sports (3rd sports dropped)
		t.Fatalf("expected 4 items after capping sports to 2, got %d", len(ordered))
	}
	if ordered[0].Topic != "News A" || ordered[1].Topic != "News B" {
		t.Fatalf("expected non-sports first, got %+v", ordered[:2])
	}
	if ordered[2].Category != sportsCategory || ordered[3].Category != sportsCategory {
		t.Fatalf("expected 2 sports items last, got %+v", ordered[2:])
	}
}
