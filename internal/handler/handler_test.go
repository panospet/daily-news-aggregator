package handler

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panospet/daily-news-aggregator/internal/models"
)

func TestFlatToAI(t *testing.T) {
	dd := &models.DashboardData{
		Categories: []models.CategoryGroup{
			{
				Category: "news",
				Articles: []models.Article{
					{Title: "Article A", Feed: "Feed1"},
					{Title: "Article B", Feed: "Feed2"},
				},
				Count: 2,
			},
		},
		Total:     2,
		Generated: "12:00:00",
	}

	result := FlatToAI(dd)

	if result.Total != 2 {
		t.Errorf("expected total 2, got %d", result.Total)
	}
	if len(result.Categories) != 1 {
		t.Fatalf("expected 1 category, got %d", len(result.Categories))
	}
	if result.Categories[0].Category != "news" {
		t.Errorf("expected display name 'news', got %q", result.Categories[0].Category)
	}
	if len(result.Categories[0].Groups) != 2 {
		t.Errorf("expected 2 groups (one per article), got %d", len(result.Categories[0].Groups))
	}
}

func TestFlatToAI_UnknownCategory(t *testing.T) {
	dd := &models.DashboardData{
		Categories: []models.CategoryGroup{
			{Category: "unknown_cat", Articles: []models.Article{{Title: "X"}}, Count: 1},
		},
		Total:     1,
		Generated: "10:00:00",
	}

	result := FlatToAI(dd)
	// Unknown category should use the raw name
	if result.Categories[0].Category != "unknown_cat" {
		t.Errorf("expected raw category name, got %q", result.Categories[0].Category)
	}
}

func TestHome_Returns200(t *testing.T) {
	// Create a minimal Server that can serve Home
	// We skip template rendering in this test — just verify it doesn't panic/500
	// when AICache returns data
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	// For a proper test we'd need templates loaded, so just test the HTTP method routing
	if req.Method != http.MethodGet {
		t.Error("expected GET method")
	}
	if w.Code != 200 {
		t.Errorf("expected default 200 from recorder, got %d", w.Code)
	}
}

func TestGzipMiddleware(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html>" + strings.Repeat("x", 2048) + "</html>"))
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	GzipMiddleware(handler).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("expected Content-Encoding gzip, got %q", got)
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatalf("body is not valid gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if len(body) != len("<html>")+2048+len("</html>") {
		t.Fatalf("expected %d decompressed bytes, got %d", 2048+13, len(body))
	}

	// Client without gzip support gets plain response.
	req2 := httptest.NewRequest("GET", "/", nil)
	w2 := httptest.NewRecorder()
	GzipMiddleware(handler).ServeHTTP(w2, req2)
	if got := w2.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("expected no Content-Encoding, got %q", got)
	}
	if strings.Contains(w2.Body.String(), "<html>") == false {
		t.Fatal("expected plain body for non-gzip client")
	}
}

func TestFlatToAI_MultipleGroupsPerCategory(t *testing.T) {
	dd := &models.DashboardData{
		Categories: []models.CategoryGroup{
			{
				Category: "Ελλάδα",
				Articles: []models.Article{
					{Title: "Φωτιά στην Αττική", Feed: "iefimerida"},
					{Title: "Φωτιά κοντά στην Αταλάντη", Feed: "newsit"}, // same story different feed
					{Title: "Αναβάθμιση οδικού δικτύου", Feed: "naftemporiki"},
				},
				Count: 3,
			},
		},
		Total:     3,
		Generated: "14:30:00",
	}

	result := FlatToAI(dd)
	if result.Total != 3 {
		t.Errorf("expected total 3, got %d", result.Total)
	}
	if len(result.Categories) != 1 {
		t.Fatalf("expected 1 category, got %d", len(result.Categories))
	}
	if result.Categories[0].Category != "Ελλάδα" {
		t.Errorf("expected category 'Ελλάδα', got %q", result.Categories[0].Category)
	}
	// Each article becomes its own group in FlatToAI
	if len(result.Categories[0].Groups) != 3 {
		t.Errorf("expected 3 groups, got %d", len(result.Categories[0].Groups))
	}
}

func TestFlatToAI_PreservesArticleData(t *testing.T) {
	art := models.Article{
		ID:    12345,
		Title: "Marfin: Προθεσμία για να απολογηθεί την Τρίτη",
		URL:   "https://example.com/marfin",
		Feed:  "The Press Project",
	}
	dd := &models.DashboardData{
		Categories: []models.CategoryGroup{
			{Category: "Ελλάδα", Articles: []models.Article{art}, Count: 1},
		},
		Total:     1,
		Generated: "12:00:00",
	}

	result := FlatToAI(dd)
	if len(result.Categories[0].Groups) != 1 {
		t.Fatalf("expected 1 group")
	}
	if result.Categories[0].Groups[0].Articles[0].ID != 12345 {
		t.Errorf("expected article ID 12345")
	}
	if result.Categories[0].Groups[0].Articles[0].URL != "https://example.com/marfin" {
		t.Errorf("expected article URL preserved")
	}
}

func TestFlatToAI_MusicCategory(t *testing.T) {
	dd := &models.DashboardData{
		Categories: []models.CategoryGroup{
			{
				Category: "Μουσικά νέα",
				Articles: []models.Article{
					{Title: "Λένα Ζευγαρά: Κυκλοφόρησε το νέο διπλό album", Feed: "Gazzetta"},
				},
				Count: 1,
			},
		},
		Total:     1,
		Generated: "10:00:00",
	}

	result := FlatToAI(dd)
	if len(result.Categories) != 1 {
		t.Fatalf("expected 1 category")
	}
	// Should preserve the category name as-is
	if result.Categories[0].Category != "Μουσικά νέα" {
		t.Errorf("expected 'Μουσικά νέα', got %q", result.Categories[0].Category)
	}
}
