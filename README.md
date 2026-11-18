# Daily News Aggregator 📰🤖

An intelligent, real-time Greek & international news aggregation and summarization platform. Powered by Go 1.24, HTMX 2.0, PostgreSQL (Miniflux schema + pgvector), Redis 7, a Python multilingual semantic clustering sidecar, and LLM-grounded summarization (CommandCode / Gemini).

Beyond serving an interactive live news dashboard, it functions as the **grounded news producer** for the audio briefing pipeline.

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
| [`audio-briefing`](https://github.com/panospet/audio-briefing) | `8080` HTTP (Basic Auth) | This service pre-generates speech-ready Greek news summaries twice daily (`07:45` & `19:45` Athens) and exposes them at `GET /api/briefing-news?slot=morning\|evening`, with an on-demand fallback `POST /api/briefing-news/generate`. |
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

### 2. Grounded Anti-Hallucination Summarization
- Uses CommandCode (`MiniMaxAI/MiniMax-M3`) with automatic fallback to Google Gemini (`gemini-2.5-flash`).
- Extracts full article text from original sources, discards paywalled/stub articles, prompts with strict source attribution, and verifies claims to eliminate hallucinated facts.
- Strips inline citation brackets `[1]` for plain, speech-ready text.

### 3. Audio Briefing Pre-Generation Pipeline
- The producer side of the audio briefing pipeline (see `BRIEFING_NEWS_PLAN.md`).
- A background ticker wakes at `07:45` and `19:45` Europe/Athens time (15 minutes ahead of the `08:00`/`20:00` broadcast).
- Selects the top 20 topics, summarizes each via bounded parallel workers (max 5 concurrent LLM calls), warms the `SummaryCache`, and commits the block to Redis.
- On total LLM failure, falls back to a titles-only block so the briefing never halts.

### 4. Resilient Multi-Tier Caching
- **`ai_cache`**: full clustered dashboard view, kept warm every 60 minutes.
- **`summary_cache`**: generated summaries.
- **`GroupMapping`**: persistent topic IDs across refresh cycles.
- **`news_block`**: pre-generated briefing blocks in Redis.

### 5. High-Performance HTMX Dashboard
- Server-rendered HTML with HTMX 2.0, debounced search, live stats every 60s, dark/light mode, and SSE/chunked streaming summarization.

---

## 🔌 Endpoints & API Reference

All endpoints are protected by HTTP Basic Auth (`AUTH_USERNAME` / `AUTH_PASSWORD`) and gzip compression.

| Method | Path | Description |
|---|---|---|
| `GET` | `/` | Dashboard homepage |
| `GET` | `/filter?cat=...` | Filter stories by category |
| `GET` | `/search?q=...` | Live search |
| `GET` | `/stats` | Feed statistics fragment |
| `GET` | `/cards?cat=...&offset=...` | Paginated cards |
| `GET` | `/card-body?cat=...&id=...` | Full article list for a topic |
| `POST` | `/summarize` | Grounded AI summary |
| `POST` | `/summarize/stream` | Streamed summary |
| `POST` | `/summarize/more` | Expanded summary |
| `POST` | `/summarize-article` | Summarize a single URL |
| `POST` | `/feedback` | Summary feedback |
| `GET` | `/api/briefing-news?slot=morning\|evening` | Speech-ready news block JSON |
| `POST` | `/api/briefing-news/generate?slot=...` | (Re)generate the news block |

Example `GET /api/briefing-news?slot=morning`:

```json
{
  "slot": "morning",
  "generated": "2026-11-18T07:45:03+02:00",
  "items": [
    {
      "topic": "Συνάντηση κορυφής στις Βρυξέλλες για την ενέργεια",
      "category": "Κόσμος",
      "summary_text": "Οι Ευρωπαίοι ηγέτες συναντήθηκαν σήμερα στις Βρυξέλλες…"
    }
  ]
}
```

---

## ⚙️ Configuration

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP server port |
| `AUTH_USERNAME` | *(required)* | Basic auth username |
| `AUTH_PASSWORD` | *(required)* | Basic auth password |
| `MF_DB_HOST` | `127.0.0.1` | Miniflux PostgreSQL host |
| `MF_DB_PORT` | `5433` | Miniflux PostgreSQL port |
| `MF_DB_NAME` | `miniflux` | Database name |
| `MF_DB_USER` | `external_reader` | PostgreSQL username |
| `MF_DB_PASSWORD` | *(required)* | PostgreSQL password |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis address |
| `AI_SERVICE_URL` | `http://127.0.0.1:8001` | Clustering sidecar URL |
| `COMMAND_CODE_API_KEY` | *(optional)* | CommandCode / MiniMax key |
| `COMMAND_CODE_MODEL` | `MiniMaxAI/MiniMax-M3` | CommandCode model |
| `GEMINI_API_KEY` | *(optional)* | Gemini key (fallback) |
| `BRIEFING_PREGEN_N` | `20` | Topics to pre-summarize per slot |
| `BRIEFING_PREGEN_MORNING` | `07:45` | Morning pregen time (Athens) |
| `BRIEFING_PREGEN_EVENING` | `19:45` | Evening pregen time (Athens) |
| `BRIEFING_NEWS_BLOCK_TTL_HOURS` | `14` | Cached news block TTL |

Miniflux and Cloudflare Access settings live in [`.miniflux.env.example`](.miniflux.env.example).

---

## 🚀 Deployment & Operations

### Docker Compose
Runs the whole news stack (Go dashboard, Python AI service, Redis, Miniflux, Postgres pgvector, Cloudflare proxy tunnel, and Flareproxy):

```bash
cp .env.example .env               # fill in dashboard/AI/db secrets
cp .miniflux.env.example .miniflux.env   # fill in Miniflux/Cloudflare secrets
docker compose up -d
```

> The `cloudflared-client` egress proxy tunnel is optional. If you fetch feeds
> directly (no proxy), remove or scale down that service and drop the
> `PROXY_URL`/`PROXY` settings.

### Local Development
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
go test ./... -v
```

---

## 📄 License

MIT License. See repository for details.
