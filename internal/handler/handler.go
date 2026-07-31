package handler

import (
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/panospet/htmx-daily-news/internal/cache"
	"github.com/panospet/htmx-daily-news/internal/config"
	"github.com/panospet/htmx-daily-news/internal/models"
	"github.com/panospet/htmx-daily-news/internal/summarize"
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

// RegisterRoutes registers all HTTP routes on the given mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.Home)
	mux.HandleFunc("GET /filter", s.CategoryFilter)
	mux.HandleFunc("GET /search", s.Search)
	mux.HandleFunc("GET /stats", s.Stats)
	mux.HandleFunc("POST /summarize", s.HandleSummarize)
}

// Home serves the main dashboard page.
func (s *Server) Home(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "no-cache")

	aiData, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		log.Printf("AI data error (falling back to flat): %v", err)
		flat, flatErr := s.Cache.Get(nil, s.RetryFn)
		if flatErr != nil {
			http.Error(w, "Data error", http.StatusInternalServerError)
			return
		}
		aiData = FlatToAI(flat)
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
	data, err := s.AICache.Get(s.DB, s.FetchFn)
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
	data, err := s.AICache.Get(s.DB, s.FetchFn)
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
	for _, g := range data.Categories {
		var matchedGroups []models.AIGroup
		for _, grp := range g.Groups {
			var matched []models.Article
			for _, a := range grp.Articles {
				if strings.Contains(strings.ToLower(a.Title), q) ||
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
		} else {
			filtered.Categories = append(filtered.Categories, models.AICategoryGroup{Category: g.Category, Count: 0})
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

		// Write-update the cached topic name
		s.AICache.Mu.Lock()
		for catIdx, cg := range data.Categories {
			if strings.EqualFold(cg.Category, cat) {
				if groupIdx >= 0 && groupIdx < len(cg.Groups) {
					data.Categories[catIdx].Groups[groupIdx].Topic = fancyTopic
				}
			}
		}
		s.AICache.Mu.Unlock()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.Tpl.Partial.ExecuteTemplate(w, "summary", map[string]interface{}{
		"Topic":    targetTopic,
		"Summary":  summary.Summary,
		"Articles": targetGroup.Articles,
		"Category": cat,
	})
}
