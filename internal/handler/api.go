package handler

import (
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/models"
)

// --- Lean response types for the read-only JSON API. ---
//
// These deliberately omit `content` (RSS teasers are frequently stubs) and any
// template-only fields. Every field is serialized; there is no HTML anywhere.

type apiArticle struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	URL       string    `json:"url"`
	Published time.Time `json:"published_at"`
	Feed      string    `json:"feed"`
	Category  string    `json:"category"`
}

type apiGroup struct {
	GroupID  string       `json:"group_id"`
	Topic    string       `json:"topic"`
	Category string       `json:"category"`
	Count    int          `json:"count"`
	Tags     []string     `json:"tags,omitempty"`
	Articles []apiArticle `json:"articles"`
}

type apiCategory struct {
	Category   string `json:"category"`
	Count      int    `json:"count"`
	GroupCount int    `json:"group_count"`
}

func toAPIArticle(a models.Article) apiArticle {
	return apiArticle{
		ID:        a.ID,
		Title:     a.Title,
		URL:       a.URL,
		Published: a.Published,
		Feed:      a.Feed,
		Category:  a.Category,
	}
}

// toAPIGroup converts a group plus its enclosing display category into the lean
// API shape. The category is passed explicitly because models.AIGroup.Category
// is always empty in cache data — the real category is the enclosing one.
func toAPIGroup(category string, g models.AIGroup) apiGroup {
	arts := make([]apiArticle, 0, len(g.Articles))
	for _, a := range g.Articles {
		arts = append(arts, toAPIArticle(a))
	}
	return apiGroup{
		GroupID:  g.GroupID,
		Topic:    g.Topic,
		Category: category,
		Count:    g.Count,
		Tags:     g.Tags,
		Articles: arts,
	}
}

// RegisterAPIRoutes registers the read-only JSON API routes on the given mux.
func (s *Server) RegisterAPIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/feeds", s.APIFeeds)
	mux.HandleFunc("GET /api/v1/feeds/{id}", s.APIFeed)
	mux.HandleFunc("GET /api/v1/feeds/{id}/articles", s.APIFeedArticles)
	mux.HandleFunc("GET /api/v1/articles", s.APIArticles)
	mux.HandleFunc("GET /api/v1/categories", s.APICategories)
	mux.HandleFunc("GET /api/v1/categories/{cat}/groups", s.APICategoryGroups)
	mux.HandleFunc("GET /api/v1/groups", s.APIGroups)
	mux.HandleFunc("GET /api/v1/groups/{id}", s.APIGroup)
}

// parsePagination reads limit/offset with sane bounds (default 50, max 200).
func parsePagination(r *http.Request) (limit, offset int) {
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// parseSince reads the `since` query param as either a Go duration ("24h",
// "6h30m") relative to now, or an RFC3339 timestamp. Zero value = no floor.
func parseSince(r *http.Request) time.Time {
	s := strings.TrimSpace(r.URL.Query().Get("since"))
	if s == "" {
		return time.Time{}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// APIFeeds lists every Miniflux feed with article counts.
func (s *Server) APIFeeds(w http.ResponseWriter, r *http.Request) {
	if s.Repo == nil {
		writeBriefingJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database unavailable"})
		return
	}
	feeds, err := s.Repo.ListFeeds()
	if err != nil {
		log.Printf("[api] ListFeeds failed: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list feeds"})
		return
	}
	writeBriefingJSON(w, http.StatusOK, map[string]interface{}{"total": len(feeds), "feeds": feeds})
}

// APIFeed returns a single feed's metadata.
func (s *Server) APIFeed(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || s.Repo == nil {
		writeBriefingJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid feed id"})
		return
	}
	feeds, err := s.Repo.ListFeeds()
	if err != nil {
		log.Printf("[api] ListFeeds failed: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to list feeds"})
		return
	}
	for _, f := range feeds {
		if f.ID == id {
			writeBriefingJSON(w, http.StatusOK, f)
			return
		}
	}
	writeBriefingJSON(w, http.StatusNotFound, map[string]string{"error": "feed not found"})
}

// APIFeedArticles returns the articles for one feed, newest first.
func (s *Server) APIFeedArticles(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeBriefingJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid feed id"})
		return
	}
	if s.Repo == nil {
		writeBriefingJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database unavailable"})
		return
	}
	limit, offset := parsePagination(r)
	since := parseSince(r)
	articles, err := s.Repo.QueryArticlesFlat(id, "", since, "", limit, offset)
	if err != nil {
		log.Printf("[api] QueryArticlesFlat failed: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query articles"})
		return
	}
	total, err := s.Repo.CountArticlesFlat(id, "", since, "")
	if err != nil {
		log.Printf("[api] CountArticlesFlat failed: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query articles"})
		return
	}
	writeBriefingJSON(w, http.StatusOK, map[string]interface{}{
		"feed_id":  id,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"articles": articlesToAPI(articles),
	})
}

// APIArticles returns a flat, filtered article list from the Miniflux DB.
func (s *Server) APIArticles(w http.ResponseWriter, r *http.Request) {
	if s.Repo == nil {
		writeBriefingJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database unavailable"})
		return
	}
	q := r.URL.Query()
	feedID, _ := strconv.ParseInt(q.Get("feed_id"), 10, 64)
	category := strings.TrimSpace(q.Get("category"))
	since := parseSince(r)
	query := stripAccents(strings.ToLower(strings.TrimSpace(q.Get("q"))))
	limit, offset := parsePagination(r)

	articles, err := s.Repo.QueryArticlesFlat(feedID, category, since, query, limit, offset)
	if err != nil {
		log.Printf("[api] QueryArticlesFlat failed: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query articles"})
		return
	}
	total, err := s.Repo.CountArticlesFlat(feedID, category, since, query)
	if err != nil {
		log.Printf("[api] CountArticlesFlat failed: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to query articles"})
		return
	}
	writeBriefingJSON(w, http.StatusOK, map[string]interface{}{
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"articles": articlesToAPI(articles),
	})
}

// APICategories lists the AI display categories with group and article counts.
func (s *Server) APICategories(w http.ResponseWriter, r *http.Request) {
	data, err := s.GetDataWithFallback()
	if err != nil {
		log.Printf("[api] categories data error: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "data error"})
		return
	}
	out := make([]apiCategory, 0, len(data.Categories))
	for _, cg := range data.Categories {
		out = append(out, apiCategory{Category: cg.Category, Count: cg.Count, GroupCount: len(cg.Groups)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Category < out[j].Category })
	writeBriefingJSON(w, http.StatusOK, map[string]interface{}{
		"generated":  data.Generated,
		"total":      len(out),
		"categories": out,
	})
}

// APICategoryGroups returns the topic groups under one AI display category,
// sorted by group size, paginated.
func (s *Server) APICategoryGroups(w http.ResponseWriter, r *http.Request) {
	cat := r.PathValue("cat")
	data, err := s.GetDataWithFallback()
	if err != nil {
		log.Printf("[api] category groups data error: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "data error"})
		return
	}
	for _, cg := range data.Categories {
		if !strings.EqualFold(cg.Category, cat) {
			continue
		}
		groups := make([]models.AIGroup, len(cg.Groups))
		copy(groups, cg.Groups)
		sort.SliceStable(groups, func(i, j int) bool { return groups[i].Count > groups[j].Count })

		limit, offset := parsePagination(r)
		start, end := offset, offset+limit
		if start > len(groups) {
			start = len(groups)
		}
		if end > len(groups) {
			end = len(groups)
		}
		page := groups[start:end]

		out := make([]apiGroup, 0, len(page))
		for _, g := range page {
			out = append(out, toAPIGroup(cg.Category, g))
		}
		writeBriefingJSON(w, http.StatusOK, map[string]interface{}{
			"category": cg.Category,
			"total":    len(groups),
			"limit":    limit,
			"offset":   offset,
			"groups":   out,
		})
		return
	}
	writeBriefingJSON(w, http.StatusNotFound, map[string]string{"error": "category not found"})
}

// APIGroups returns topic groups flattened across categories, deduplicated by
// group_id, optionally filtered to one category, sorted by group size and
// paginated.
func (s *Server) APIGroups(w http.ResponseWriter, r *http.Request) {
	cat := strings.TrimSpace(r.URL.Query().Get("cat"))
	data, err := s.GetDataWithFallback()
	if err != nil {
		log.Printf("[api] groups data error: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "data error"})
		return
	}

	type scored struct {
		category string
		group    models.AIGroup
	}
	var all []scored
	seen := make(map[string]bool)
	for _, cg := range data.Categories {
		if cat != "" && !strings.EqualFold(cg.Category, cat) {
			continue
		}
		for _, g := range cg.Groups {
			// Multi-tagged groups appear once per category; render each once.
			if g.GroupID != "" && seen[g.GroupID] {
				continue
			}
			seen[g.GroupID] = true
			all = append(all, scored{category: cg.Category, group: g})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].group.Count != all[j].group.Count {
			return all[i].group.Count > all[j].group.Count
		}
		return all[i].group.Topic < all[j].group.Topic
	})

	limit, offset := parsePagination(r)
	start, end := offset, offset+limit
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	page := all[start:end]

	out := make([]apiGroup, 0, len(page))
	for _, sc := range page {
		out = append(out, toAPIGroup(sc.category, sc.group))
	}
	writeBriefingJSON(w, http.StatusOK, map[string]interface{}{
		"generated": data.Generated,
		"total":     len(all),
		"limit":     limit,
		"offset":    offset,
		"groups":    out,
	})
}

// APIGroup returns a single topic group (by group_id) with its articles.
func (s *Server) APIGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	data, err := s.GetDataWithFallback()
	if err != nil {
		log.Printf("[api] group data error: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "data error"})
		return
	}
	for _, cg := range data.Categories {
		for _, g := range cg.Groups {
			if g.GroupID == groupID {
				writeBriefingJSON(w, http.StatusOK, toAPIGroup(cg.Category, g))
				return
			}
		}
	}
	writeBriefingJSON(w, http.StatusNotFound, map[string]string{"error": "group not found"})
}

func articlesToAPI(articles []models.Article) []apiArticle {
	out := make([]apiArticle, 0, len(articles))
	for _, a := range articles {
		out = append(out, toAPIArticle(a))
	}
	return out
}
