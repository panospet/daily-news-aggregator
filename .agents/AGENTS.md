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

### 4. Negative Feedback Loop
- Added a "thumbs down" button on each article source within an expanded group.
- Clicking the button silently triggers a POST request to `/feedback`.
- This feedback logs the entire group cluster + the specific rejected article to a `negative_feedback` Postgres table.
- This data allows us to manually review failing clusters and tweak distance thresholds or models in the future.

## Current State & Next Steps
- The system is fully operational.
- The AI service performs perfectly with no OOM errors.
- The frontend is polished and ready. 
- Future work: Consider training or fine-tuning models based on the gathered `negative_feedback` data.
