package grouping

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"sort"

	"github.com/panospet/daily-news-aggregator/internal/models"
)

// GroupBy partitions a slice by a key function.
func GroupBy[T any](items []T, keyFn func(T) string) map[string][]T {
	m := make(map[string][]T)
	for _, item := range items {
		k := keyFn(item)
		m[k] = append(m[k], item)
	}
	return m
}

// CountArticles sums the Count field across a slice of AIGroup.
func CountArticles(groups []models.AIGroup) int {
	n := 0
	for _, g := range groups {
		n += g.Count
	}
	return n
}

// CountCatGroups counts total groups across all categories.
func CountCatGroups(cats []models.AICategoryGroup) int {
	n := 0
	for _, c := range cats {
		n += len(c.Groups)
	}
	return n
}

// BuildAICatGroupFlat creates an AICategoryGroup where each article is its own topic group.
func BuildAICatGroupFlat(cat string, arts []models.Article, displayName string) models.AICategoryGroup {
	acg := models.AICategoryGroup{Category: displayName, Count: len(arts)}
	for _, a := range arts {
		acg.Groups = append(acg.Groups, models.AIGroup{Topic: a.Title, Articles: []models.Article{a}, Count: 1})
	}
	return acg
}

// BuildAICatGroupFromParsed builds an AICategoryGroup from parsed Gemini grouping results.
func BuildAICatGroupFromParsed(cat string, arts []models.Article, displayName string, groups []struct {
	Topic          string `json:"topic"`
	ArticleIndices []int  `json:"article_indices"`
	Sentence       string `json:"sentence"`
}) models.AICategoryGroup {
	acg := models.AICategoryGroup{Category: displayName, Count: len(arts)}
	used := make(map[int]bool)
	if len(groups) > 0 {
		for _, g := range groups {
			var grouped []models.Article
			for _, idx := range g.ArticleIndices {
				if idx >= 0 && idx < len(arts) && !used[idx] {
					grouped = append(grouped, arts[idx])
					used[idx] = true
				}
			}
			if len(grouped) > 0 {
				acg.Groups = append(acg.Groups, models.AIGroup{Topic: g.Topic, Articles: grouped, Count: len(grouped)})
			}
		}
	}
	// Add remaining (ungrouped) articles as singletons
	for i, a := range arts {
		if !used[i] {
			acg.Groups = append(acg.Groups, models.AIGroup{Topic: a.Title, Articles: []models.Article{a}, Count: 1})
		}
	}
	return acg
}

// FlatGroupResult creates a flat (non-AI) grouping respecting preferred order.
func FlatGroupResult(catMap map[string][]models.Article, preferredOrder []string) []models.AICategoryGroup {
	var result []models.AICategoryGroup
	for _, cat := range preferredOrder {
		if arts, ok := catMap[cat]; ok {
			acg := models.AICategoryGroup{Category: cat, Count: len(arts)}
			for _, a := range arts {
				acg.Groups = append(acg.Groups, models.AIGroup{Topic: a.Title, Articles: []models.Article{a}, Count: 1})
			}
			result = append(result, acg)
			delete(catMap, cat)
		}
	}
	for cat, arts := range catMap {
		acg := models.AICategoryGroup{Category: cat, Count: len(arts)}
		for _, a := range arts {
			acg.Groups = append(acg.Groups, models.AIGroup{Topic: a.Title, Articles: []models.Article{a}, Count: 1})
		}
		result = append(result, acg)
	}
	return result
}

// AssignGroupIDs computes stable, content-based IDs for each group.
// Uses SHA-256 of sorted article IDs so the same group of articles
// always gets the same ID regardless of clustering order.
func AssignGroupIDs(categories []models.AICategoryGroup) {
	totalGroups := 0
	for ci := range categories {
		for gi := range categories[ci].Groups {
			g := &categories[ci].Groups[gi]
			if len(g.Articles) == 0 {
				continue
			}
			ids := make([]int64, len(g.Articles))
			for i, a := range g.Articles {
				ids[i] = a.ID
			}
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			h := sha256.New()
			for _, id := range ids {
				h.Write([]byte(fmt.Sprintf("%d,", id)))
			}
			g.GroupID = hex.EncodeToString(h.Sum(nil))[:12]
			totalGroups++
		}
	}
	log.Printf("[groupID] Assigned IDs to %d groups", totalGroups)
}
