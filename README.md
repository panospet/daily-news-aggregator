# newsbrief — Daily News Aggregator 📰🤖

An intelligent, real-time Greek & international news aggregation and summarization platform. Powered by Go, HTMX 2.0, PostgreSQL (Miniflux schema + pgvector), Redis 7, a Python multilingual semantic clustering sidecar, and LLM-grounded summarization (CommandCode / Gemini).

The dashboard is titled **newsbrief**. Beyond serving the interactive news dashboard, this service is the **grounded news producer** for the audio-briefing pipeline.

---

## 🌐 Pipeline Context

This repository is the news producer in a small pipeline. It exposes speech-ready summaries that [`audio-briefing`](https://github.com/panospet/audio-briefing) reads and that [`voice-service`](https://github.com/panospet/voice-service) can narrate:

```
   RSS feeds ──> Miniflux ──> PostgreSQL ──> Python AI sidecar ──> Go dashboard (:8080)
                                                                    │  GET /api/briefing-news
                                                                    ▼
                                                              audio-briefing
```

| Linked Service | Port / Protocol | Connection Details |
|---|---|---|
| [`audio-briefing`](https://github.com/panospet/audio-briefing) | `8080` HTTP (Basic Auth) | Reads speech-ready Greek news summaries pre-generated twice daily (`07:45` & `19:45` Athens) from `GET /api/briefing-news` (`?slot=morning` or `?slot=evening`), with an on-demand fallback `POST /api/briefing-news/generate`. |
| [`voice-service`](https://github.com/panospet/voice-service) | `8002` HTTP | Speech synthesis engine for web article narration ("read aloud"). |
| Egress proxy (optional) | HTTP proxy | Miniflux and Flaresolverr can route RSS downloads through an HTTP egress proxy via the bundled `cloudflared-client` (a Cloudflare Access TCP tunnel). See `.miniflux.env.example`. |

---

## 🏗️ Architecture & Component Overview

```
                      ┌────────────────────────────────────────┐
                      │            Miniflux (Feed Fetcher)     │
                      │   - Optional HTTP egress proxy         │
                      │   - Flareproxy / Flaresolverr bypass   │
                      │   - WP-Adapter for WordPress feeds     │
                      └──────────────────┬─────────────────────┘
                                         │ Writes RSS entries
                                         ▼
                             ┌───────────────────────┐
                             │ PostgreSQL + pgvector │
                             │      (:5435/5432)     │
                             └───────────┬───────────┘
                                         │
                    ┌────────────────────┴────────────────────┐
                    │                                         │
                    ▼                                         ▼
      ┌───────────────────────────┐             ┌───────────────────────────┐
      │   Python AI Sidecar (:8001)│             │     Redis Cache (:6379)   │
      │   - FastEmbed Multilingual│             │  - ai_cache (60m TTL)     │
      │   - Cosine Clustering     │             │  - summary_cache (60m TTL)│
      │   - Category & Tag Naming │             │  - briefing_news (14h TTL)│
      └─────────────┬─────────────┘             └─────────────┬─────────────┘
                    │                                         │
                    └────────────────────┬────────────────────┘
                                         │
                                         ▼
                      ┌────────────────────────────────────────┐
                      │          Go Dashboard Server (:8080)   │
                      │  ├── Background ai_cache Refresher     │
                      │  ├── Grounded Summarizer (MiniMax/Gem) │
                      │  ├── Scheduled Briefing News Pregen    │
                      │  └── HTMX Web Dashboard & JSON APIs    │
                      └──────────────────┬─────────────────────┘
                                         │
                    ┌────────────────────┴────────────────────┐
                    │                                         │
                    ▼                                         ▼
      ┌───────────────────────────┐             ┌───────────────────────────┐
      │     Web Browser (HTMX)    │             │   audio-briefing (:8380)  │
      │  - Category filters       │             │  - GET /api/briefing-news │
      │  - Real-time search       │             │  - Speech-ready summaries │
      │  - Infinite scroll cards  │             └───────────────────────────┘
      │  - Streaming AI summaries │
      └───────────────────────────┘
```

---

## ✨ Features

### 1. Semantic Clustering & Topic Deduplication
- The Python `ai-service` (`FastAPI` + `FastEmbed` multilingual embeddings) groups related articles into unified story topics across categories using cosine-distance clustering, and generates descriptive Greek topic titles and auto-tags.
- A multi-tagged group is intentionally filed under every display category it belongs to; views that merge categories dedupe by `group_id`.

### 2. Grounded Anti-Hallucination Summarization
- Uses CommandCode (`MiniMaxAI/MiniMax-M3`) with automatic fallback to Google Gemini (`gemini-2.5-flash`).
- Extracts full article text from original sources, discards paywalled/stub articles, and prompts with strict source attribution.
- A topic-relevance gate drops articles whose stored embedding is far from the cluster centroid (lexical word-overlap fallback when embeddings are sparse) before MMR source selection, so a mis-clustered story cannot reach the summary.

### 3. Audio Briefing Pre-Generation Pipeline
- The producer side of the audio briefing pipeline (see `BRIEFING_NEWS_PLAN.md`).
- A background ticker wakes at `07:45` and `19:45` Europe/Athens time (15 minutes ahead of the `08:00`/`20:00` broadcast).
- Selects the top 20 topics (non-sports first, sports capped at 2 and placed last), summarizes each via bounded parallel workers (max 5 concurrent LLM calls), warms the `SummaryCache`, and commits the block to Redis.
- Groups that fail to summarize are dropped; if fewer than 5 of the selected topics produce a verified summary, generation fails and the previously stored block is left untouched, so the briefing never serves a truncated edition.

### 4. Resilient Multi-Tier Caching
- **`ai_cache`**: full clustered dashboard view, kept warm every 60 minutes and refreshed in the background (stale data is served while refreshing).
- **`article_cache`**: flat, ungrouped article list (2m TTL) used when the AI sidecar is unavailable.
- **`summary_cache`**: generated summaries, keyed by category + `group_id` (60m TTL).
- **`GroupMapping`**: persistent topic IDs across refresh cycles.
- **`briefing_news:<slot>`**: pre-generated briefing blocks (14h TTL by default).

### 5. High-Performance HTMX Dashboard
- Server-rendered HTML with HTMX 2.0, debounced search (`300ms`), infinite-scroll cards, dark/light theme, client-side read state, and SSE streaming summarization.

### 6. Read-Only JSON API
- `/api/v1/*` exposes feeds, articles and AI topic groups to external consumers (e.g. mobile apps), with a bearer token that does not require the dashboard's Basic Auth credentials.

---

## 🔌 HTTP API

### Authentication

Every route — including `/static/*` — is behind HTTP Basic Auth (`AUTH_USERNAME` / `AUTH_PASSWORD`). Unauthenticated requests get `401` with `WWW-Authenticate: Basic realm="Restricted"`.

Optionally set `API_TOKEN` to accept `Authorization: Bearer <token>` in addition. The bearer token is honoured **only for `GET` requests under `/api/`** (`/api/v1/*` and `/api/briefing-news`). Write routes (`POST /summarize*`, `POST /feedback*`, `POST /api/briefing-news/generate`) and all HTML routes always require Basic Auth. With `API_TOKEN` unset, bearer auth is disabled entirely.

```bash
# Bearer token (JSON API only)
curl -H "Authorization: Bearer $API_TOKEN" http://127.0.0.1:8080/api/v1/feeds

# Basic Auth (everything, including the dashboard and writes)
curl -u "$AUTH_USERNAME:$AUTH_PASSWORD" http://127.0.0.1:8080/
```

### Dashboard & HTMX routes (HTML)

These back the web UI. They return HTML, not JSON, and are not a stable public interface — use `/api/v1/*` for integrations. `GET /`, `/filter` and `/search` return only the articles fragment when the request carries `HX-Request: true`, and a full page otherwise.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/` | Front page: top story groups across all display categories, deduplicated by `group_id` |
| `GET` | `/filter?cat=<display category>` | One display category, groups sorted by size |
| `GET` | `/search?q=…` | `#tag` → hashtag chip view; `feed:<feed title>` → every article from that feed; anything else → semantic (embedding) search with substring fallback; empty `q` → front page |
| `GET` | `/stats` | Feed/category statistics fragment |
| `GET` | `/cards?cat=…&offset=N` | Infinite-scroll cards for one category (10 per page) |
| `GET` | `/cards?tag=…&offset=N` | Infinite-scroll cards for a hashtag (40 per page) |
| `GET` | `/card-body?gid=<group_id>&cat=<category>` | Lazily loaded card body: tags, summarize button, sources. `400` when `gid` is missing |
| `POST` | `/summarize` | Grounded topic summary fragment |
| `POST` | `/summarize/stream` | Same summary as an SSE stream |
| `POST` | `/summarize/more` | Longer/expanded summary, reusing the existing one |
| `POST` | `/summarize-article` | Summary for a single article |
| `POST` | `/feedback` | Negative feedback: rejects one article source of a group |
| `POST` | `/feedback/positive` | Positive feedback: endorses a whole topic group |

`POST` bodies are `application/x-www-form-urlencoded` (`r.FormValue`). Fields:

| Route | Form fields |
|---|---|
| `/summarize` | `category`, `group_id` |
| `/summarize/stream` | `category`, `group_id` |
| `/summarize/more` | `category`, `group_id`, `existing_summary` |
| `/summarize-article` | `article_id` |
| `/feedback` | `category`, `group_id`, `irrelevant_url` |
| `/feedback/positive` | `category`, `group_id` |

Responses are HTML snippets (feedback routes return a small confirmation `<span>`). `/summarize/stream` answers with `Content-Type: text/event-stream` and emits `event: status` (`{"message": …}`), `event: delta` (`{"delta": …}`), `event: reset` (`{}`) and a terminal `event: done` (`{"html": …}` or `{"error": …}`).

### Briefing JSON

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/briefing-news?slot=<slot>&limit=N` | Serves the stored block. `slot` is `morning` or `evening` (default `morning`); `limit` (alias `n`) trims the list *after* ordering; sports items are always capped at 2 and placed last. |
| `POST` | `/api/briefing-news/generate?slot=…&limit=N` | Regenerates the block synchronously (10-minute timeout). Idempotent; used for manual runs and cron triggers. Basic Auth only. |

Statuses: `400` invalid slot, `404` no block stored for that slot yet, `503` news-block store unavailable, `500` generation failure.

```json
{
  "slot": "morning",
  "generated": "2026-11-19T07:48:16.273590467+02:00",
  "items": [
    {
      "topic": "Τριμερής συνάντηση Τραμπ–Πούτιν–Σι στη Σεντζέν",
      "category": "Κόσμος",
      "summary_text": "Η ιστορική τριμερής συνάντηση διεξάγεται την Πέμπτη…"
    }
  ]
}
```

### Read-only JSON API (`/api/v1/*`)

Read-only, JSON only, no HTML fragments. Bearer-token accessible (see [Authentication](#authentication)).

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/v1/feeds` | Every Miniflux feed with its stored article count. Not paginated. |
| `GET` | `/api/v1/feeds/{id}` | One feed's metadata. |
| `GET` | `/api/v1/feeds/{id}/articles` | Articles of one feed, newest first. An unknown feed returns an empty page, not `404`. |
| `GET` | `/api/v1/articles` | Flat article list from the Miniflux database. |
| `GET` | `/api/v1/categories` | AI display categories with article and group counts. |
| `GET` | `/api/v1/categories/{cat}/groups` | Topic groups under one display category, largest first. |
| `GET` | `/api/v1/groups` | All topic groups, deduplicated by `group_id`, largest first. |
| `GET` | `/api/v1/groups/{group_id}` | One topic group with all of its articles. |

**Common query parameters**

| Parameter | Applies to | Behaviour |
|---|---|---|
| `limit` | article and group list routes | Default `50`, clamped to `200`; values ≤ 0 fall back to the default |
| `offset` | article and group list routes | Default `0` |
| `since` | article routes | A Go duration (`24h`, `6h30m`) or an RFC3339 timestamp. Unparseable values are ignored (no time floor) |
| `q` | `/api/v1/articles` | Case- and accent-insensitive substring over article title + feed title |
| `feed_id` | `/api/v1/articles` | Restrict to one Miniflux feed |
| `category` | `/api/v1/articles` | Miniflux *feed* category (`news`, `sports`, `music`, `international`, …) |
| `cat` | `/api/v1/groups` | AI *display* category (Greek, e.g. `Ελλάδα`), matched case-insensitively |

**Response shapes**

`total` is always the number of matching records *ignoring* pagination, so `offset + len(page) < total` means another page exists.

```json
// GET /api/v1/feeds
{ "total": 205, "feeds": [
  { "id": 94, "title": "Al Jazeera – Breaking News", "site_url": "https://www.aljazeera.com",
    "category": "international", "disabled": false, "article_count": 1397 } ] }

// GET /api/v1/feeds/{id}/articles  and  GET /api/v1/articles
{ "total": 62737, "limit": 2, "offset": 0, "articles": [
  { "id": 2247647, "title": "Στόχος κυβερνοεπίθεσης έγινε η Ευρωπαϊκή Επιτροπή",
    "url": "https://www.ertnews.gr/…", "published_at": "2026-11-19T08:32:36Z",
    "feed": "ertnews.gr", "category": "news" } ] }

// GET /api/v1/categories
{ "generated": "11:12:51", "total": 17, "categories": [
  { "category": "Ελλάδα", "count": 131, "group_count": 131 } ] }

// GET /api/v1/groups  (and /categories/{cat}/groups)
{ "generated": "11:12:51", "total": 522, "limit": 2, "offset": 0, "groups": [
  { "group_id": "258f99ab02ed", "topic": "Τραμπ, Σι και Πούτιν στη Σεντζέν", "category": "Κόσμος",
    "count": 42, "tags": ["Κόσμος", "Τραμπ"], "articles": [ /* article objects */ ] } ] }
```

`generated` is the `HH:MM:SS` timestamp of the clustered cache that served the request. `count` is the number of articles in the group; `tags` is omitted when empty.

**Errors** — every error is `{"error": "…"}`: `400` invalid path/query (`invalid feed id`), `401` missing or wrong credentials, `404` unknown feed/category/group (`feed not found`, `category not found`, `group not found`), `500` query or cache failure (`failed to query articles`, `data error`), `503` database unavailable (`database unavailable`).

**Two category namespaces** — `/api/v1/articles?category=` and every `feed.category` use Miniflux's *feed* categories (`news`, `sports`, `music`, …), while `/api/v1/categories`, `/api/v1/groups?cat=` and `/api/v1/groups/{id}` use the AI *display* categories (Greek). Feed titles are the raw `feeds.title` value. Article `content` is deliberately not exposed.

```bash
curl -s -H "Authorization: Bearer $API_TOKEN" \
  "http://127.0.0.1:8080/api/v1/feeds/264/articles?limit=5"
```

---

## ⚙️ Configuration

### Dashboard / Go server

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP server port |
| `AUTH_USERNAME` | *(required)* | Basic auth username |
| `AUTH_PASSWORD` | *(required)* | Basic auth password |
| `API_TOKEN` | *(empty = disabled)* | Bearer token accepted on `GET /api/*` |
| `MF_DB_HOST` | `127.0.0.1` | Miniflux PostgreSQL host |
| `MF_DB_PORT` | `5433` | Miniflux PostgreSQL port (Compose publishes `5435`) |
| `MF_DB_NAME` | `miniflux` | Database name |
| `MF_DB_USER` | `external_reader` | PostgreSQL username |
| `MF_DB_PASSWORD` | *(required)* | PostgreSQL password |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis address |
| `AI_SERVICE_URL` | `http://127.0.0.1:8001` | Clustering sidecar URL |
| `COMMAND_CODE_API_KEY` | *(optional)* | CommandCode key for summaries and topic naming |
| `COMMAND_CODE_MODEL` | `MiniMaxAI/MiniMax-M3` | CommandCode model |
| `GEMINI_API_KEY` | *(optional)* | Gemini key (fallback LLM) |
| `BRIEFING_PREGEN_N` | `20` | Topics to pre-summarize per slot |
| `BRIEFING_PREGEN_MORNING` | `07:45` | Morning pregen time (Athens) |
| `BRIEFING_PREGEN_EVENING` | `19:45` | Evening pregen time (Athens) |
| `BRIEFING_NEWS_BLOCK_TTL_HOURS` | `14` | Stored news block TTL |

With `AUTH_USERNAME`/`AUTH_PASSWORD` empty no browser request can authenticate — always set both.

### AI sidecar (`ai/main.py`)

| Variable | Default | Description |
|---|---|---|
| `COMMAND_CODE_API_KEY` | *(empty)* | LLM key for topic naming |
| `COMMAND_CODE_MODEL` | `MiniMaxAI/MiniMax-M3` | Naming model |
| `GEMINI_API_KEY` | *(empty)* | Gemini fallback key |
| `AI_DISTANCE_THRESHOLD` | `0.45` | Agglomerative cosine-distance threshold (`average` linkage). Lower = tighter, more groups; the deployed instance runs `0.50`. |

### Miniflux & egress proxy (`.miniflux.env`)

Copy `.miniflux.env.example` → `.miniflux.env`. It carries `POSTGRES_PASSWORD`, `ADMIN_PASSWORD`, `DATABASE_URL`, `PGPASSWORD`, the Cloudflare Access service token (`CF_CLIENT_ID`, `CF_CLIENT_SECRET`, `CF_ACCESS_HOSTNAME`), `HTTP_CLIENT_TIMEOUT`, and the entry-retention knobs `CLEANUP_ARCHIVE_UNREAD_DAYS` (default `15`) / `CLEANUP_ARCHIVE_READ_DAYS` (default `2`).

Miniflux is pointed at the egress proxy via `PROXY_URL=http://cloudflared-client:8888`; `POLLING_FREQUENCY=15` (minutes) and `POLLING_PARSING_ERROR_LIMIT=0` (keep polling failing feeds) are set in `docker-compose.yml`.

---

## 🚀 Deployment & Operations

### Services

`docker compose` runs 11 services:

| Service | Port (host → container) | Role |
|---|---|---|
| `dashboard` | `8080` (host network) | This Go server: UI, summarization, briefing pregen, JSON API |
| `ai-service` | `8001 → 8000` | Clustering + naming sidecar |
| `redis` | `127.0.0.1:6379` | Cache and group mapping |
| `db` | `127.0.0.1:5435 → 5432` | PostgreSQL + pgvector (Miniflux schema + embeddings) |
| `miniflux` | `9292 → 8080` | Feed fetcher and reader |
| `wp-adapter` | `8008 → 8000` | WordPress-feed adapter for sites without usable RSS |
| `flareproxy` | `8005 → 8000` | Proxy to Flaresolverr for Cloudflare-protected feeds |
| `flaresolverr` | internal `8191` | Browser challenge solver |
| `cloudflared-client` | internal `8888` | Cloudflare Access egress tunnel used as HTTP proxy |
| `cloudflared-newspaper` | host network | Public Cloudflare tunnel for the dashboard |
| `db-cleanup` | — | Daily cron: prune read entries >2 days, unread >15 days, keep 5000 entries/feed, `VACUUM FULL` |

```bash
cp .env.example .env                     # dashboard/AI/db secrets
cp .miniflux.env.example .miniflux.env   # Miniflux/Cloudflare secrets
docker compose up -d
```

The `cloudflared-client` egress tunnel is optional. If feeds are fetched directly, remove that service (and `flaresolverr`/`flareproxy`) and drop `PROXY_URL`/`PROXY`.

### Common commands

```bash
docker compose up -d                                   # start everything
docker compose build dashboard && docker compose up -d dashboard   # after Go/template/CSS changes
docker compose build ai-service && docker compose up -d ai-service # after ai/ changes
docker logs miniflux-dashboard --tail 50
docker logs miniflux-ai-service --tail 50

# Cache inspection / recovery
docker exec dashboard-redis redis-cli flushall                       # force re-fetch + re-cluster
docker exec dashboard-redis redis-cli GET ai_cache | jq '.data.categories[].category'
docker exec dashboard-redis redis-cli GET ai_cache | jq '[.data.categories[].groups[].count] | sort | reverse | .[0:10]'

# Smoke-test the API
curl -s -u "$AUTH_USERNAME:$AUTH_PASSWORD" localhost:8080/api/v1/categories | jq '.total'
curl -s -u "$AUTH_USERNAME:$AUTH_PASSWORD" 'localhost:8080/api/briefing-news?slot=morning&limit=3' | jq '.items|length'
```

Templates, CSS and JS are embedded into the Go binary, so any UI change requires rebuilding the `dashboard` image — editing files in a running container changes nothing.

### Scheduled work

| When | What |
|---|---|
| every 60 min | Background `ai_cache` refresh; the first refresh starts 90s after boot so the AI model can load |
| `07:45` / `19:45` Athens | Briefing news pre-generation (`BRIEFING_PREGEN_*`, `BRIEFING_PREGEN_N`) |
| daily (container cron) | `db-cleanup` retention pruning |

### Local development

```bash
# 1. Python AI sidecar
cd ai
python3 -m venv venv && source venv/bin/activate
pip install -r requirements.txt
uvicorn main:app --port 8001

# 2. Go server (repo root)
export AUTH_USERNAME="admin"
export AUTH_PASSWORD="password"
export COMMAND_CODE_API_KEY="your-api-key"
export GEMINI_API_KEY="your-gemini-key"
go run cmd/server/main.go
```

---

## 🧪 Testing

```bash
# Go: handlers, cache, summarize (including the off-topic gate), config
go test ./...

# Python: category decision rules (dependency-free, 24 cases, runs without models)
python3 -m unittest discover -s ai -p 'test_categorization.py'
```

Requires a running stack:

```bash
python3 ai/test_clustering.py            # end-to-end clustering assertions against http://127.0.0.1:8001/cluster
```

Model-local tuning scripts (run inside the AI container so the fastembed cache is warm):

```bash
docker exec -i miniflux-ai-service python3 < ai/test_threshold.py   # cosine distances for a feedback case
```

`ai/test_clustering_thresholds.py`, `ai/test_embeddings_clustering.py`, `ai/debug_distance.py`, `ai/test_threshold2.py` and `ai/backfill_embeddings.py` are analysis utilities: they load the embedding model and/or connect to Postgres directly, and they print diagnostics rather than asserting.

`scripts/estimate_cost.go` reads `ai_cache` straight from Redis and estimates per-run LLM token cost; `scripts/estimate_cost.py` does the same from a dumped `/tmp/ai_cache.json`.

---

## 🩺 Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Every group has `count: 1`, categories read `ειδήσεις Κόσμος`-style instead of `Ελλάδα`/`Κόσμος` | The AI sidecar is down or busy, so the dashboard is serving the flat `article_cache` fallback. Check `docker logs miniflux-ai-service`, then `docker exec dashboard-redis redis-cli flushall`. |
| AI container killed / OOM | Embedding a large batch at once. `ai/main.py` embeds with `batch_size=16`; do not increase it. |
| Clusters too broad or too fragmented | Tune `AI_DISTANCE_THRESHOLD` (`0.45` default, deployment uses `0.50`, `average` linkage) and restart `ai-service`; re-run the threshold scripts for a concrete feedback case. |
| `GET /api/briefing-news` returns `404` | No block stored for that slot yet (before the first pregen, after TTL expiry, or after a Redis flush). Trigger `POST /api/briefing-news/generate?slot=morning`. |
| `POST /api/briefing-news/generate` returns `401` with a bearer token | Bearer auth covers `GET /api/*` only; generation needs Basic Auth. |
| UI change not visible after editing files | Templates are embedded at build time: `docker compose build dashboard && docker compose up -d dashboard`. |
| Dashboard shows stale stories | `ai_cache` is served while refreshing (60m TTL). Flush Redis to force a rebuild. |
| `503 database unavailable` on `/api/v1/*` | `MF_DB_*` points at the wrong database/port (Compose exposes Postgres on `5435`, not the `5433` default). |

---

## 📄 License

MIT License. See repository for details.
