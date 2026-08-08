# HTMX Daily News - Project Progress & AI Knowledge

## Goal
A smart news aggregator that pulls feeds (via Miniflux), clusters them using an AI embedding model (fastembed + sentence-transformers/paraphrase-multilingual-mpnet-base-v2), and displays them grouped by topic in a beautiful, premium HTMX dashboard.

## Completed Features

### 1. AI Clustering & Categorization
- We migrated the monolithic python scripts (`cluster_articles.py`) into a dedicated `miniflux-ai-service` container in the `ai/` folder.
- The AI service now uses `sentence-transformers/paraphrase-multilingual-mpnet-base-v2` because `intfloat/multilingual-e5-small` failed.
- We embed the title + the first 50 words of each article to significantly improve semantic matching accuracy.
- We run `fastembed` with a restricted `batch_size=16` to prevent the Uvicorn container from running out of memory and crashing on massive data sets.
- Articles are intelligently mapped into 6 distinct categories based on language and content (e.g. `Ελληνική ειδησεογραφία`, `Ξένη ειδησεογραφία`, `Νέες μουσικές κυκλοφορίες`).

### 2. Dashboard UI & UX
- Completely overhauled the dashboard with a premium, state-of-the-art UI.
- Upgraded typography to `Inter` for a sleek professional look.
- Added animated, heavily blurred glassmorphism to the sidebar (`backdrop-filter: blur(24px)`).
- Included animated mesh gradient backgrounds for depth.
- Refined micro-animations, hover tilt states, and animated gradient underlines for headers.

### 3. Data & Caching
- Added robust dual-caching in Redis: 
  - `article_cache`: The raw flat structure served instantly as a fallback.
  - `ai_cache`: The fully clustered structure served once the AI service completes processing.
- The Go backend safely falls back to `article_cache` if the AI service goes offline or is busy downloading models.

### 4. Feedback Loop (Positive & Negative)
- **Negative Feedback**: A "thumbs down" button exists on each article source within an expanded group.
  - Clicking it silently triggers a POST request to `/feedback`, hiding the positive button.
  - Logs the entire group cluster + the specific rejected article to a `negative_feedback` Postgres table.
- **Positive Feedback**: A "thumbs up" button exists next to the group title (when expanded).
  - Clicking it silently triggers a POST request to `/feedback/positive`, hiding the negative buttons.
  - Logs the entire group cluster to a `positive_feedback` Postgres table.
- **Handling Feedback (Agent Protocol)**: When a user reports a feedback entry (positive or negative), the agent MUST:
  1. Fetch the latest feedback from the DB (`positive_feedback` or `negative_feedback`).
  2. Use this as a testcase to evaluate clustering distance/threshold logic.
  3. Write a temporary python script (e.g. `ai/test_threshold.py`) that mimics the distance logic in `ai/main.py` using `fastembed` for the affected articles.
  4. Run the script inside the container (`docker exec -i miniflux-ai-service python3 < ai/test_threshold.py`) to compute the exact cosine distances.
  5. Identify the correct `AI_DISTANCE_THRESHOLD` required to separate/group them properly and inform the user.

### 5. Mark as Read
- A group is marked as "read" when the user **both expands it AND requests a summary** (via the "✨ Summarize Topic" button).
- Read groups appear grayed out (reduced opacity, desaturated via CSS `grayscale()` filter) with a small ✓ badge on the expand icon.
- A "📬 Σήμανση ως μη αναγνωσμένο" (Mark Unread) button appears on read groups to revert them.
- Read state is stored in `localStorage` keyed by `groupID:articleCount`, so if a new article is added to the group (changing the count), the group automatically becomes unread again.
- Old read entries (>24 hours) are automatically cleaned up from localStorage.
- Read state is restored on page load, HTMX partial swaps, and filter/search navigation.
- Implementation is entirely client-side (no backend changes needed).

## Current State & Next Steps
- The system is fully operational.
- The AI service performs perfectly with no OOM errors.
- The frontend is polished and ready. 
- Future work: Consider training or fine-tuning models based on the gathered `negative_feedback` data.

---

## Architecture Overview

```
┌──────────────┐     ┌──────────────────┐     ┌──────────────┐
│   Browser    │────▶│  Go Dashboard    │────▶│  Postgres DB │
│  (HTMX)     │◀────│  :8080           │◀────│  :5435       │
└──────────────┘     │  (cmd/server/)   │     │  (Miniflux)  │
                     └────────┬─────────┘     └──────────────┘
                              │
                     ┌────────▼─────────┐     ┌──────────────┐
                     │  Python AI Svc   │     │    Redis     │
                     │  :8001→:8000     │     │    :6379     │
                     │  (ai/main.py)    │     │  (caching)   │
                     └──────────────────┘     └──────────────┘
```

### Request Flow
1. Browser hits Go server at `:8080`
2. Go checks Redis for `ai_cache` (clustered data, 60min TTL)
3. If stale/missing → Go fetches articles from Postgres (last 12h), POSTs them to Python AI service at `:8001`
4. Python AI service embeds articles with fastembed, clusters with AgglomerativeClustering, names groups via Gemini
5. Go caches result in Redis as `ai_cache`, renders HTMX templates
6. If AI service is down → Go falls back to flat `article_cache` (2min TTL)

---

## File Map

### Go Backend (`cmd/`, `internal/`)
| File | Purpose |
|------|---------|
| `cmd/server/main.go` | Entrypoint. Wires config, DB, Redis, templates, grouping service, and HTTP server |
| `internal/config/config.go` | Env-based config (`MF_DB_*`, `PORT`, `GEMINI_API_KEY`). Also has `CatDisplayName` map (old flat category names) |
| `internal/database/database.go` | Opens Postgres, creates `negative_feedback` table, `QueryArticles()` fetches last N hours, `SaveNegativeFeedback()` |
| `internal/models/models.go` | All data structs: `Article`, `CategoryGroup` (flat), `AIGroup`, `AICategoryGroup`, `AIDashboardData` (clustered) |
| `internal/cache/cache.go` | Redis dual-cache: `ArticleCache` (flat fallback, 2min), `AICache` (clustered, 60min, background refresh). Also `FetchFresh()` wiring function |
| `internal/handler/handler.go` | HTTP routes: `GET /` (Home), `GET /filter`, `GET /search`, `GET /stats`, `POST /summarize`, `POST /feedback` |
| `internal/handler/templates.go` | Template loading from embedded FS |
| `internal/grouping/grouping.go` | `GroupArticles()` calls Python AI service. Falls back to flat grouping. Also has legacy Gemini-based grouping methods |
| `internal/gemini/` | Gemini API client (used for summarization and legacy grouping prompts) |
| `internal/summarize/` | AI topic summarization via Gemini |
| `embed.go` | Embeds `static/` and `templates/` into the Go binary |

### Python AI Service (`ai/`)
| File | Purpose |
|------|---------|
| `ai/main.py` | FastAPI app. Loads `paraphrase-multilingual-mpnet-base-v2` on startup. Exposes `POST /cluster`. Category assignment + AgglomerativeClustering + Gemini topic naming |
| `ai/Dockerfile` | Python 3.11 slim image, installs deps, runs uvicorn on `:8000` |
| `ai/requirements.txt` | fastembed, fastapi, uvicorn, scikit-learn, numpy, requests |
| `ai/test_clustering.py` | Integration tests against the running AI service (hit `http://127.0.0.1:8001/cluster`) |

### Frontend (`templates/`, `static/`)
| File | Purpose |
|------|---------|
| `templates/base.tmpl` | Main HTML layout: sidebar with categories, search bar, mesh-glow background, Inter font |
| `templates/partials/articles.tmpl` | Article cards with expand/collapse, feedback thumbs-down buttons, source pills |
| `templates/partials/stats.tmpl` | Stats bar (article count) |
| `static/style.css` | Full CSS: design tokens (CSS vars), glassmorphism sidebar, mesh gradient animations, card hover effects, responsive breakpoints |
| `static/htmx.min.js` | HTMX library (vendored) |

### Infrastructure
| File | Purpose |
|------|---------|
| `docker-compose.yml` | All services: dashboard, ai-service, redis, miniflux, postgres, cloudflared, flaresolverr, flareproxy, db-cleanup |
| `Dockerfile` | Go binary build (multi-stage) |
| `.env` | Secrets: `MF_DB_*`, `GEMINI_API_KEY`, `AI_DISTANCE_THRESHOLD` |
| `.miniflux.env` | Miniflux + Postgres passwords, Cloudflare tokens |
| `flareproxy/` | Python proxy for Cloudflare-protected feeds (gunicorn + requests) |
| `cleanup-cron` | Cron script to purge old feed entries from Postgres |

---

## Key Configuration

| Env Var | Default | Where Used | Notes |
|---------|---------|------------|-------|
| `AI_DISTANCE_THRESHOLD` | `0.45` | `ai/main.py` | AgglomerativeClustering cosine distance threshold. Currently set to `0.50` in `.env`. After evaluating recent negative/positive feedback, we switched `linkage` from `complete` to `average` and lowered the threshold to `0.50`. This combination perfectly merges cohesive large clusters (like the 12-article Marfin group) while aggressively shattering unrelated news (like distinct sports or music articles). |
| `GEMINI_API_KEY` | _(empty)_ | `ai/main.py`, Go backend | Used for topic naming (AI service) and summarization (Go) |
| `MF_DB_HOST` | `127.0.0.1` | Go backend | Postgres host |
| `MF_DB_PORT` | `5433` | Go backend | Currently `5435` in `.env` |
| `PORT` | `8080` | Go backend | Dashboard HTTP port |

---

## Six Display Categories (AI Service)

Defined in `ai/main.py` → `assign_category()` and `PREFERRED_ORDER`:

| Category | Content | Assignment Logic |
|----------|---------|-----------------|
| **Ελληνική ειδησεογραφία** | Greek-language news | `is_greek(text)` returns True |
| **Ξένη ειδησεογραφία** | English-language news | `is_greek(text)` returns False |
| **Αθλητικές ειδήσεις** | Sports (any language) | Miniflux category is `sports` or `panionara` |
| **Νέες μουσικές κυκλοφορίες** | New album/single releases | Miniflux category is `music` AND matches `NEW_RELEASE_PATTERN` regex |
| **Μουσικά νέα** | General music news | Miniflux category is `music` but no release keywords |
| **Διάφορες ειδήσεις** | Leftovers | Fallback (currently unused since is_greek catches everything) |

**Important**: The Go backend's `internal/config/config.go` still has the **old flat category names** (`ειδήσεις Κόσμος`, `ειδήσεις Ελλάδα`, etc.). These are only used in the fallback path when the AI service is unreachable. The AI service defines the authoritative 6-category system above.

---

## Clustering Pipeline (ai/main.py)

1. **Extract text**: Strip HTML tags from article content, truncate to 1000 chars
2. **Assign category**: Each article → one of 6 buckets based on Miniflux category + language detection
3. **Embed**: For each bucket, embed `title + first 50 words of content` using fastembed (`batch_size=16`)
4. **Cluster**: `AgglomerativeClustering(metric='precomputed', linkage='average', distance_threshold=0.50)`
5. **Name groups**: Multi-threaded Gemini calls to generate Greek topic names for groups with >1 article
6. **Return**: JSON with `categories[].groups[].articles[]`

---

## Redis Cache Keys

| Key | Contents | TTL | Refresh |
|-----|----------|-----|---------|
| `article_cache` | Flat `DashboardData` (no clustering) | 2 min | Synchronous on request |
| `ai_cache` | Full `AIDashboardData` (clustered) | 60 min | Background goroutine, serves stale while refreshing |

### Useful Commands
```bash
# Flush all caches (forces full re-fetch + re-cluster on next request)
docker exec dashboard-redis redis-cli flushall

# Inspect current AI cache
docker exec dashboard-redis redis-cli GET ai_cache | jq '.data.categories[].category'

# Check group counts
docker exec dashboard-redis redis-cli GET ai_cache | jq '[.data.categories[].groups[].count] | sort | reverse | .[0:10]'
```

---

## Running Locally

```bash
# Start everything
docker compose up -d

# Rebuild just the AI service after code changes
docker compose build ai-service && docker compose up -d ai-service

# Rebuild just the Go dashboard after code changes
docker compose build dashboard && docker compose up -d dashboard

# Run clustering tests (requires AI service to be running)
python3 ai/test_clustering.py

# Check AI service logs
docker logs miniflux-ai-service --tail 50

# Check dashboard logs
docker logs miniflux-dashboard --tail 50
```

---

## Known Gotchas & Past Failures

1. **OOM Kills**: The AI service embedding 400+ articles at once caused 6-8GB RAM spikes → kernel OOM kill. Fixed by adding `batch_size=16` to `model.embed()`.
2. **Model choice**: `intfloat/multilingual-e5-small` does not work with fastembed. Use `sentence-transformers/paraphrase-multilingual-mpnet-base-v2`.
3. **`query:` prefix**: The mpnet model does NOT need the `query:` prefix that E5 models use. Removed it.
4. **Mean pooling warning**: fastembed ≥0.6 switched mpnet from CLS to mean pooling. The warning is harmless and distances still work well.
5. **Gemini `'feed'` error**: The Gemini naming function references `a['feed']` but test articles sent without a `feed` field → `KeyError`. Non-blocking (naming falls back to first article title).
6. **Fallback flat view**: If the AI service is down (model downloading, OOM, restart), the dashboard silently serves the flat `article_cache` with old category names (`ειδήσεις Κόσμος` etc.) and every article appears as its own group with count=1. This is by design.
7. **The dashboard is available at `http://rakos:8080`** (the hostname of the machine running Docker).
8. **Gemini JSON errors**: Gemini occasionally returns malformed JSON (duplicate suffixes). The code handles this gracefully and falls back to the original article title.
