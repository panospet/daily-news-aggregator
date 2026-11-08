# Daily News Aggregator 📰🤖

An intelligent, real-time Greek & international news aggregation and summarization platform. Powered by Go 1.24, HTMX 2.0, PostgreSQL (Miniflux schema + pgvector), Redis 7, a Python multilingual semantic clustering sidecar, and LLM-grounded summarization (CommandCode / Gemini).

Beyond serving an interactive live news dashboard, it functions as the **grounded news producer** for the personal daily audio briefing pipeline.

---

## 🌐 The 4-Service Ecosystem Context

This repository is one of four interconnected services powering personal media and automated news delivery:

```
                      ┌────────────────────────────────────────────────────────┐
                      │                      media-stack                       │
                      │  ┌──────────────┐     ┌──────────────┐                 │
                      │  │  Navidrome   │     │  Tinyproxy   │                 │
                      │  │  (Port 4533) │     │  (Port 8888) │                 │
                      │  └──────┬───────┘     └──────┬───────┘                 │
                      │         │ Subsonic API       │ Tunnel via CF Access    │
                      │         │                    ▼ (tinyproxy.panos.pet)   │
                      │         │             ┌──────────────────────────────┐ │
                      │         │             │ daily-news-aggregator (Here) │ │
                      │         │             │  (Miniflux + AI Clustering + │ │
                      │         │             │   Grounded Summaries :8080)  │ │
                      │         │             └──────────────┬───────────────┘ │
                      │         │                            │                 │
                      │         │                            │ GET /api/       │
                      │         │                            │ briefing-news   │
                      │         ▼                            ▼                 │
                      │  ┌──────────────────────────────────────────────┐      │
                      │  │                audio-briefing                │      │
                      │  │       (Pipeline Orchestrator & Player :8380) │      │
                      │  └──────────────────────┬───────────────────────┘      │
                      │                         │                              │
                      │                         │ POST /synthesize             │
                      │                         ▼                              │
                      │  ┌──────────────────────────────────────────────┐      │
                      │  │                voice-service                 │      │
                      │  │    (Neural Greek TTS & Web Scraper :8002)    │      │
                      │  └──────────────────────────────────────────────┘      │
                      │                         │                              │
                      │                         ▼                              │
                      │             newsbrief.panos.pet (Cloudflare Tunnel)    │
                      └────────────────────────────────────────────────────────┘
```

### Role & Inter-Service Communications

| Linked Service | Port / Protocol | Connection Details |
|---|---|---|
| **[`media-stack`](https://github.com/panospet/media-stack) (Tinyproxy)** | Port `8888` via Cloudflare Access (`tinyproxy.panos.pet`) | Miniflux and Flaresolverr route RSS feed downloads through `media-stack`'s Tinyproxy using a Cloudflare Access secure TCP client container (`cloudflared-client`). |
| **[`audio-briefing`](https://github.com/panospet/audio-briefing)** | Port `8080` HTTP (Basic Auth) | `daily-news-aggregator` pre-generates speech-ready Greek news summaries twice daily (`07:45` & `19:45` Athens time) and exposes them at `GET /api/briefing-news?slot=morning\|evening`. Also supports on-demand trigger `POST /api/briefing-news/generate`. |
| **[`voice-service`](https://github.com/panospet/voice-service)** | Port `8002` HTTP | Available as the speech synthesis engine for web article narration ("read aloud"). |

---

## 🏗️ Architecture & Component Overview

```
                      ┌────────────────────────────────────────┐
                      │            Miniflux (Feed Fetcher)     │
                      │   - Tunnels via tinyproxy.panos.pet    │
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
- Multiple media outlets constantly report on the same event. Rather than displaying 20 redundant articles, the Python `ai-service` (`FastAPI` + `FastEmbed` multilingual embeddings) groups related articles into unified story topics across categories using cosine distance clustering.
- Generates descriptive Greek topic titles and auto-tags topics (e.g. politics, sports, international).

### 2. Grounded Anti-Hallucination Summarization
- Leverages CommandCode (`MiniMaxAI/MiniMax-M3`) with automatic fallback to Google Gemini (`gemini-2.5-flash`).
- **Strict Verification**: Extracts full article text from original web sources, discards paywalled/stub articles, prompts with strict source attribution, and verifies claims to eliminate hallucinated facts.
- **Citation Stripping**: Cleans inline citation brackets `[1]` to provide plain, speech-ready text for audio synthesis.

### 3. Audio Briefing Pre-Generation Pipeline
- Implements the producer side of the personal daily audio briefing pipeline (detailed in `BRIEFING_NEWS_PLAN.md`).
- A background ticker wakes at `07:45` and `19:45` Europe/Athens time (15 minutes ahead of the `08:00`/`20:00` audio broadcast).
- Selects the top 20 topics from the homepage ranking, summarizes each via bounded parallel workers (max 5 concurrent LLM calls), warms the `SummaryCache`, and commits the block to Redis.
- If upstream LLMs fail completely, generates a graceful fallback titles-only block so the audio briefing never halts.

### 4. Resilient Multi-Tier Caching
- **`ai_cache`**: Caches the full clustered dashboard view in Redis. A background refresh worker (`StartBackgroundRefresh`) keeps the cache permanently warm every 60 minutes, ensuring zero cold-start latency for visitors.
- **`summary_cache`**: Caches generated article and topic summaries so repeated requests are instantaneous.
- **`GroupMapping`**: Preserves persistent IDs for topics across refresh cycles, preventing layout jumps.
- **`news_block`**: Stores pre-generated briefing blocks in Redis under `briefing_news:morning` and `briefing_news:evening`.

### 5. High-Performance HTMX Dashboard
- Server-rendered HTML with HTMX 2.0: Instant tab switching, debounced active search across titles and feeds, live statistics updating silently every 60s, dark/light mode toggle, and SSE/chunked streaming summarization.

---

## 🔌 Endpoints & API Reference

All endpoints are protected by HTTP Basic Authentication (`AUTH_USERNAME` / `AUTH_PASSWORD`) and Gzip compression.

### Dashboard & HTMX
| Method | Path | Description |
|---|---|---|
| `GET` | `/` | Main dashboard homepage (or refreshes article grid) |
| `GET` | `/filter?cat=...` | Filters stories by category tab (`all`, `greece`, `world`, `tech`, `music`, etc.) |
| `GET` | `/search?q=...` | Debounced live search across article titles and publishers |
| `GET` | `/stats` | Returns real-time feed statistics fragment for auto-refresh |
| `GET` | `/cards?cat=...&offset=...` | Paginated cards for infinite scrolling |
| `GET` | `/card-body?cat=...&id=...` | Expands and loads the full article list for a topic card |

### AI Summaries
| Method | Path | Description |
|---|---|---|
| `POST` | `/summarize` | Generates a grounded AI summary for a topic group |
| `POST` | `/summarize/stream` | Streams AI summary chunks for real-time typewriter UI |
| `POST` | `/summarize/more` | Generates an expanded comprehensive summary |
| `POST` | `/summarize-article` | Summarizes a single article URL |
| `POST` | `/feedback` | Submits feedback on summary quality |

### Audio Briefing News Blocks
| Method | Path | Description |
|---|---|---|
| `GET` | `/api/briefing-news?slot=morning\|evening` | Returns the pre-generated speech-ready news block JSON for the requested slot. |
| `POST`| `/api/briefing-news/generate?slot=morning\|evening` | Synchronously generates (or re-generates) and stores the news block, returning the result. |

#### Example Response: `GET /api/briefing-news?slot=morning`
```json
{
  "slot": "morning",
  "generated": "2026-11-08T07:45:03+02:00",
  "items": [
    {
      "topic": "Συνάντηση κορυφής στις Βρυξέλλες για την ενέργεια",
      "category": "Κόσμος",
      "summary_text": "Οι Ευρωπαίοι ηγέτες συναντήθηκαν σήμερα στις Βρυξέλλες για να συζητήσουν νέα μέτρα σταθεροποίησης των τιμών ενέργειας. Η συμφωνία περιλαμβάνει κοινές προμήθειες φυσικού αερίου και ενίσχυση των ανανεώσιμων πηγών."
    },
    {
      "topic": "Νέες ρυθμίσεις στις αστικές συγκοινωνίες",
      "category": "Ελλάδα",
      "summary_text": "Από τη Δευτέρα τίθεται σε εφαρμογή το νέο ηλεκτρονικό εισιτήριο και η επέκταση δρομολογίων λεωφορείων προς τα νότια προάστια."
    }
  ]
}
```

---

## ⚙️ Configuration & Environment Variables

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP server listening port |
| `AUTH_USERNAME` | *(required)* | HTTP Basic authentication username |
| `AUTH_PASSWORD` | *(required)* | HTTP Basic authentication password |
| `MF_DB_HOST` | `127.0.0.1` | Miniflux PostgreSQL database host |
| `MF_DB_PORT` | `5433` | Miniflux PostgreSQL database port |
| `MF_DB_NAME` | `miniflux` | Miniflux database name |
| `MF_DB_USER` | `external_reader` | PostgreSQL username |
| `MF_DB_PASSWORD` | *(required)* | PostgreSQL password |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis host:port address |
| `AI_SERVICE_URL` | `http://127.0.0.1:8001`| Base URL for the Python clustering sidecar service |
| `COMMAND_CODE_API_KEY` | *(optional)* | API key for CommandCode / MiniMax LLM provider |
| `COMMAND_CODE_MODEL` | `MiniMaxAI/MiniMax-M3` | Model name for CommandCode |
| `GEMINI_API_KEY` | *(optional)* | Google Gemini API key (primary fallback summarizer) |
| `BRIEFING_PREGEN_N` | `20` | Number of top topic groups to pre-summarize per slot |
| `BRIEFING_PREGEN_MORNING`| `07:45` | Athens local time (HH:MM) to pre-generate morning block |
| `BRIEFING_PREGEN_EVENING`| `19:45` | Athens local time (HH:MM) to pre-generate evening block |
| `BRIEFING_NEWS_BLOCK_TTL_HOURS`| `14` | Expiration time for cached briefing news blocks |

---

## 🚀 Deployment & Operations

### Docker Compose
Run the entire news stack (Go dashboard, Python AI service, Redis, Miniflux, Postgres pgvector, Cloudflare proxy tunnel, and Flareproxy):
```bash
docker compose up -d
```

### Local Development
```bash
# 1. Start Python AI sidecar
cd ai
python3 -m venv venv && source venv/bin/activate
pip install -r requirements.txt
uvicorn main:app --port 8001

# 2. Start Go server (in main repo root)
export AUTH_USERNAME="admin"
export AUTH_PASSWORD="password"
export COMMAND_CODE_API_KEY="your-api-key"
export GEMINI_API_KEY="your-gemini-key"
go run cmd/server/main.go
```

---

## 🧪 Testing

Run all Go unit and integration tests:
```bash
go test ./... -v
```

---

## 📄 License

MIT License. See repository for details.
