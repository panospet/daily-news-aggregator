package handler

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
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
	Cfg        config.Config
	DB         *sql.DB
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
	mux.HandleFunc("GET /cards", s.Cards)
	mux.HandleFunc("GET /card-body", s.CardBody)
	mux.HandleFunc("POST /summarize", s.HandleSummarize)
	mux.HandleFunc("POST /feedback", s.HandleFeedback)
	mux.HandleFunc("POST /feedback/positive", s.HandlePositiveFeedback)
}

// Cards serves paginated cards for infinite scroll, either per category
// (cat=) or across categories for hashtag views (tag=).
func (s *Server) Cards(w http.ResponseWriter, r *http.Request) {
	cat := r.URL.Query().Get("cat")
	tag := strings.ToLower(r.URL.Query().Get("tag"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	data, err := s.GetDataWithFallback()
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")

	type item struct {
		Category string
		Group    models.AIGroup
	}
	var items []item
	limit := 10
	if tag != "" {
		limit = 40
		for _, cg := range data.Categories {
			for _, g := range cg.Groups {
				for _, t := range g.Tags {
					if strings.Contains(strings.ToLower(t), tag) {
						items = append(items, item{Category: cg.Category, Group: g})
						break
					}
				}
			}
		}
	} else {
		for _, cg := range data.Categories {
			if cat == "" || cat == "all" || strings.EqualFold(cg.Category, cat) {
				for _, g := range cg.Groups {
					items = append(items, item{Category: cg.Category, Group: g})
				}
			}
		}
	}

	end := offset + limit
	if end > len(items) {
		end = len(items)
	}
	batch := items[offset:end]

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, it := range batch {
		_ = s.Tpl.Partial.ExecuteTemplate(w, "card", it)
	}
	if end < len(items) {
		var nextURL string
		if tag != "" {
			nextURL = "/cards?tag=" + url.QueryEscape(tag) + "&offset=" + strconv.Itoa(end)
		} else {
			nextURL = "/cards?cat=" + url.QueryEscape(cat) + "&offset=" + strconv.Itoa(end)
		}
		_ = s.Tpl.Partial.ExecuteTemplate(w, "cards-sentinel", map[string]interface{}{"url": nextURL})
	}
	if offset == 0 && len(items) == 0 {
		_ = s.Tpl.Partial.ExecuteTemplate(w, "cards-empty", nil)
	}
}

// CardBody serves the expandable body (tags, summarize button, sources)
// for one group, fetched lazily when a card is first opened.
func (s *Server) CardBody(w http.ResponseWriter, r *http.Request) {
	groupID := r.URL.Query().Get("gid")
	cat := r.URL.Query().Get("cat")
	if groupID == "" {
		http.Error(w, "Missing gid", http.StatusBadRequest)
		return
	}

	data, err := s.GetDataWithFallback()
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")

	for _, cg := range data.Categories {
		if cat != "" && !strings.EqualFold(cg.Category, cat) {
			continue
		}
		for _, g := range cg.Groups {
			if g.GroupID == groupID {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_ = s.Tpl.Partial.ExecuteTemplate(w, "card-body", map[string]interface{}{
					"Category": cg.Category,
					"Group":    g,
				})
				return
			}
		}
	}
	http.Error(w, "Not found", http.StatusNotFound)
}

// Home serves the main dashboard page: a flat grid of the most
// important groups (most sources) across all categories.
func (s *Server) Home(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "no-cache")

	aiData, err := s.GetDataWithFallback()
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}

	view := topView(*aiData)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Push-Url", "/")
		_ = s.Tpl.Partial.ExecuteTemplate(w, "articles", view)
		return
	}

	_ = s.Tpl.Base.ExecuteTemplate(w, "base", view)
}

// topView builds the flat "Top Stories" homepage view: the top N groups
// ranked by number of distinct source feeds, tie-broken by article count.
func topView(data models.AIDashboardData) models.AIDashboardData {
	const topN = 40
	type scored struct {
		cat      string
		group    models.AIGroup
		feeds    int
		articles int
	}
	var all []scored
	for _, cg := range data.Categories {
		for _, g := range cg.Groups {
			feeds := make(map[string]bool)
			for _, a := range g.Articles {
				feeds[a.Feed] = true
			}
			all = append(all, scored{cat: cg.Category, group: g, feeds: len(feeds), articles: len(g.Articles)})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].feeds != all[j].feeds {
			return all[i].feeds > all[j].feeds
		}
		return all[i].articles > all[j].articles
	})
	if len(all) > topN {
		all = all[:topN]
	}
	view := models.AIDashboardData{
		Total:      len(all),
		Generated:  data.Generated,
		IsFallback: data.IsFallback,
		TopTags:    data.TopTags,
		Categories: data.Categories,
		View:       "top",
	}
	for _, s := range all {
		view.TopGroups = append(view.TopGroups, models.TopGroup{Category: s.cat, Group: s.group})
	}
	return view
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
	filtered.View = "category"

	// HTMX request: return only the articles partial
	if r.Header.Get("HX-Request") == "true" {
		_ = s.Tpl.Partial.ExecuteTemplate(w, "articles", filtered)
		return
	}
	// Full page load: render base template with filtered data
	_ = s.Tpl.Base.ExecuteTemplate(w, "base", filtered)
}

// tagView reuses search matching for hashtag clicks: matched groups keep
// their original category so cards carry the right chip.
func tagView(data models.AIDashboardData, q string) models.AIDashboardData {
	var filtered models.AIDashboardData
	filtered.Generated = data.Generated
	filtered.TopTags = data.TopTags
	filtered.Categories = data.Categories // sidebar nav; stream renders TopGroups in this view
	filtered.View = "tags"
	for _, g := range data.Categories {
		for _, grp := range g.Groups {
			tagMatched := false
			for _, t := range grp.Tags {
				if strings.Contains(strings.ToLower(t), q) {
					tagMatched = true
					break
				}
			}
			if tagMatched {
				filtered.TopGroups = append(filtered.TopGroups, models.TopGroup{Category: g.Category, Group: grp})
				filtered.Total++
			}
		}
	}
	return filtered
}

// Search handles article and hashtag search queries. Hashtag queries
// (q starts with #) render the flat chip view; text queries render
// category sections.
func (s *Server) Search(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	q := strings.ToLower(raw)
	data, err := s.GetDataWithFallback()
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "no-cache")

	// Empty query renders the homepage top view.
	if q == "" {
		view := topView(*data)
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Push-Url", "/")
			_ = s.Tpl.Partial.ExecuteTemplate(w, "articles", view)
			return
		}
		_ = s.Tpl.Base.ExecuteTemplate(w, "base", view)
		return
	}

	var filtered models.AIDashboardData
	filtered.Generated = data.Generated
	filtered.TopTags = data.TopTags
	if strings.HasPrefix(q, "#") {
		// Hashtag: flat chip view of matching groups.
		filtered = tagView(*data, strings.TrimPrefix(q, "#"))
		filtered.Query = strings.TrimPrefix(raw, "#")
		filtered.Generated = data.Generated
		filtered.TopTags = data.TopTags
	} else {
		filtered.View = "search"
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
					matchedGroups = append(matchedGroups, models.AIGroup{
						Topic: grp.Topic, Articles: matched, Count: len(matched),
						GroupID: grp.GroupID, Tags: grp.Tags,
					})
					filtered.Total += len(matched)
				}
			}
			if len(matchedGroups) > 0 {
				filtered.Categories = append(filtered.Categories, models.AICategoryGroup{Category: g.Category, Groups: matchedGroups, Count: len(matchedGroups)})
			}
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
		s.AICache.UpdateTopic(cat, targetGroup.GroupID, fancyTopic)
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
