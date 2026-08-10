package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	_ "github.com/lib/pq"

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
	groupSvc := &grouping.Service{Gemini: geminiClient}
	summarizeSvc := &summarize.Service{Gemini: geminiClient}

	// Build the retry function (used for DB queries)
	retryFn := func(hours int) (*models.DashboardData, error) {
		return repo.QueryArticles(hours)
	}

	fetchFn := cache.FetchFresh(db, repo, groupSvc, retryFn)

	redisClient := redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:6379",
	})
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		log.Printf("Warning: Redis connection failed: %v", err)
	} else {
		log.Println("Redis connected successfully")
	}

	srv := &handler.Server{
		Cfg:         cfg,
		DB:          db,
		Tpl:         tpl,
		Cache:       cache.NewArticleCache(redisClient, 2*time.Minute),
		AICache:     cache.NewAICache(redisClient, 60*time.Minute),
		Summarizer:  summarizeSvc,
		FetchFn:     fetchFn,
		RetryFn:     retryFn,
	}

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	mux.Handle("GET /static/", http.StripPrefix("/static/", handler.StaticHandler(htmxdailynews.EmbeddedFS)))

	addr := fmt.Sprintf(":%d", cfg.HTTPPort)
	log.Printf("🤖 Miniflux AI Dashboard starting on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
