# Daily News Pre-Generation for Audio Briefing — Implementation Plan

> Status: **Plan / proposal** (not yet implemented).
> Companion to `AUDIO_BRIEFING_PLAN.md` (the consumer). This document covers the **producer** side: pre-generating grounded summaries and exposing them as a read-only "news block" that the audio-briefing service fetches over HTTP.

---

## 1. Goal

Twice a day, **before** each briefing slot, pre-generate grounded Greek summaries for the **top 20 topic groups** and store them so the audio-briefing service can fetch everything it needs in **one HTTP call**.

Why pre-generate (rather than the briefing calling `/summarize` per topic on demand):

- The briefing is **time-sensitive**: at 08:00 it must be *ready*, not "starting to generate."
- Summarization is slow (enrichment + LLM + grounding verification). Pre-generating moves that latency out of the briefing's critical path.
- Generating **20** (not 10) gives fallback headroom: the briefing takes 10, so up to 10 failures can be silently absorbed.

---

## 2. Where it lives

Inside the existing **daily-news-aggregator Go dashboard** (same process, same module). It reuses:

- `topView()` — the existing top-groups ranking (article count desc, then distinct feeds desc, deduped by `group_id`).
- `internal/summarize` — the full anti-hallucination pipeline (enrichment → stub filter → grounded prompt → verification). **No shortcut**: summaries must go through verification, never bypassed.
- `SummaryCache` (`summary_cache` in Redis) — already caches summaries; pre-generation warms this cache ahead of time.

---

## 3. What it produces

A **news block** per slot, exposed read-only:

```
GET /api/briefing-news?slot=morning|evening
Authorization: Basic <AUTH_USERNAME>:<AUTH_PASSWORD>

200 OK  application/json
{
  "slot": "morning",
  "generated": "2026-11-06T07:45:02+02:00",
  "items": [
    {"topic": "Απεργία στα ΜΜΜ", "category": "Ελλάδα", "summary_text": "Σήμερα τα μέσα μεταφοράς …"},
    …
  ]
}
```

- `topic` — the group's topic title (the fancy Greek title from the summarizer, falling back to the AI service's name).
- `summary_text` — the verified summary, **plain text with inline `[n]` citations stripped** (speech-ready).
- `category` — optional, for display in the web player.
- `generated` — Athens timestamp of the block.

Sorted in ranking order (top first). The consumer takes the first `items`.

A manual trigger is also exposed (used by the scheduler fallback / crontab):

```
POST /api/briefing-news/generate?slot=morning|evening
```

It runs generation synchronously and returns the freshly written block.

---

## 4. Per-slot steps

1. **Select top-20** groups using `topView()` (cut to 20).
2. **Summarize** each group via `internal/summarize`, **in parallel** goroutines, reusing `SummaryCache` (skip groups already cached and fresh).
3. **Citations are already stripped** by the summarizer (it returns clean text), so the producer passes the summary through as-is. The block's `topic` uses the summarizer's fancy title, falling back to the group's topic.
4. **Store** the block as JSON in Redis under `briefing_news:morning` / `briefing_news:evening`.
5. **Serve** via the endpoint above (basic-auth protected).

---

## 5. Sequencing & scheduling

The pipeline must run in order:

```
ai_cache refresh → pre-generate top-20 (07:45 / 19:45) → briefing runs (08:00 / 20:00)
```

- Pre-generation runs ~15 minutes before each slot so summaries reflect the latest stories and are already cached when the briefing asks.
- Implementation: a scheduled goroutine (ticker) in the dashboard process, **and/or** a host crontab hitting an internal trigger endpoint (consistent with the briefing's crontab fallback pattern). Both must be idempotent (re-running is safe; cached groups are skipped).
- Times configurable via env (default `07:45,19:45`).

---

## 6. Failure handling

- **Partial results are fine:** if 5 of 20 fail verification, store the 15 good ones. The consumer takes 10 and never notices.
- **Retry:** a failed group may be retried a couple of times before being dropped for that slot.
- **Total failure** (LLM/API key down): store a **titles-only** block (`summary_text` = the topic title, or empty) so the briefing can still publish something; log and alert.
- **Verification rejection** (HTTP 422 path): the group is simply skipped; this is normal operation, not an error to alarm on.
- Log to the dashboard's existing logging; optional Telegram alert (reuse media-stack token).

---

## 7. Configuration (environment)

| Variable | Default | Purpose |
|----------|---------|---------|
| `BRIEFING_PREGEN_N` | `20` | Top groups to summarize per slot |
| `BRIEFING_PREGEN_MORNING` | `07:45` | Pre-generation time for the morning slot (Europe/Athens) |
| `BRIEFING_PREGEN_EVENING` | `19:45` | Pre-generation time for the evening slot (Europe/Athens) |
| `BRIEFING_NEWS_BLOCK_TTL_HOURS` | `36` | How long a stored block stays valid |

No new secrets — reuses `AUTH_USERNAME`/`AUTH_PASSWORD` and the existing Gemini/LLM config.

---

## 8. Implementation phases

1. **Selection** — `topView()` → top-20, reusing existing ranking. *(~0.5 day)*
2. **Parallel summarize** — loop with goroutines + `SummaryCache`, idempotent. *(~1 day)*
3. **Speech-ready text** — citation/HTML stripping. *(~0.5 day)*
4. **Storage + endpoint** — store block, add `GET /api/briefing-news` (basic auth). *(~0.5 day)*
5. **Scheduler + integration** — ticker/crontab, sequencing after `ai_cache` refresh, end-to-end test with a manual run. *(~0.5 day)*

---

## 9. Notes & decisions

- **Top-20, not top-10:** fallback headroom for the consumer (it needs 10).
- **Ranking = `topView()`:** article count desc, then distinct feeds desc, deduped by `group_id` — no new ranking logic.
- **Grounded pipeline only:** reuse `internal/summarize` verbatim; do not bypass verification.
- **Speech-ready output:** the consumer does zero text processing (no HTML stripping, no citation regex).
- **Idempotent:** re-running a pre-generation is safe; cached summaries are reused, and the block is atomically overwritten.
