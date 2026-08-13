package handler

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/panospet/daily-news-aggregator/internal/cache"
	"github.com/panospet/daily-news-aggregator/internal/config"
	"github.com/panospet/daily-news-aggregator/internal/database"
	"github.com/panospet/daily-news-aggregator/internal/models"
	"github.com/panospet/daily-news-aggregator/internal/summarize"
)

// Server holds all dependencies for HTTP handlers.
type Server struct {
	Cfg         config.Config
	DB          *sql.DB
	Tpl        *TemplateSet
	Cache      *cache.ArticleCache
	AICache    *cache.AICache
	Summarizer *summarize.Service
	FetchFn    func(db *sql.DB) (*models.AIDashboardData, error)
	RetryFn    func(hours int) (*models.DashboardData, error)
}

// GetDataWithFallback fetches AI data, falling back to flat data if AI cache is empty or refreshing.
func (s *Server) GetDataWithFallback() (*models.AIDashboardData, error) {
	aiData, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		log.Printf("AI data error (falling back to flat): %v", err)
		flat, flatErr := s.Cache.Get(nil, s.RetryFn)
		if flatErr != nil {
			return nil, flatErr
		}
		aiData = FlatToAI(flat)
	}
	return aiData, nil
}

// RegisterRoutes registers all HTTP routes on the given mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.Home)
	mux.HandleFunc("GET /filter", s.CategoryFilter)
	mux.HandleFunc("GET /search", s.Search)
	mux.HandleFunc("GET /stats", s.Stats)
	mux.HandleFunc("POST /summarize", s.HandleSummarize)
	mux.HandleFunc("POST /feedback", s.HandleFeedback)
	mux.HandleFunc("POST /feedback/positive", s.HandlePositiveFeedback)
}

// Home serves the main dashboard page.
func (s *Server) Home(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "no-cache")

	aiData, err := s.GetDataWithFallback()
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Push-Url", "/")
		_ = s.Tpl.Partial.ExecuteTemplate(w, "articles", aiData)
		return
	}

	_ = s.Tpl.Base.ExecuteTemplate(w, "base", aiData)
}

// CategoryFilter serves filtered articles by category.
func (s *Server) CategoryFilter(w http.ResponseWriter, r *http.Request) {
	cat := r.URL.Query().Get("cat")
	data, err := s.GetDataWithFallback()
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "no-cache")

	var filtered models.AIDashboardData
	if cat == "" || cat == "all" {
		filtered = *data
	} else {
		for _, g := range data.Categories {
			if strings.EqualFold(g.Category, cat) {
				filtered.Categories = append(filtered.Categories, g)
				filtered.Total = g.Count
			}
		}
	}
	filtered.Generated = data.Generated
	filtered.IsFallback = data.IsFallback
	filtered.TopTags = data.TopTags

	// HTMX request: return only the articles partial
	if r.Header.Get("HX-Request") == "true" {
		_ = s.Tpl.Partial.ExecuteTemplate(w, "articles", filtered)
		return
	}
	// Full page load: render base template with filtered data
	_ = s.Tpl.Base.ExecuteTemplate(w, "base", filtered)
}

// Search handles article search queries.
func (s *Server) Search(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	data, err := s.GetDataWithFallback()
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")

	if q == "" {
		if r.Header.Get("HX-Request") == "true" {
			_ = s.Tpl.Partial.ExecuteTemplate(w, "articles", data)
			return
		}
		_ = s.Tpl.Base.ExecuteTemplate(w, "base", data)
		return
	}

	var filtered models.AIDashboardData
	filtered.Generated = data.Generated
	filtered.TopTags = data.TopTags
	for _, g := range data.Categories {
		var matchedGroups []models.AIGroup
		for _, grp := range g.Groups {
			tagMatched := false
			for _, t := range grp.Tags {
				if strings.Contains(strings.ToLower("#"+t), q) || strings.Contains(strings.ToLower(t), q) {
					tagMatched = true
					break
				}
			}

			var matched []models.Article
			for _, a := range grp.Articles {
				if tagMatched || strings.Contains(strings.ToLower(a.Title), q) ||
					strings.Contains(strings.ToLower(a.Feed), q) {
					matched = append(matched, a)
				}
			}
			if len(matched) > 0 {
				matchedGroups = append(matchedGroups, models.AIGroup{Topic: grp.Topic, Articles: matched, Count: len(matched)})
				filtered.Total += len(matched)
			}
		}
		if len(matchedGroups) > 0 {
			filtered.Categories = append(filtered.Categories, models.AICategoryGroup{Category: g.Category, Groups: matchedGroups, Count: len(matchedGroups)})
		}
	}

	// HTMX request: return only matched articles partial
	if r.Header.Get("HX-Request") == "true" {
		_ = s.Tpl.Partial.ExecuteTemplate(w, "articles", filtered)
		return
	}
	// Full page load: render base template with matched data
	_ = s.Tpl.Base.ExecuteTemplate(w, "base", filtered)
}

// Stats serves the stats partial.
func (s *Server) Stats(w http.ResponseWriter, r *http.Request) {
	data, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	_ = s.Tpl.Partial.ExecuteTemplate(w, "stats-old", models.DashboardData{Total: data.Total, Generated: data.Generated})
}

// HandleSummarize generates AI summaries for topic groups.
func (s *Server) HandleSummarize(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cat := r.FormValue("category")
	groupID := r.FormValue("group_id")
	groupIdxStr := r.FormValue("group")

	groupIdx, err := strconv.Atoi(groupIdxStr)
	if err != nil {
		groupIdx = -1
	}

	data, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}

	var targetGroup models.AIGroup
	var targetTopic string
	found := false

	// Prefer lookup by stable group_id (prevents index-mismatch on cache refresh)
	for _, cg := range data.Categories {
		if !strings.EqualFold(cg.Category, cat) {
			continue
		}
		if groupID != "" {
			for _, g := range cg.Groups {
				if g.GroupID == groupID {
					targetGroup = g
					targetTopic = g.Topic
					found = true
					break
				}
			}
		}
		// Fallback to index lookup ONLY if no group_id was provided (legacy clients)
		if !found && groupID == "" && groupIdx >= 0 && groupIdx < len(cg.Groups) {
			targetGroup = cg.Groups[groupIdx]
			targetTopic = cg.Groups[groupIdx].Topic
			found = true
		}
		if found {
			break
		}
	}

	if !found {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<div class="summary-refresh">⚠️ Τα δεδομένα άλλαξαν. Κάνε <a href="/" hx-get="/" hx-target="body">ανανέωση</a> και δοκίμασε ξανά.</div>`))
		return
	}

	// No AI key — show fallback directly
	if s.Cfg.GeminiAPIKey == "" {
		targetGroup.Category = cat
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.Tpl.Partial.ExecuteTemplate(w, "summary-fallback", targetGroup)
		return
	}

	summary, err := s.Summarizer.Summarize(targetTopic, targetGroup.Articles)
	if err != nil {
		log.Printf("Summarize error: %v", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.Tpl.Partial.ExecuteTemplate(w, "summary-fallback", targetGroup)
		return
	}

	// Update cache data so the newly generated fancy topic persists
	if len(summary.Sources) > 0 && summary.Sources[0] != "" {
		fancyTopic := summary.Sources[0]
		targetTopic = fancyTopic

		// Write-update the cached topic name in Redis
		s.AICache.UpdateTopic(cat, groupIdx, fancyTopic)
	}

	formattedSummary := strings.ReplaceAll(summary.Summary, "\n", "<br>")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.Tpl.Partial.ExecuteTemplate(w, "summary", map[string]interface{}{
		"Topic":    targetTopic,
		"Summary":  template.HTML(formattedSummary),
		"Articles": targetGroup.Articles,
		"Category": cat,
		"GroupID":  targetGroup.GroupID,
		"Tags":     targetGroup.Tags,
	})
}

// HandleFeedback stores negative feedback for an irrelevant article
func (s *Server) HandleFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cat := r.FormValue("category")
	groupID := r.FormValue("group_id")
	irrelevantUrl := r.FormValue("irrelevant_url")

	if cat == "" || irrelevantUrl == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		return
	}

	data, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}

	var targetGroup models.AIGroup
	var targetTopic string
	found := false

	// Find the exact group being reported
	for _, cg := range data.Categories {
		if !strings.EqualFold(cg.Category, cat) {
			continue
		}
		if groupID != "" {
			for _, g := range cg.Groups {
				if g.GroupID == groupID {
					targetGroup = g
					targetTopic = g.Topic
					found = true
					break
				}
			}
		}
		if found {
			break
		}
	}

	if !found {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<span class="feedback-success">Feedback received ✅</span>`)) // Pretend success if group vanished
		return
	}

	groupJsonBytes, err := json.Marshal(targetGroup)
	if err != nil {
		log.Printf("Failed to marshal group for feedback: %v", err)
	} else {
		repo := &database.Repository{DB: s.DB}
		if err := repo.SaveNegativeFeedback(cat, targetTopic, groupJsonBytes, irrelevantUrl); err != nil {
			log.Printf("Failed to save negative feedback: %v", err)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<span class="feedback-success">Feedback received ✅</span>`))
}

// HandlePositiveFeedback stores positive feedback for a topic group
func (s *Server) HandlePositiveFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cat := r.FormValue("category")
	groupID := r.FormValue("group_id")

	if cat == "" || groupID == "" {
		http.Error(w, "Missing required parameters", http.StatusBadRequest)
		return
	}

	data, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}

	var targetGroup models.AIGroup
	var targetTopic string
	found := false

	for _, cg := range data.Categories {
		if !strings.EqualFold(cg.Category, cat) {
			continue
		}
		if groupID != "" {
			for _, g := range cg.Groups {
				if g.GroupID == groupID {
					targetGroup = g
					targetTopic = g.Topic
					found = true
					break
				}
			}
		}
		if found {
			break
		}
	}

	if !found {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<span class="feedback-success">Feedback received ✅</span>`))
		return
	}

	groupJsonBytes, err := json.Marshal(targetGroup)
	if err != nil {
		log.Printf("Failed to marshal group for positive feedback: %v", err)
	} else {
		repo := &database.Repository{DB: s.DB}
		if err := repo.SavePositiveFeedback(cat, targetTopic, groupJsonBytes); err != nil {
			log.Printf("Failed to save positive feedback: %v", err)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<span class="feedback-success">Feedback received ✅</span>`))
}
