package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/panospet/daily-news-aggregator/internal/cache"
	"github.com/panospet/daily-news-aggregator/internal/models"
)

const (
	sportsCategory = "Αθλητισμός"
	maxSportsItems = 2
)

// athensLocation is the local timezone for scheduling and timestamps.
var athensLocation = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		return time.Local
	}
	return loc
}()

var sportsTopicRe = regexp.MustCompile(
	`(?i)(ολυμπιακ|παναθηναικ|παοκ|\bαεκ\b|\bαρης\b|euroleague|super\s*league|\bnba\b|champions\s*league|premier\s*league|europa\s*league|conference\s*league|τζοκοβιτς|τσιτσιπας|αντετοκουνμπο|φουρνιε|γιαννακοπουλ|ζαλγκιρις|εβερτον|κοβεντρι|gazz\s*floor|πεναλτι|γκολ|μπασκετ|ποδοσφαιρ)`,
)

func stripAccentsLower(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isSportsGroup reports whether a topic group is a sports story based on its
// assigned category, tags, feed categories of its articles, or headline terms.
func isSportsGroup(tg models.TopGroup) bool {
	if strings.EqualFold(strings.TrimSpace(tg.Category), sportsCategory) {
		return true
	}
	for _, t := range tg.Group.Tags {
		if strings.EqualFold(strings.TrimSpace(t), sportsCategory) {
			return true
		}
	}
	arts := tg.Group.Articles
	if len(arts) > 0 {
		sportsArts := 0
		for _, a := range arts {
			cat := strings.ToLower(strings.TrimSpace(a.Category))
			if cat == "sports" || cat == "αθλητισμός" || cat == "αθλητικα" || cat == "αθλητικά" {
				sportsArts++
			}
		}
		if sportsArts*2 >= len(arts) {
			return true
		}
	}
	return sportsTopicRe.MatchString(stripAccentsLower(tg.Group.Topic))
}

func isSportsItem(it cache.NewsBlockItem) bool {
	if strings.EqualFold(strings.TrimSpace(it.Category), sportsCategory) {
		return true
	}
	return sportsTopicRe.MatchString(stripAccentsLower(it.Topic))
}

// selectBriefingGroups partitions topGroups into non-sports and sports, caps
// sports at maxSports (2), and places the sports groups at the end of the
// selected slice of length <= n.
func selectBriefingGroups(topGroups []models.TopGroup, n, maxSports int) []models.TopGroup {
	if n <= 0 || len(topGroups) == 0 {
		return nil
	}
	if maxSports < 0 {
		maxSports = 0
	}
	var nonSports, sports []models.TopGroup
	for _, tg := range topGroups {
		if isSportsGroup(tg) {
			tg.Category = sportsCategory
			sports = append(sports, tg)
		} else {
			nonSports = append(nonSports, tg)
		}
	}
	if len(sports) > maxSports {
		sports = sports[:maxSports]
	}
	maxNonSports := n - len(sports)
	if maxNonSports < 0 {
		maxNonSports = 0
		sports = sports[:n]
	}
	if len(nonSports) > maxNonSports {
		nonSports = nonSports[:maxNonSports]
	}
	out := make([]models.TopGroup, 0, len(nonSports)+len(sports))
	out = append(out, nonSports...)
	out = append(out, sports...)
	return out
}

// orderAndCapBriefingItems ensures that in any returned list (optionally capped
// to limit > 0), there are at most maxSports sports items and they appear last.
func orderAndCapBriefingItems(items []cache.NewsBlockItem, limit, maxSports int) []cache.NewsBlockItem {
	var nonSports, sports []cache.NewsBlockItem
	for _, it := range items {
		if isSportsItem(it) {
			it.Category = sportsCategory
			sports = append(sports, it)
		} else {
			nonSports = append(nonSports, it)
		}
	}
	if maxSports >= 0 && len(sports) > maxSports {
		sports = sports[:maxSports]
	}
	if limit > 0 {
		if len(sports) > limit {
			sports = sports[:limit]
		}
		maxNonSports := limit - len(sports)
		if len(nonSports) > maxNonSports {
			nonSports = nonSports[:maxNonSports]
		}
	}
	out := make([]cache.NewsBlockItem, 0, len(nonSports)+len(sports))
	out = append(out, nonSports...)
	out = append(out, sports...)
	return out
}

// validSlot reports whether slot is one of the two briefing slots.
func validSlot(slot string) bool {
	return slot == "morning" || slot == "evening"
}

// HandleBriefingNews serves the pre-generated news block for a slot.
// Supports an optional ?limit=N (or ?n=N) query parameter; sports items are
// always capped at 2 and placed last.
func (s *Server) HandleBriefingNews(w http.ResponseWriter, r *http.Request) {
	slot := r.URL.Query().Get("slot")
	if slot == "" {
		slot = "morning"
	}
	if !validSlot(slot) {
		writeBriefingJSON(w, http.StatusBadRequest, map[string]string{"error": "slot must be morning or evening"})
		return
	}
	if s.NewsBlock == nil {
		writeBriefingJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "news block store unavailable"})
		return
	}

	block, ok, err := s.NewsBlock.Get(slot)
	if err != nil {
		log.Printf("[briefing] news block read failed: %v", err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to read news block"})
		return
	}
	if !ok {
		writeBriefingJSON(w, http.StatusNotFound, map[string]string{"error": "no news block for slot " + slot + " yet"})
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit, _ = strconv.Atoi(r.URL.Query().Get("n"))
	}
	block.Items = orderAndCapBriefingItems(block.Items, limit, maxSportsItems)
	writeBriefingJSON(w, http.StatusOK, block)
}

// HandleBriefingNewsGenerate (re)generates the news block for a slot synchronously.
// It is exposed for manual runs and as a crontab trigger; generation is idempotent.
func (s *Server) HandleBriefingNewsGenerate(w http.ResponseWriter, r *http.Request) {
	slot := r.URL.Query().Get("slot")
	if slot == "" {
		slot = "morning"
	}
	if !validSlot(slot) {
		writeBriefingJSON(w, http.StatusBadRequest, map[string]string{"error": "slot must be morning or evening"})
		return
	}

	genCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()

	block, err := s.GenerateNewsBlock(genCtx, slot)
	if err != nil {
		log.Printf("[briefing] generation failed for %s: %v", slot, err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit, _ = strconv.Atoi(r.URL.Query().Get("n"))
	}
	if limit > 0 {
		block.Items = orderAndCapBriefingItems(block.Items, limit, maxSportsItems)
	}
	writeBriefingJSON(w, http.StatusOK, block)
}

// GenerateNewsBlock selects the top N groups (reusing the homepage topView
// ranking, with sports capped at 2 and placed last), summarizes each via the
// grounded pipeline, and stores the result as a read-only news block.
func (s *Server) GenerateNewsBlock(ctx context.Context, slot string) (cache.NewsBlock, error) {
	s.briefingMu.Lock()
	defer s.briefingMu.Unlock()

	var (
		data *models.AIDashboardData
		err  error
	)
	if s.AICache != nil {
		data, err = s.AICache.EnsureFresh(s.DB, 90*time.Minute, s.FetchFn)
	}
	if err != nil || data == nil {
		if s.AICache != nil && s.Cache != nil {
			data, err = s.GetDataWithFallback()
		}
		if err != nil || data == nil {
			if err == nil {
				err = fmt.Errorf("no dashboard data available")
			}
			return cache.NewsBlock{}, err
		}
	}

	n := s.Cfg.BriefingPregenN
	if n <= 0 {
		n = 20
	}

	view := topView(*data)
	groups := selectBriefingGroups(view.TopGroups, n, maxSportsItems)
	if len(groups) == 0 {
		return cache.NewsBlock{}, fmt.Errorf("no topic groups available to summarize")
	}

	// Summarize in parallel with a bounded worker count so we never flood the LLM.
	const maxConcurrent = 5
	sem := make(chan struct{}, maxConcurrent)
	items := make([]cache.NewsBlockItem, len(groups))
	var wg sync.WaitGroup
	for i, tg := range groups {
		wg.Add(1)
		go func(i int, tg models.TopGroup) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if item, ok := s.summarizeGroup(ctx, tg); ok {
				items[i] = item
			}
		}(i, tg)
	}
	wg.Wait()

	// Compact out the groups that failed.
	compacted := items[:0]
	for _, it := range items {
		if it.Topic != "" || it.SummaryText != "" {
			compacted = append(compacted, it)
		}
	}

	compacted = orderAndCapBriefingItems(compacted, 0, maxSportsItems)

	minRequired := 5
	if len(groups) < minRequired {
		minRequired = len(groups)
	}
	if len(compacted) < minRequired {
		return cache.NewsBlock{}, fmt.Errorf("insufficient verified summaries for %s: got %d/%d (need at least %d)", slot, len(compacted), len(groups), minRequired)
	}

	block := cache.NewsBlock{
		Slot:      slot,
		Generated: time.Now().In(athensLocation),
		Items:     compacted,
	}
	if s.NewsBlock != nil {
		if err := s.NewsBlock.Set(slot, block); err != nil {
			log.Printf("[briefing] failed to store news block for %s: %v", slot, err)
			return cache.NewsBlock{}, fmt.Errorf("store news block: %w", err)
		}
	}
	log.Printf("[briefing] news block for %s generated: %d items", slot, len(compacted))
	return block, nil
}

// summarizeGroup returns a speech-ready item for one top group, reusing the
// summary cache when available so a warm cache never re-runs the LLM.
func (s *Server) summarizeGroup(ctx context.Context, tg models.TopGroup) (cache.NewsBlockItem, bool) {
	cat, g := tg.Category, tg.Group
	if isSportsGroup(tg) {
		cat = sportsCategory
	}
	topic := g.Topic

	summary := models.AISummaryResult{}
	found := false
	if s.SummaryCache != nil {
		if cached, ok, err := s.SummaryCache.GetGroup(cat, g.GroupID); err == nil && ok {
			summary, found = cached, true
		} else if cat != tg.Category {
			if cachedOrig, okOrig, errOrig := s.SummaryCache.GetGroup(tg.Category, g.GroupID); errOrig == nil && okOrig {
				summary, found = cachedOrig, true
			}
		}
	}
	if !found {
		if s.Summarizer == nil {
			return cache.NewsBlockItem{}, false
		}
		res, err := s.Summarizer.SummarizeContext(ctx, topic, g.Articles)
		if err != nil {
			log.Printf("[briefing] summarize failed for %q (%s): %v", topic, g.GroupID, err)
			return cache.NewsBlockItem{}, false
		}
		summary = res
		if s.SummaryCache != nil {
			_ = s.SummaryCache.SetGroup(cat, g.GroupID, summary)
		}
		if len(summary.Sources) > 0 && strings.TrimSpace(summary.Sources[0]) != "" && s.AICache != nil {
			s.AICache.UpdateTopic(tg.Category, g.GroupID, strings.TrimSpace(summary.Sources[0]))
		}
	}

	finalTopic := topic
	if len(summary.Sources) > 0 && strings.TrimSpace(summary.Sources[0]) != "" {
		finalTopic = strings.TrimSpace(summary.Sources[0])
	}

	text := strings.TrimSpace(summary.Summary)
	if text == "" {
		return cache.NewsBlockItem{}, false
	}

	sources := make([]cache.NewsBlockSource, 0, len(g.Articles))
	for _, a := range g.Articles {
		src := cache.NewsBlockSource{
			Newspaper: strings.TrimSpace(a.Feed),
			Title:     strings.TrimSpace(a.Title),
			URL:       strings.TrimSpace(a.URL),
		}
		if src.Newspaper == "" && src.Title == "" && src.URL == "" {
			continue
		}
		sources = append(sources, src)
	}

	return cache.NewsBlockItem{
		Topic:       finalTopic,
		Category:    cat,
		SummaryText: text,
		Sources:     sources,
	}, true
}

// StartBriefingPregen runs the pre-generation scheduler: it wakes every minute
// and, once a slot's pregen time has passed for the day, generates that slot's
// news block if it has not already been generated today. Failures are retried
// with a 30-minute backoff.
func (s *Server) StartBriefingPregen(ctx context.Context) {
	go func() {
		slots := []struct {
			name string
			at   string
		}{
			{"morning", s.Cfg.BriefingPregenMorning},
			{"evening", s.Cfg.BriefingPregenEvening},
		}

		var mu sync.Mutex
		nextAttempt := make(map[string]time.Time)

		trySlot := func(name string) {
			log.Printf("[briefing] pre-generating %s news block", name)
			if _, err := s.GenerateNewsBlock(ctx, name); err != nil {
				log.Printf("[briefing] pre-generation failed for %s: %v", name, err)
			}
		}

		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		eveningMin, eveningErr := parseHHMM(s.Cfg.BriefingPregenEvening)

		for {
			select {
			case <-ctx.Done():
				log.Println("[briefing] pre-generation scheduler stopped")
				return
			case <-ticker.C:
				now := time.Now().In(athensLocation)
				today := now.Format("2006-01-02")
				nowMin := now.Hour()*60 + now.Minute()

				for _, sl := range slots {
					t, err := parseHHMM(sl.at)
					if err != nil {
						log.Printf("[briefing] invalid pregen time %q: %v", sl.at, err)
						continue
					}
					if nowMin < t {
						continue // slot time not reached yet today
					}
					// Once the evening slot window begins, do not catch up a missed
					// morning pre-generation concurrently with evening.
					if sl.name == "morning" && eveningErr == nil && nowMin >= eveningMin {
						continue
					}
					if s.NewsBlock != nil {
						if block, ok, _ := s.NewsBlock.Get(sl.name); ok {
							if block.Generated.In(athensLocation).Format("2006-01-02") == today {
								continue // already generated today
							}
						}
					}
					mu.Lock()
					if !nextAttempt[sl.name].IsZero() && now.Before(nextAttempt[sl.name]) {
						mu.Unlock()
						continue // backing off after a recent failure
					}
					nextAttempt[sl.name] = now.Add(30 * time.Minute)
					mu.Unlock()
					go trySlot(sl.name)
				}
			}
		}
	}()
}

// parseHHMM parses "HH:MM" into minutes since midnight.
func parseHHMM(s string) (int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid time %q", s)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid time %q", s)
	}
	return h*60 + m, nil
}

// writeBriefingJSON writes v as JSON with the given status code.
func writeBriefingJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
