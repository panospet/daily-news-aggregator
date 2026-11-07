package handler

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

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
	GroupMap   *cache.GroupMapping
	Summarizer   *summarize.Service
	SummaryCache *cache.SummaryCache
	NewsBlock    *cache.NewsBlockStore
	FetchFn      func(db *sql.DB) (*models.AIDashboardData, error)
	RetryFn    func(hours int) (*models.DashboardData, error)
	Repo       *database.Repository
}

// aiServiceURL returns the configured Python AI service base URL,
// falling back to the local default when unset (e.g. in tests).
func (s *Server) aiServiceURL() string {
	if u := s.Cfg.AIServiceURL; u != "" {
		return u
	}
	return "http://127.0.0.1:8001"
}

// findGroup locates a group by category name and stable group_id.
// Returns the matched group and whether it was found.
func findGroup(data *models.AIDashboardData, cat, groupID string) (models.AIGroup, bool) {
	if data == nil || groupID == "" {
		return models.AIGroup{}, false
	}
	for _, cg := range data.Categories {
		if !strings.EqualFold(cg.Category, cat) {
			continue
		}
		for _, g := range cg.Groups {
			if g.GroupID == groupID {
				return g, true
			}
		}
	}
	return models.AIGroup{}, false
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
	mux.HandleFunc("POST /summarize/stream", s.HandleSummarizeStream)
	mux.HandleFunc("POST /summarize/more", s.HandleSummarizeMore)
	mux.HandleFunc("POST /summarize-article", s.HandleSummarizeArticle)
	mux.HandleFunc("POST /feedback", s.HandleFeedback)
	mux.HandleFunc("POST /feedback/positive", s.HandlePositiveFeedback)
	mux.HandleFunc("GET /api/briefing-news", s.HandleBriefingNews)
	mux.HandleFunc("POST /api/briefing-news/generate", s.HandleBriefingNewsGenerate)
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

	// Deduplicate across categories: multi-tagged groups appear in every
	// category they were tagged with, so merged views must render each
	// story group once (empty group_id = fallback data, keep all).
	seen := make(map[string]bool)
	unique := items[:0]
	for _, it := range items {
		if it.Group.GroupID != "" && seen[it.Group.GroupID] {
			continue
		}
		seen[it.Group.GroupID] = true
		unique = append(unique, it)
	}
	items = unique

	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Group.Count > items[j].Group.Count
	})

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
	// The AI service multi-tags groups across categories, so the same story
	// group can appear under several categories. Render each group once.
	seen := make(map[string]bool)
	for _, cg := range data.Categories {
		for _, g := range cg.Groups {
			if g.GroupID != "" && seen[g.GroupID] {
				continue
			}
			seen[g.GroupID] = true
			feeds := make(map[string]bool)
			for _, a := range g.Articles {
				feeds[a.Feed] = true
			}
			all = append(all, scored{cat: cg.Category, group: g, feeds: len(feeds), articles: len(g.Articles)})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].articles != all[j].articles {
			return all[i].articles > all[j].articles
		}
		return all[i].feeds > all[j].feeds
	})
	if len(all) > topN {
		all = all[:topN]
	}
	view := models.AIDashboardData{
		Total:      len(all),
		Generated:  data.Generated,
		IsFallback: data.IsFallback,
		TopTags:    data.TopTags,
		TopFeeds:   data.TopFeeds,
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
				gCopy := g
				gCopy.Groups = make([]models.AIGroup, len(g.Groups))
				copy(gCopy.Groups, g.Groups)
				sort.SliceStable(gCopy.Groups, func(i, j int) bool {
					return gCopy.Groups[i].Count > gCopy.Groups[j].Count
				})
				filtered.Categories = append(filtered.Categories, gCopy)
				filtered.Total = gCopy.Count
			}
		}
	}
	filtered.Generated = data.Generated
	filtered.IsFallback = data.IsFallback
	filtered.TopTags = data.TopTags
	filtered.TopFeeds = data.TopFeeds
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
	filtered.TopFeeds = data.TopFeeds
	filtered.Categories = data.Categories // sidebar nav; stream renders TopGroups in this view
	filtered.View = "tags"
	// Deduplicate by group_id: the same multi-tagged group can match from
	// several categories; render each story group once.
	seen := make(map[string]bool)
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
				if grp.GroupID != "" && seen[grp.GroupID] {
					continue
				}
				seen[grp.GroupID] = true
				filtered.TopGroups = append(filtered.TopGroups, models.TopGroup{Category: g.Category, Group: grp})
				filtered.Total++
			}
		}
	}
	sort.SliceStable(filtered.TopGroups, func(i, j int) bool {
		return filtered.TopGroups[i].Group.Count > filtered.TopGroups[j].Group.Count
	})
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
		filtered.TopFeeds = data.TopFeeds
	} else if strings.HasPrefix(q, "feed:") {
		// Feed view: flat list of articles from a specific feed.
		// Use q (lowercase) to detect prefix, but we want the actual feed name,
		// so we strip from raw using case-insensitive cut.
		_, feedName, _ := strings.Cut(raw, ":")
		feedName = strings.TrimSpace(feedName)
		filtered.View = "feed"
		filtered.Query = feedName
		filtered.Generated = data.Generated
		filtered.TopTags = data.TopTags
		filtered.TopFeeds = data.TopFeeds
		filtered.Categories = data.Categories // for sidebar

		// Gather all articles for this feed across all groups
		var feedArticles []models.Article
		seenArt := make(map[int64]bool)
		for _, g := range data.Categories {
			for _, grp := range g.Groups {
				for _, a := range grp.Articles {
					if strings.EqualFold(a.Feed, feedName) && !seenArt[a.ID] {
						seenArt[a.ID] = true
						feedArticles = append(feedArticles, a)
					}
				}
			}
		}
		sort.SliceStable(feedArticles, func(i, j int) bool {
			return feedArticles[i].Published.After(feedArticles[j].Published)
		})
		
		// Wrap in a dummy group so we can render it
		filtered.TopGroups = []models.TopGroup{
			{
				Category: "Feed",
				Group: models.AIGroup{
					Topic: feedName,
					Articles: feedArticles,
					Count: len(feedArticles),
				},
			},
		}
		filtered.Total = len(feedArticles)
	} else {
		// Semantic search via embeddings
		groups := s.semanticSearch(raw, data)
		if groups != nil {
			filtered.View = "search"
			filtered.Categories = data.Categories // for sidebar
			filtered.TopGroups = groups
			filtered.TopFeeds = data.TopFeeds
			for _, g := range groups {
				filtered.Total += g.Group.Count
			}
		} else {
			// Fallback to substring matching if semantic search fails
			// Don't set View="search" here — that renders TopGroups (empty).
			// Leave View="" so the template renders Categories sections.
			qNorm := stripAccents(q)
			filtered.TopFeeds = data.TopFeeds
			for _, g := range data.Categories {
				var matchedGroups []models.AIGroup
				for _, grp := range g.Groups {
					var matched []models.Article
					for _, a := range grp.Articles {
						titleNorm := stripAccents(strings.ToLower(a.Title))
						feedNorm := stripAccents(strings.ToLower(a.Feed))
						if strings.Contains(titleNorm, qNorm) ||
							strings.Contains(feedNorm, qNorm) {
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
				sort.SliceStable(matchedGroups, func(i, j int) bool {
					return matchedGroups[i].Count > matchedGroups[j].Count
				})
				if len(matchedGroups) > 0 {
					filtered.Categories = append(filtered.Categories, models.AICategoryGroup{Category: g.Category, Groups: matchedGroups, Count: len(matchedGroups)})
				}
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

// stripAccents removes Greek diacritical marks (accents) from text.
// This ensures "ολυμπιακος" matches "ολυμπιακός" in search comparisons.
func stripAccents(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if mapped, ok := greekAccentMap[r]; ok {
			b.WriteRune(mapped)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// greekAccentMap maps accented Greek characters to their unaccented equivalents.
var greekAccentMap = map[rune]rune{
	'ά': 'α', 'έ': 'ε', 'ή': 'η', 'ί': 'ι', 'ό': 'ο', 'ύ': 'υ', 'ώ': 'ω',
	'Ά': 'Α', 'Έ': 'Ε', 'Ή': 'Η', 'Ί': 'Ι', 'Ό': 'Ο', 'Ύ': 'Υ', 'Ώ': 'Ω',
	'ΐ': 'ι', 'ΰ': 'υ', 'ϊ': 'ι', 'ϋ': 'υ', 'Ϊ': 'Ι', 'Ϋ': 'Υ',
}

// semanticSearch performs embedding-based search: embeds the query,
// finds nearest articles via pgvector, then resolves their groups.
func (s *Server) semanticSearch(query string, data *models.AIDashboardData) []models.TopGroup {
	if s.Repo == nil || s.GroupMap == nil {
		return nil
	}

	// 1. Get query embeddings from the AI service (may return multiple: original + accent-stripped)
	reqBody, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		log.Printf("[search] Failed to marshal query: %v", err)
		return nil
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(s.aiServiceURL()+"/search-embed", "application/json", bytes.NewBuffer(reqBody))
	if err != nil {
		log.Printf("[search] Failed to call search-embed: %v", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("[search] search-embed returned status %d", resp.StatusCode)
		return nil
	}

	var embedResp struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&embedResp); err != nil {
		log.Printf("[search] Failed to decode embeddings: %v", err)
		return nil
	}
	if len(embedResp.Embeddings) == 0 {
		return nil
	}

	// 2. Find nearest articles for EACH embedding variant and merge results
	seenArticle := make(map[int64]bool)
	var allArticleIDs []int64
	for i, emb := range embedResp.Embeddings {
		ids, err := s.Repo.SearchByEmbedding(emb, 50)
		if err != nil {
			log.Printf("[search] pgvector search failed for variant %d: %v", i, err)
			continue
		}
		for _, id := range ids {
			if !seenArticle[id] {
				seenArticle[id] = true
				allArticleIDs = append(allArticleIDs, id)
			}
		}
	}
	if len(allArticleIDs) == 0 {
		return nil
	}

	// 3. Look up group IDs from Redis DB 1
	groupIDs, err := s.GroupMap.GetGroupIDs(allArticleIDs)
	if err != nil {
		log.Printf("[search] Failed to fetch group mappings: %v", err)
		return nil
	}

	// 4. Find the full group data from the AI cache, preserving result order
	// Build an ordered, deduplicated list of group IDs
	seen := make(map[string]bool)
	var orderedGroupIDs []string
	for _, artID := range allArticleIDs {
		gid, ok := groupIDs[artID]
		if !ok || gid == "" || seen[gid] {
			continue
		}
		seen[gid] = true
		orderedGroupIDs = append(orderedGroupIDs, gid)
	}

	// Build a lookup map from the cached data
	type groupWithCat struct {
		category string
		group    models.AIGroup
	}
	groupLookup := make(map[string]groupWithCat)
	for _, cg := range data.Categories {
		for _, g := range cg.Groups {
			if g.GroupID != "" {
				if _, exists := groupLookup[g.GroupID]; !exists {
					groupLookup[g.GroupID] = groupWithCat{category: cg.Category, group: g}
				}
			}
		}
	}

	// 5. Assemble results in relevance order
	var results []models.TopGroup
	for _, gid := range orderedGroupIDs {
		if gc, ok := groupLookup[gid]; ok {
			results = append(results, models.TopGroup{Category: gc.category, Group: gc.group})
		}
	}

	log.Printf("[search] Semantic search for %q: %d articles → %d groups (%d embedding variants)", query, len(allArticleIDs), len(results), len(embedResp.Embeddings))
	return results
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
	start := time.Now()
	defer func() {
		log.Printf("[handler] HandleSummarize took %v", time.Since(start))
	}()

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cat := r.FormValue("category")
	groupID := r.FormValue("group_id")

	data, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}

	var targetGroup models.AIGroup
	var targetTopic string
	found := false

	targetGroup, found = findGroup(data, cat, groupID)
	if found {
		targetTopic = targetGroup.Topic
	}

	if !found {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<div class="summary-refresh">⚠️ Τα δεδομένα άλλαξαν. Κάνε <a href="/" hx-get="/" hx-target="body">ανανέωση</a> και δοκίμασε ξανά.</div>`))
		return
	}

	// Cache hit: an unchanged group never needs another LLM call.
	if cached, ok, cacheErr := s.SummaryCache.GetGroup(cat, groupID); cacheErr == nil && ok {
		cachedTopic := targetTopic
		if len(cached.Sources) > 0 && cached.Sources[0] != "" {
			cachedTopic = cached.Sources[0]
		}
		formattedSummary := strings.ReplaceAll(htmlpkg.EscapeString(cached.Summary), "\n", "<br>")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.Tpl.Partial.ExecuteTemplate(w, "summary", map[string]interface{}{
			"Topic":      cachedTopic,
			"Summary":    template.HTML(formattedSummary),
			"Articles":  targetGroup.Articles,
			"Category":  cat,
			"GroupID":   targetGroup.GroupID,
			"Tags":      targetGroup.Tags,
			"IsFallback": cached.IsFallback,
		})
		return
	}

	// No AI key — show fallback directly
	if s.Cfg.CommandCodeAPIKey == "" {
		targetGroup.Category = cat
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.Tpl.Partial.ExecuteTemplate(w, "summary-fallback", targetGroup)
		return
	}

	summary, err := s.Summarizer.Summarize(targetTopic, targetGroup.Articles)
	if err != nil {
		// Grounding failures (and other errors) fall back to the plain
		// article list — never show an unverified AI summary.
		log.Printf("Summarize error: %v", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.Tpl.Partial.ExecuteTemplate(w, "summary-fallback", targetGroup)
		return
	}

	// Persist the generated summary so repeat clicks do not re-run the LLM.
	if s.SummaryCache != nil {
		_ = s.SummaryCache.SetGroup(cat, groupID, summary)
	}

	// Update cache data so the newly generated fancy topic persists
	if len(summary.Sources) > 0 && summary.Sources[0] != "" {
		fancyTopic := summary.Sources[0]
		targetTopic = fancyTopic

		// Write-update the cached topic name in Redis
		s.AICache.UpdateTopic(cat, targetGroup.GroupID, fancyTopic)
	}

	// Sanitize the AI-generated summary before rendering it as HTML:
	// escape everything, then re-introduce only OUR paragraph breaks.
	// (Summary text is derived from untrusted RSS content — never render it raw.)
	formattedSummary := strings.ReplaceAll(htmlpkg.EscapeString(summary.Summary), "\n", "<br>")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.Tpl.Partial.ExecuteTemplate(w, "summary", map[string]interface{}{
		"Topic":    targetTopic,
		"Summary":  template.HTML(formattedSummary),
		"Articles": targetGroup.Articles,
		"Category": cat,
		"GroupID":  targetGroup.GroupID,
		"Tags":     targetGroup.Tags,
		"IsFallback": summary.IsFallback,
	})
}

// sseSink turns the summarizer's stream callbacks into Server-Sent Events.
type sseSink struct {
	write func(event string, payload interface{}) error
}

func (s *sseSink) Delta(text string) error {
	return s.write("delta", map[string]string{"delta": text})
}

func (s *sseSink) Reset() error {
	return s.write("reset", map[string]string{})
}

// HandleSummarizeStream is HandleSummarize with the summary streamed as it is written.
//
// The reader gets `delta` events (clean text, safe to append with textContent), a
// `reset` event when a failed grounding check makes the model start over, and a final
// `done` event carrying the very same rendered HTML the non-streaming endpoint returns —
// so the card ends up in exactly the state it would have, sources and buttons included.
//
// Streaming shows text before the grounding check has seen it; the check still runs on
// the finished text, and a rejected summary is replaced by the fallback card in the
// `done` event. Nothing unverified survives the request.
func (s *Server) HandleSummarizeStream(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		log.Printf("[handler] HandleSummarizeStream took %v", time.Since(start))
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	cat := r.FormValue("category")
	groupID := r.FormValue("group_id")

	write := func(event string, payload interface{}) error {
		body, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // never let a proxy buffer the stream
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	data, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		_ = write("done", map[string]string{"error": "Data error"})
		return
	}

	targetGroup, found := findGroup(data, cat, groupID)
	if !found {
		_ = write("done", map[string]string{"error": "Τα δεδομένα άλλαξαν. Κάνε ανανέωση και δοκίμασε ξανά."})
		return
	}

	// Cache hit: render the cached summary as the final card with no LLM call.
	if cached, ok, cacheErr := s.SummaryCache.GetGroup(cat, groupID); cacheErr == nil && ok {
		cachedTopic := targetGroup.Topic
		if len(cached.Sources) > 0 && cached.Sources[0] != "" {
			cachedTopic = cached.Sources[0]
		}
		formattedSummary := strings.ReplaceAll(htmlpkg.EscapeString(cached.Summary), "\n", "<br>")
		var buf bytes.Buffer
		if err := s.Tpl.Partial.ExecuteTemplate(&buf, "summary", map[string]interface{}{
			"Topic":      cachedTopic,
			"Summary":    template.HTML(formattedSummary),
			"Articles":   targetGroup.Articles,
			"Category":   cat,
			"GroupID":    targetGroup.GroupID,
			"Tags":       targetGroup.Tags,
			"IsFallback": cached.IsFallback,
		}); err == nil {
			_ = write("done", map[string]string{"html": buf.String()})
			return
		}
	}

	fallbackHTML := func() string {
		var buf bytes.Buffer
		_ = s.Tpl.Partial.ExecuteTemplate(&buf, "summary-fallback", targetGroup)
		return buf.String()
	}

	if s.Cfg.CommandCodeAPIKey == "" {
		_ = write("done", map[string]string{"html": fallbackHTML()})
		return
	}

	sink := &sseSink{write: write}
	summary, err := s.Summarizer.SummarizeStreamContext(r.Context(), targetGroup.Topic, targetGroup.Articles, sink)
	if err != nil {
		// Grounding failures land here too: the streamed text is discarded by the
		// reader and the fallback card takes its place.
		log.Printf("Summarize (stream) error: %v", err)
		_ = write("done", map[string]string{
			"html":  fallbackHTML(),
			"error": "Summary unavailable",
		})
		return
	}

	// Persist the generated summary so a repeat click does not re-run the LLM.
	if s.SummaryCache != nil {
		_ = s.SummaryCache.SetGroup(cat, targetGroup.GroupID, summary)
	}

	targetTopic := targetGroup.Topic
	if len(summary.Sources) > 0 && summary.Sources[0] != "" {
		targetTopic = summary.Sources[0]
		s.AICache.UpdateTopic(cat, targetGroup.GroupID, targetTopic)
	}

	// Same sanitising as the non-streaming path: escape everything, then reintroduce
	// only our own paragraph breaks. (The interim streamed text was appended with
	// textContent, so this card is the first thing rendered as HTML.)
	formattedSummary := strings.ReplaceAll(htmlpkg.EscapeString(summary.Summary), "\n", "<br>")

	var buf bytes.Buffer
	if err := s.Tpl.Partial.ExecuteTemplate(&buf, "summary", map[string]interface{}{
		"Topic":      targetTopic,
		"Summary":    template.HTML(formattedSummary),
		"Articles":   targetGroup.Articles,
		"Category":   cat,
		"GroupID":    targetGroup.GroupID,
		"Tags":       targetGroup.Tags,
		"IsFallback": summary.IsFallback,
	}); err != nil {
		_ = write("done", map[string]string{"html": fallbackHTML(), "error": "render error"})
		return
	}
	_ = write("done", map[string]string{"html": buf.String()})
}

// HandleSummarizeMore generates a supplemental summary for a topic group.
func (s *Server) HandleSummarizeMore(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		log.Printf("[handler] HandleSummarizeMore took %v", time.Since(start))
	}()

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cat := r.FormValue("category")
	groupID := r.FormValue("group_id")
	existingSummary := r.FormValue("existing_summary")

	data, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}

	var targetGroup models.AIGroup
	var targetTopic string
	found := false

	targetGroup, found = findGroup(data, cat, groupID)
	if found {
		targetTopic = targetGroup.Topic
	}

	if !found {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<div class="summary-refresh">⚠️ Τα δεδομένα άλλαξαν. Κάνε <a href="/" hx-get="/" hx-target="body">ανανέωση</a> και δοκίμασε ξανά.</div>`))
		return
	}

	if s.Cfg.CommandCodeAPIKey == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<div class="summary" style="margin-top: 15px; border-top: 1px dashed var(--line); padding-top: 15px;"><em>Supplemental summary unavailable (No API key).</em></div>`))
		return
	}

	moreDetails, err := s.Summarizer.SummarizeMore(targetTopic, existingSummary, targetGroup.Articles)
	if err != nil {
		log.Printf("SummarizeMore error: %v", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// Instead of replacing the whole card on failure, just append an error message or do nothing
		_, _ = w.Write([]byte(`<div class="summary" style="margin-top: 15px; border-top: 1px dashed var(--line); padding-top: 15px; color: var(--text-muted);"><em>Αποτυχία δημιουργίας επιπλέον σύνοψης.</em></div>`))
		return
	}

	formattedSummary := strings.ReplaceAll(htmlpkg.EscapeString(moreDetails), "\n", "<br>")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Render just the new summary piece so it can be appended
	// Removing the button is handled by outerHTML swapping on the button itself.
	_, _ = w.Write([]byte(fmt.Sprintf(`<div class="summary" style="margin-top: 15px; border-top: 1px dashed var(--line); padding-top: 15px;">%s</div>`, formattedSummary)))
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

	targetGroup, found = findGroup(data, cat, groupID)
	if found {
		targetTopic = targetGroup.Topic
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

	targetGroup, found = findGroup(data, cat, groupID)
	if found {
		targetTopic = targetGroup.Topic
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

// HandleSummarizeArticle generates an AI summary for a single article.
func (s *Server) HandleSummarizeArticle(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	articleID, _ := strconv.ParseInt(r.FormValue("article_id"), 10, 64)
	if articleID == 0 {
		http.Error(w, "Missing article_id", http.StatusBadRequest)
		return
	}

	data, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		http.Error(w, "Data error", http.StatusInternalServerError)
		return
	}

	var targetArticle *models.Article
	for _, cg := range data.Categories {
		for _, g := range cg.Groups {
			for _, a := range g.Articles {
				if a.ID == articleID {
					// Need a copy so we can take the address safely
					artCopy := a
					targetArticle = &artCopy
					break
				}
			}
			if targetArticle != nil {
				break
			}
		}
		if targetArticle != nil {
			break
		}
	}

	if targetArticle == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<li class="summary-refresh">⚠️ Article not found.</li>`))
		return
	}

	// Cache hit: a single article never needs another LLM call.
	if cached, ok, cacheErr := s.SummaryCache.GetArticle(articleID); cacheErr == nil && ok {
		formattedSummary := strings.ReplaceAll(htmlpkg.EscapeString(cached.Summary), "\n", "<br>")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.Tpl.Partial.ExecuteTemplate(w, "summary-article", map[string]interface{}{
			"Article": targetArticle,
			"Summary": template.HTML(formattedSummary),
		})
		return
	}

	if s.Cfg.CommandCodeAPIKey == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.Tpl.Partial.ExecuteTemplate(w, "summary-article-fallback", targetArticle)
		return
	}

	summary, err := s.Summarizer.Summarize(targetArticle.Title, []models.Article{*targetArticle})
	if err != nil {
		log.Printf("Summarize article error: %v", err)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = s.Tpl.Partial.ExecuteTemplate(w, "summary-article-fallback", targetArticle)
		return
	}

	if s.SummaryCache != nil {
		_ = s.SummaryCache.SetArticle(articleID, summary)
	}

	formattedSummary := strings.ReplaceAll(htmlpkg.EscapeString(summary.Summary), "\n", "<br>")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.Tpl.Partial.ExecuteTemplate(w, "summary-article", map[string]interface{}{
		"Article": targetArticle,
		"Summary": template.HTML(formattedSummary),
	})
}
