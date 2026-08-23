package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	htmxdailynews "github.com/panospet/daily-news-aggregator"
	"github.com/panospet/daily-news-aggregator/internal/cache"
	"github.com/panospet/daily-news-aggregator/internal/config"
	"github.com/panospet/daily-news-aggregator/internal/database"
	"github.com/panospet/daily-news-aggregator/internal/gemini"
	"github.com/panospet/daily-news-aggregator/internal/grouping"
	"github.com/panospet/daily-news-aggregator/internal/handler"
	"github.com/panospet/daily-news-aggregator/internal/models"
	"github.com/panospet/daily-news-aggregator/internal/summarize"
)

func basicAuthMiddleware(cfg config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != cfg.AuthUsername || pass != cfg.AuthPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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

	if cfg.GeminiAPIKey == "" {
		log.Println("WARNING: GEMINI_API_KEY not set — AI features disabled")
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

	geminiClient := &gemini.Client{
		HTTPClient: httpClient,
		APIKey:     cfg.GeminiAPIKey,
	}

	repo := &database.Repository{DB: db}
	groupSvc := &grouping.Service{Gemini: geminiClient, Repo: repo, AIServiceURL: cfg.AIServiceURL}
	summarizeSvc := &summarize.Service{Gemini: geminiClient, Extractor: summarize.NewExtractor()}

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

	srv := &handler.Server{
		Cfg:        cfg,
		DB:         db,
		Tpl:        tpl,
		Cache:      cache.NewArticleCache(redisClient, 2*time.Minute),
		AICache:    cache.NewAICache(redisClient, 60*time.Minute),
		GroupMap:   groupMap,
		Summarizer: summarizeSvc,
		FetchFn:    fetchFn,
		RetryFn:    retryFn,
		Repo:       repo,
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	mux.Handle("GET /static/", http.StripPrefix("/static/", handler.StaticHandler(htmxdailynews.EmbeddedFS)))

	addr := fmt.Sprintf(":%d", cfg.HTTPPort)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: basicAuthMiddleware(cfg, handler.GzipMiddleware(mux)),
	}

	// Graceful shutdown: finish in-flight requests (including long AI
	// clustering calls) on SIGINT/SIGTERM instead of dropping them.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
