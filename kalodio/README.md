# ΤΟ ΚΑΛΩΔΙΟ — the newspaper frontend

Kalodio («The Wire») is the **official public frontend** served at
**https://newspaper.panos.pet**. It is a single-file, stdlib-only Python
newspaper-style UI that reads the clustered news feed from Redis and proxies
AI summaries through the Go dashboard.

## Architecture

```
browser ──▶ cloudflared tunnel (newspaper.panos.pet)
                │
                ▼
        kalodio :8280  ◀──reads──▶ redis :6379 (ai_cache, db 0)
                │
                └─POST /summarize (basic-auth proxy)──▶ dashboard :8081 ──▶ Gemini
```

- **Reads, never writes**: Kalodio consumes the `ai_cache` Redis key that the
  Go backend maintains. It has no DB credentials.
- **Summaries are proxied**: `POST /api/summary` → upstream `dashboard:8081`
  `/summarize` with basic auth (creds auto-read from repo `.env`), response is
  stripped to plain text before it reaches the browser.
- **No third-party deps** — pure Python 3 stdlib (http.server + a minimal RESP
  client). The container image is `python:3.12-alpine`.

## Configuration (environment)

| Variable            | Default               | Purpose                        |
|---------------------|-----------------------|--------------------------------|
| `KALODIO_PORT`      | `8280`                | Listen port                    |
| `KALODIO_BIND`      | `0.0.0.0`             | Bind address                   |
| `KALODIO_REDIS_HOST`| `127.0.0.1`           | Redis host                     |
| `KALODIO_REDIS_PORT`| `6379`                | Redis port                     |
| `KALODIO_UPSTREAM`  | `http://127.0.0.1:8080` | Dashboard base URL for /summarize |
| `KALODIO_AUTH_USER` | *(from repo .env)*    | Dashboard basic-auth user      |
| `KALODIO_AUTH_PASS` | *(from repo .env)*    | Dashboard basic-auth password  |

## Running locally

```bash
cd kalodio
python3 kalodio.py          # serves on http://0.0.0.0:8280
```

## Running in Docker

```bash
docker compose up -d --build kalodio
curl http://127.0.0.1:8280/healthz   # -> ok
```

## Endpoints

| Route           | Description                                   |
|-----------------|-----------------------------------------------|
| `GET /`         | The newspaper page                            |
| `GET /api/news` | Clustered feed JSON (60s in-process cache)    |
| `POST /api/summary` | `{category, group_id}` → proxied AI summary |
| `GET /healthz`  | Liveness probe                                |

## Reverting to the legacy HTMX dashboard

The old frontend remains available and can be restored at any time:

```bash
git checkout legacy-frontend -- .     # or inspect the tag
# swap the tunnel target back to the dashboard port in Cloudflare,
# or restore PORT=8080 for the dashboard service.
```
