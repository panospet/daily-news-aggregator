package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/panospet/daily-news-aggregator/internal/cache"
	"github.com/panospet/daily-news-aggregator/internal/models"
)

// athensLocation is the local timezone for scheduling and timestamps.
var athensLocation = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Athens")
	if err != nil {
		return time.Local
	}
	return loc
}()

// validSlot reports whether slot is one of the two briefing slots.
func validSlot(slot string) bool {
	return slot == "morning" || slot == "evening"
}

// HandleBriefingNews serves the pre-generated news block for a slot.
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

	block, err := s.GenerateNewsBlock(r.Context(), slot)
	if err != nil {
		log.Printf("[briefing] generation failed for %s: %v", slot, err)
		writeBriefingJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeBriefingJSON(w, http.StatusOK, block)
}

// GenerateNewsBlock selects the top N groups (reusing the homepage topView
// ranking), summarizes each via the grounded pipeline, and stores the result as
// a read-only news block. It is idempotent and safe to run repeatedly.
func (s *Server) GenerateNewsBlock(ctx context.Context, slot string) (cache.NewsBlock, error) {
	data, err := s.AICache.Get(s.DB, s.FetchFn)
	if err != nil {
		return cache.NewsBlock{}, err
	}

	n := s.Cfg.BriefingPregenN
	if n <= 0 {
		n = 20
	}

	view := topView(*data)
	groups := view.TopGroups
	if len(groups) > n {
		groups = groups[:n]
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
			sem <- struct{}{}
			defer func() { <-sem }()
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

	// Total failure → a titles-only block so the consumer still has something.
	if len(compacted) == 0 {
		for _, tg := range groups {
			compacted = append(compacted, cache.NewsBlockItem{
				Topic:       tg.Group.Topic,
				Category:    tg.Category,
				SummaryText: tg.Group.Topic,
			})
		}
	}

	block := cache.NewsBlock{
		Slot:      slot,
		Generated: time.Now().In(athensLocation),
		Items:     compacted,
	}
	if s.NewsBlock != nil {
		if err := s.NewsBlock.Set(slot, block); err != nil {
			log.Printf("[briefing] failed to store news block for %s: %v", slot, err)
		}
	}
	log.Printf("[briefing] news block for %s generated: %d items", slot, len(compacted))
	return block, nil
}

// summarizeGroup returns a speech-ready item for one top group, reusing the
// summary cache when available so a warm cache never re-runs the LLM.
func (s *Server) summarizeGroup(ctx context.Context, tg models.TopGroup) (cache.NewsBlockItem, bool) {
	cat, g := tg.Category, tg.Group
	topic := g.Topic

	summary := models.AISummaryResult{}
	found := false
	if s.SummaryCache != nil {
		if cached, ok, err := s.SummaryCache.GetGroup(cat, g.GroupID); err == nil && ok {
			summary, found = cached, true
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
	}

	finalTopic := topic
	if len(summary.Sources) > 0 && strings.TrimSpace(summary.Sources[0]) != "" {
		finalTopic = strings.TrimSpace(summary.Sources[0])
	}

	text := strings.TrimSpace(summary.Summary)
	if text == "" {
		text = finalTopic
	}

	return cache.NewsBlockItem{
		Topic:       finalTopic,
		Category:    cat,
		SummaryText: text,
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
			if _, err := s.GenerateNewsBlock(context.Background(), name); err != nil {
				log.Printf("[briefing] pre-generation failed for %s: %v", name, err)
			}
		}

		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

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
