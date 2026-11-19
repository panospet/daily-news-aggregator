package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	htmxdailynews "github.com/panospet/daily-news-aggregator"
	"github.com/panospet/daily-news-aggregator/internal/cache"
	"github.com/panospet/daily-news-aggregator/internal/config"
	"github.com/panospet/daily-news-aggregator/internal/database"
	"github.com/panospet/daily-news-aggregator/internal/grouping"
	"github.com/panospet/daily-news-aggregator/internal/handler"
	"github.com/panospet/daily-news-aggregator/internal/llm"
	"github.com/panospet/daily-news-aggregator/internal/models"
	"github.com/panospet/daily-news-aggregator/internal/summarize"
)

// authMiddleware guards the whole server. HTTP Basic Auth is always accepted.
// Additionally, a bearer token (API_TOKEN) is accepted on read-only GET API
// routes so external consumers can call the JSON endpoints without the Basic
// Auth credentials. Write paths (POST /summarize*, POST /feedback*, and
// POST /api/briefing-news/generate) stay Basic-Auth-only.
func authMiddleware(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); ok && user == cfg.AuthUsername && pass == cfg.AuthPassword {
			next.ServeHTTP(w, r)
			return
		}
		if cfg.APIToken != "" && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/") {
			if tok := bearerToken(r); tok != "" && tok == cfg.APIToken {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func main() {
	cfg := config.Load()

	db, err := database.OpenDB(cfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("Database ping failed: %v", err)
	}
	log.Println("Database connected successfully")

	tpl, err := handler.LoadTemplates(htmxdailynews.EmbeddedFS)
	if err != nil {
		log.Fatalf("Failed to load templates: %v", err)
	}

	if cfg.CommandCodeAPIKey == "" {
		log.Println("WARNING: COMMAND_CODE_API_KEY not set — AI features disabled")
	}

	if cfg.AuthUsername == "" || cfg.AuthPassword == "" {
		log.Fatal("FATAL: AUTH_USERNAME and AUTH_PASSWORD must be set in the environment")
	}

	// HTTP client with higher concurrent connection limits for parallel Gemini calls
	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 20,
			MaxConnsPerHost:     20,
			IdleConnTimeout:     90 * time.Second,
		},
		Timeout: 300 * time.Second,
	}

	llmClient := &llm.Client{
		HTTPClient:        httpClient,
		CommandCodeAPIKey: cfg.CommandCodeAPIKey,
		CommandCodeModel:  cfg.CommandCodeModel,
		FallbackAPIKey:    cfg.FallbackAPIKey,
	}

	repo := &database.Repository{DB: db}
	groupSvc := &grouping.Service{LLM: llmClient, Repo: repo, AIServiceURL: cfg.AIServiceURL}
	summarizeSvc := &summarize.Service{LLM: llmClient, Extractor: summarize.NewExtractor(), Embeddings: repo}

	// Build the retry function (used for DB queries)
	retryFn := func(hours int) (*models.DashboardData, error) {
		return repo.QueryArticles(hours)
	}

	groupMap := cache.NewGroupMapping(cfg.RedisAddr)
	fetchFn := cache.FetchFresh(db, repo, groupSvc, retryFn, groupMap)

	redisClient := redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr,
	})
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		log.Printf("Warning: Redis connection failed: %v", err)
	} else {
		log.Println("Redis connected successfully")
	}

	aiCache := cache.NewAICache(redisClient, 60*time.Minute)

	newsBlock := cache.NewNewsBlockStore(redisClient, time.Duration(cfg.BriefingNewsBlockTTLHours)*time.Hour)

	srv := &handler.Server{
		Cfg:          cfg,
		DB:           db,
		Tpl:          tpl,
		Cache:        cache.NewArticleCache(redisClient, 2*time.Minute),
		AICache:      aiCache,
		SummaryCache: cache.NewSummaryCache(redisClient, 60*time.Minute),
		NewsBlock:    newsBlock,
		GroupMap:     groupMap,
		Summarizer:   summarizeSvc,
		FetchFn:      fetchFn,
		RetryFn:      retryFn,
		Repo:         repo,
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	srv.RegisterAPIRoutes(mux)
	mux.Handle("GET /static/", http.StripPrefix("/static/", handler.StaticHandler(htmxdailynews.EmbeddedFS)))

	addr := fmt.Sprintf(":%d", cfg.HTTPPort)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: authMiddleware(cfg, handler.GzipMiddleware(mux)),
	}

	// Graceful shutdown: finish in-flight requests (including long AI
	// clustering calls) on SIGINT/SIGTERM instead of dropping them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Keep ai_cache warm on a timer so freshness no longer depends on somebody
	// (or an external watchdog) opening a page. The 90s delay lets the AI
	// service finish loading fastembed/spaCy before the first clustering call.
	aiCache.StartBackgroundRefresh(ctx, db, fetchFn, 90*time.Second)

	// Pre-generate the audio-briefing news blocks on schedule (see
	// BRIEFING_NEWS_PLAN.md). Idempotent; a restart re-checks the stored block.
	srv.StartBriefingPregen(ctx)

	go func() {
		log.Printf("🤖 Miniflux AI Dashboard starting on http://localhost%s", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("Shutdown signal received — draining connections...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("Graceful shutdown timed out, forcing close: %v", err)
	}
	log.Println("Server stopped cleanly")
}
