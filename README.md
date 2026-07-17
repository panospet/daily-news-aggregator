# Miniflux Dashboard 📰

A real-time news dashboard powered by Go + HTMX. Connects to a [Miniflux](https://miniflux.app) PostgreSQL database and displays articles grouped by category with live search, filtering, and auto-refresh.

## ✨ Features

- **Live feed** — Articles from your Miniflux instance, grouped by category
- **Category filtering** — Click tabs to filter by music/tech/sports/random
- **Real-time search** — Debounced search across titles and sources (HTMX active search)
- **Auto-refresh** — Automatically polls for new articles every 2 minutes
- **Dark/Light theme** — Toggle with one click
- **SSH tunnel** — Connect to remote Miniflux DB through an SSH tunnel
- **Stale caching** — Serves cached data gracefully if DB is unreachable

## 🏗️ Tech Stack

| Layer        | Technology     |
|-------------|----------------|
| Backend     | Go 1.26        |
| Frontend    | HTMX 2.0       |
| Database    | PostgreSQL (Miniflux) |
| Templates   | Go `html/template` |
| Styling     | Custom CSS, dark/light theme |

## 🚀 Quick Start

### 1. Clone & configure

```bash
git clone https://github.com/ppapacon/htmx-daily-news.git
cd htmx-daily-news
cp .env.example .env
# Edit .env with your Miniflux DB credentials
```

### 2. Environment variables

| Variable          | Description                          | Default         |
|-------------------|--------------------------------------|-----------------|
| `MF_DB_HOST`      | Miniflux DB host                     | `127.0.0.1`     |
| `MF_DB_PORT`      | Miniflux DB port                     | `5433`          |
| `MF_DB_NAME`      | Database name                        | `miniflux`      |
| `MF_DB_USER`      | Database user                        | `external_reader` |
| `MF_DB_PASSWORD`  | Database password                    | *(required)*    |
| `MF_SSH_HOST`     | SSH host for tunnel (blank = direct) | *(optional)*    |
| `MF_SSH_REMOTE_PORT` | Remote PostgreSQL port            | `5435`          |
| `MF_SSH_TUNNEL_PORT`  | Local tunnel port                 | `5433`          |
| `PORT`            | HTTP server port                     | `8080`          |

### 3. Run

```bash
# Build
go build -o htmx-daily-news .

# Run
./htmx-daily-news
```

Then open http://localhost:8080

## 🧠 Architecture

```
┌──────────┐     SSH Tunnel      ┌─────────────┐
│  Browser  │  ←─── HTTP ───→   │  Go Server  │
│  (HTMX)   │                    │  :8080      │
└──────────┘                    └──────┬──────┘
                                       │ PostgreSQL
                                       ↓
                                  ┌─────────────┐
                                  │  Miniflux   │
                                  │  Database   │
                                  └─────────────┘
```

The Go server:
1. Opens an SSH tunnel to the Miniflux host (if configured)
2. Queries the last 24 hours of articles
3. Groups them by category
4. Caches results for 2 minutes
5. Serves HTMX-powered partials for instant UI updates

## 🎯 HTMX Interactions

| Trigger              | Endpoint      | Swap Target       | Description                    |
|----------------------|---------------|-------------------|--------------------------------|
| Click category tab   | `GET /filter` | `#articles`       | Filter by category             |
| Type in search       | `GET /search` | `#articles`       | Debounced real-time search     |
| Click Refresh        | `GET /`       | `#articles`       | Manual refresh                 |
| Every 60s            | `GET /stats`  | `#stats-tab`      | Update stats silently          |
| Every 120s           | `GET /`       | `#articles`       | Auto-refresh articles          |

## 📁 Project Structure

```
├── main.go                    # Server, routes, DB, handlers
├── static/
│   ├── htmx.min.js            # HTMX (v2.0.4)
│   └── style.css              # All styling
├── templates/
│   ├── base.tmpl              # Layout, HTMX config, theme toggle
│   └── partials/
│       ├── articles.tmpl      # Category sections + article cards
│       └── stats.tmpl         # Stats bar (auto-refreshed)
├── .env.example
├── .gitignore
├── go.mod / go.sum
└── README.md
```

## 📄 License

MIT
