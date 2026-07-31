package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	_ "github.com/lib/pq"

	htmxdailynews "github.com/panospet/htmx-daily-news"
	"github.com/panospet/htmx-daily-news/internal/cache"
	"github.com/panospet/htmx-daily-news/internal/config"
	"github.com/panospet/htmx-daily-news/internal/database"
	"github.com/panospet/htmx-daily-news/internal/gemini"
	"github.com/panospet/htmx-daily-news/internal/grouping"
	"github.com/panospet/htmx-daily-news/internal/handler"
	"github.com/panospet/htmx-daily-news/internal/models"
	"github.com/panospet/htmx-daily-news/internal/summarize"
	"github.com/panospet/htmx-daily-news/internal/tunnel"
)

func main() {
	cfg := config.Load()

	tun := &tunnel.Tunnel{}
	if err := tun.Open(cfg); err != nil {
		log.Fatalf("Failed to open SSH tunnel: %v", err)
	}
	defer tun.Close()

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

	// Build the retry function (handles tunnel reconnection)
	retryFn := func(hours int) (*models.DashboardData, error) {
		return dbQueryWithRetry(repo, tun, cfg, hours)
	}

	fetchFn := cache.FetchFresh(db, repo, groupSvc, retryFn)

	srv := &handler.Server{
		Cfg:         cfg,
		DB:          db,
		Tpl:         tpl,
		Cache:       cache.NewArticleCache(2 * time.Minute),
		AICache:     cache.NewAICache(60 * time.Minute),
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

// dbQueryWithRetry handles DB query retries with SSH tunnel reconnection.
func dbQueryWithRetry(repo *database.Repository, tun *tunnel.Tunnel, cfg config.Config, hours int) (*models.DashboardData, error) {
	data, err := repo.QueryArticles(hours)
	if err != nil && (strings.Contains(err.Error(), "i/o timeout") || strings.Contains(err.Error(), "connection refused")) {
		log.Printf("DB connection lost, reconnecting tunnel: %v", err)
		tun.Close()
		time.Sleep(1 * time.Second)
		if tunErr := tun.Open(cfg); tunErr != nil {
			log.Printf("Tunnel reopen failed: %v", tunErr)
		} else if pingErr := repo.DB.Ping(); pingErr == nil {
			log.Println("Tunnel reconnected, retrying query")
			time.Sleep(2 * time.Second) // wait for SSH tunnel to be ready
			data, err = repo.QueryArticles(hours)
		}
		// One more retry if still failing
		if err != nil {
			time.Sleep(3 * time.Second)
			tun.Close()
			if tunErr := tun.Open(cfg); tunErr == nil {
				time.Sleep(2 * time.Second)
				data, err = repo.QueryArticles(hours)
			}
		}
	}
	return data, err
}
