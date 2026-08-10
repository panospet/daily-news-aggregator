package grouping

import (
	"testing"

	"github.com/panospet/daily-news-aggregator/internal/models"
)

func TestCountArticles(t *testing.T) {
	groups := []models.AIGroup{
		{Count: 3},
		{Count: 5},
		{Count: 1},
	}
	got := CountArticles(groups)
	if got != 9 {
		t.Errorf("expected 9, got %d", got)
	}
}

func TestCountArticles_Empty(t *testing.T) {
	got := CountArticles(nil)
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestCountCatGroups(t *testing.T) {
	cats := []models.AICategoryGroup{
		{Groups: make([]models.AIGroup, 3)},
		{Groups: make([]models.AIGroup, 2)},
	}
	got := CountCatGroups(cats)
	if got != 5 {
		t.Errorf("expected 5, got %d", got)
	}
}

func TestGroupBy(t *testing.T) {
	items := []models.Article{
		{Category: "news", Title: "A"},
		{Category: "sports", Title: "B"},
		{Category: "news", Title: "C"},
	}
	result := GroupBy(items, func(a models.Article) string { return a.Category })
	if len(result["news"]) != 2 {
		t.Errorf("expected 2 news articles, got %d", len(result["news"]))
	}
	if len(result["sports"]) != 1 {
		t.Errorf("expected 1 sports article, got %d", len(result["sports"]))
	}
}

func TestAssignGroupIDs_Deterministic(t *testing.T) {
	cats := []models.AICategoryGroup{
		{
			Category: "test",
			Groups: []models.AIGroup{
				{
					Articles: []models.Article{
						{ID: 100},
						{ID: 200},
						{ID: 50},
					},
				},
			},
		},
	}

	AssignGroupIDs(cats)
	firstID := cats[0].Groups[0].GroupID
	if firstID == "" {
		t.Error("expected non-empty GroupID")
	}

	// Run again — should produce the same ID
	AssignGroupIDs(cats)
	secondID := cats[0].Groups[0].GroupID
	if firstID != secondID {
		t.Errorf("expected deterministic IDs: %q != %q", firstID, secondID)
	}
}

func TestAssignGroupIDs_DifferentOrder_SameID(t *testing.T) {
	cats1 := []models.AICategoryGroup{
		{Groups: []models.AIGroup{{Articles: []models.Article{{ID: 1}, {ID: 3}, {ID: 2}}}}},
	}
	cats2 := []models.AICategoryGroup{
		{Groups: []models.AIGroup{{Articles: []models.Article{{ID: 3}, {ID: 1}, {ID: 2}}}}},
	}

	AssignGroupIDs(cats1)
	AssignGroupIDs(cats2)

	if cats1[0].Groups[0].GroupID != cats2[0].Groups[0].GroupID {
		t.Error("same articles in different order should produce same GroupID")
	}
}

func TestFlatGroupResult(t *testing.T) {
	catMap := map[string][]models.Article{
		"news":   {{Title: "A"}, {Title: "B"}},
		"sports": {{Title: "C"}},
	}
	displayNames := map[string]string{
		"news":   "ειδήσεις Ελλάδα",
		"sports": "αθλητικές ειδήσεις",
	}
	preferredOrder := []string{"sports", "news"}

	result := FlatGroupResult(catMap, preferredOrder, displayNames)
	if len(result) != 2 {
		t.Fatalf("expected 2 categories, got %d", len(result))
	}
	// First should be sports (preferred order)
	if result[0].Category != "αθλητικές ειδήσεις" {
		t.Errorf("expected sports first, got %q", result[0].Category)
	}
	if result[1].Category != "ειδήσεις Ελλάδα" {
		t.Errorf("expected news second, got %q", result[1].Category)
	}
}

func TestBuildAICatGroupFlat(t *testing.T) {
	arts := []models.Article{
		{Title: "Article 1"},
		{Title: "Article 2"},
	}
	result := BuildAICatGroupFlat("news", arts, "ειδήσεις Ελλάδα")
	if result.Category != "ειδήσεις Ελλάδα" {
		t.Errorf("expected display name, got %q", result.Category)
	}
	if result.Count != 2 {
		t.Errorf("expected count 2, got %d", result.Count)
	}
	if len(result.Groups) != 2 {
		t.Errorf("expected 2 groups (one per article), got %d", len(result.Groups))
	}
}
