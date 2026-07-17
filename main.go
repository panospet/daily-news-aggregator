package main

import (
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

// ---------- config ----------

type config struct {
	dbHost    string
	dbPort    int
	dbName    string
	dbUser    string
	dbPass    string
	sshHost   string
	sshRemote int
	sshTunnel int
	httpPort  int
}

func loadConfig() config {
	return config{
		dbHost:    envStr("MF_DB_HOST", "127.0.0.1"),
		dbPort:    envInt("MF_DB_PORT", 5433),
		dbName:    envStr("MF_DB_NAME", "miniflux"),
		dbUser:    envStr("MF_DB_USER", "external_reader"),
		dbPass:    envStr("MF_DB_PASSWORD", ""),
		sshHost:   envStr("MF_SSH_HOST", ""),
		sshRemote: envInt("MF_SSH_REMOTE_PORT", 5435),
		sshTunnel: envInt("MF_SSH_TUNNEL_PORT", 5433),
		httpPort:  envInt("PORT", 8080),
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return def
}

// ---------- SSH tunnel ----------

type tunnel struct {
	cmd *exec.Cmd
	mu  sync.Mutex
}

func (t *tunnel) open(cfg config) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if cfg.sshHost == "" {
		log.Println("No SSH host configured — connecting directly to DB")
		return nil
	}

	// Kill any stale tunnel on same port
	_ = exec.Command("pkill", "-f", fmt.Sprintf("ssh.*-L %d:", cfg.sshTunnel)).Run()
	time.Sleep(500 * time.Millisecond)

	cmd := exec.Command("ssh",
		"-f", "-N",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=/dev/null",
		"-L", fmt.Sprintf("%d:127.0.0.1:%d", cfg.sshTunnel, cfg.sshRemote),
		cfg.sshHost,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("SSH tunnel failed: %w\n%s", err, out)
	}
	t.cmd = cmd
	log.Printf("SSH tunnel open: localhost:%d → %s:%d", cfg.sshTunnel, cfg.sshHost, cfg.sshRemote)
	return nil
}

func (t *tunnel) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	_ = exec.Command("pkill", "-f", fmt.Sprintf("ssh.*-L %d:", 5433)).Run()
}

// ---------- database ----------

type article struct {
	Title      string
	URL        string
	Published  time.Time
	Feed       string
	Category   string
	SourceCount int
	Sources    []source
	TimeAgo    string
}

type source struct {
	Feed string
	URL  string
	Cat  string
}

type categoryGroup struct {
	Category string
	Articles []article
	Count    int
}

type dashboardData struct {
	Categories []categoryGroup
	Total      int
	Generated  string
}

func openDB(cfg config) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable connect_timeout=5",
		cfg.dbHost, cfg.dbPort, cfg.dbUser, cfg.dbPass, cfg.dbName,
	)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	return db, nil
}

func queryArticles(db *sql.DB, hours int) (*dashboardData, error) {
	since := time.Now().Add(-time.Duration(hours) * time.Hour)

	rows, err := db.Query(`
		SELECT e.title, e.url, e.published_at, f.title AS feed, c.title AS category
		FROM entries e
		JOIN feeds f ON f.id = e.feed_id
		JOIN categories c ON c.id = f.category_id
		WHERE e.published_at >= $1
		  AND (f.disabled = false OR f.disabled IS NULL)
		ORDER BY e.published_at DESC
	`, since)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	// Dedup by title, collect sources
	type entry struct {
		title     string
		url       string
		published time.Time
		feed      string
		category  string
	}
	seen := make(map[string]*entry)
	var ordered []string

	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.title, &e.url, &e.published, &e.feed, &e.category); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		e.title = strings.TrimSpace(e.title)
		if _, exists := seen[e.title]; !exists {
			seen[e.title] = &e
			ordered = append(ordered, e.title)
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows iter: %w", err)
	}

	// Group by category
	catMap := make(map[string][]article)
	for _, title := range ordered {
		e := seen[title]
		art := article{
			Title:    e.title,
			URL:      e.url,
			Published: e.published,
			Feed:     e.feed,
			Category: e.category,
			TimeAgo:  timeAgo(e.published),
		}
		catMap[e.category] = append(catMap[e.category], art)
	}

	// Category order (user's preference)
	preferredOrder := []string{"music", "tech", "sports", "random", "panionara"}
	catSet := make(map[string]bool)
	for _, c := range preferredOrder {
		catSet[c] = true
	}

	var groups []categoryGroup
	for _, cat := range preferredOrder {
		if arts, ok := catMap[cat]; ok {
			groups = append(groups, categoryGroup{
				Category: cat,
				Articles: arts,
				Count:    len(arts),
			})
			delete(catMap, cat)
		}
	}
	// Remaining categories (not in preferred order)
	for cat, arts := range catMap {
		groups = append(groups, categoryGroup{
			Category: cat,
			Articles: arts,
			Count:    len(arts),
		})
	}

	total := len(ordered)
	return &dashboardData{
		Categories: groups,
		Total:      total,
		Generated:  time.Now().Format("15:04:05"),
	}, nil
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	default:
		return t.Format("Jan 2 15:04")
	}
}

// ---------- cache ----------

type cache struct {
	mu       sync.RWMutex
	data     *dashboardData
	ts       time.Time
	interval time.Duration
}

func newCache(interval time.Duration) *cache {
	return &cache{interval: interval}
}

func (c *cache) get(db *sql.DB) (*dashboardData, error) {
	c.mu.RLock()
	if c.data != nil && time.Since(c.ts) < c.interval {
		d := c.data
		c.mu.RUnlock()
		return d, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock
	if c.data != nil && time.Since(c.ts) < c.interval {
		return c.data, nil
	}

	data, err := queryArticles(db, 24)
	if err != nil {
		// Return stale data if available
		if c.data != nil {
			log.Printf("DB query failed, serving stale data: %v", err)
			return c.data, nil
		}
		return nil, err
	}
	c.data = data
	c.ts = time.Now()
	return data, nil
}

// ---------- templates ----------

//go:embed templates static
var embeddedFS embed.FS

type templateSet struct {
	base    *template.Template
	partial *template.Template
}

func loadTemplates() (*templateSet, error) {
	funcMap := template.FuncMap{
		"upper": strings.ToUpper,
		"catIcon": func(cat string) string {
			switch strings.ToLower(cat) {
			case "music":
				return "🎵"
			case "tech":
				return "💻"
			case "sports":
				return "⚽"
			case "random":
				return "🌀"
			case "panionara":
				return "🔴🔵"
			default:
				return "📰"
			}
		},
	}

	// Parse templates from embedded FS
	tpl := template.New("").Funcs(funcMap)

	// Walk and parse all .tmpl files
	err := fs.WalkDir(embeddedFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".tmpl") {
			content, err := embeddedFS.ReadFile(path)
			if err != nil {
				return err
			}
			_, err = tpl.Parse(string(content))
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &templateSet{
		base:    tpl,
		partial: tpl,
	}, nil
}

// ---------- HTTP handlers ----------

type server struct {
	cfg    config
	db     *sql.DB
	tpl    *templateSet
	cache  *cache
	tunnel *tunnel
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	data, err := s.cache.get(s.db)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		log.Printf("home: %v", err)
		return
	}
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "no-cache")

	// HTMX request: return only the articles partial
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Push-Url", "/")
		err = s.tpl.partial.ExecuteTemplate(w, "articles", data)
		if err != nil {
			log.Printf("template error: %v", err)
		}
		return
	}

	err = s.tpl.base.ExecuteTemplate(w, "base", data)
	if err != nil {
		log.Printf("template error: %v", err)
	}
}

func (s *server) categoryFilter(w http.ResponseWriter, r *http.Request) {
	cat := r.URL.Query().Get("cat")
	data, err := s.cache.get(s.db)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Vary", "HX-Request")
	w.Header().Set("Cache-Control", "no-cache")

	// Build filtered data
	var filtered dashboardData
	if cat == "" || cat == "all" {
		filtered = *data
	} else {
		for _, g := range data.Categories {
			if strings.EqualFold(g.Category, cat) {
				filtered.Categories = append(filtered.Categories, g)
				filtered.Total = g.Count
				break
			}
		}
		for _, g := range data.Categories {
			if !strings.EqualFold(g.Category, cat) {
				filtered.Categories = append(filtered.Categories, categoryGroup{
					Category: g.Category,
					Count:    0,
				})
			}
		}
	}
	filtered.Generated = data.Generated

	_ = s.tpl.partial.ExecuteTemplate(w, "articles", filtered)
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	data, err := s.cache.get(s.db)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")

	if q == "" {
		_ = s.tpl.partial.ExecuteTemplate(w, "articles", data)
		return
	}

	var filtered dashboardData
	filtered.Generated = data.Generated
	for _, g := range data.Categories {
		var matched []article
		for _, a := range g.Articles {
			if strings.Contains(strings.ToLower(a.Title), q) ||
				strings.Contains(strings.ToLower(a.Feed), q) {
				matched = append(matched, a)
			}
		}
		if len(matched) > 0 {
			filtered.Categories = append(filtered.Categories, categoryGroup{
				Category: g.Category,
				Articles: matched,
				Count:    len(matched),
			})
			filtered.Total += len(matched)
		} else {
			filtered.Categories = append(filtered.Categories, categoryGroup{
				Category: g.Category,
				Count:    0,
			})
		}
	}

	_ = s.tpl.partial.ExecuteTemplate(w, "articles", filtered)
}

func (s *server) stats(w http.ResponseWriter, r *http.Request) {
	data, err := s.cache.get(s.db)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	_ = s.tpl.partial.ExecuteTemplate(w, "stats", data)
}

// ---------- static files ----------

func staticHandler() http.Handler {
	staticFS, err := fs.Sub(embeddedFS, "static")
	if err != nil {
		log.Fatalf("static sub fs: %v", err)
	}
	return http.FileServer(http.FS(staticFS))
}

// ---------- main ----------

func main() {
	cfg := loadConfig()

	// SSH tunnel
	t := &tunnel{}
	if err := t.open(cfg); err != nil {
		log.Fatalf("Failed to open SSH tunnel: %v", err)
	}
	defer t.close()

	// Database
	db, err := openDB(cfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Verify connection
	if err := db.Ping(); err != nil {
		log.Fatalf("Database ping failed: %v", err)
	}
	log.Println("Database connected successfully")

	// Templates
	tpl, err := loadTemplates()
	if err != nil {
		log.Fatalf("Failed to load templates: %v", err)
	}

	// Server
	srv := &server{
		cfg:    cfg,
		db:     db,
		tpl:    tpl,
		cache:  newCache(2 * time.Minute),
		tunnel: t,
	}

	// Routes
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", srv.home)
	mux.HandleFunc("GET /filter", srv.categoryFilter)
	mux.HandleFunc("GET /search", srv.search)
	mux.HandleFunc("GET /stats", srv.stats)
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))

	addr := fmt.Sprintf(":%d", cfg.httpPort)
	log.Printf("🚀 Server starting on http://localhost%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
