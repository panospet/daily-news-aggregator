# Kalodio — Missing Features Plan

> **Status:** Planning only. No code below has been implemented.
> **Scope:** Features present in the legacy HTMX dashboard (`daily-news-aggregator`, `:8080`)
> that are **not** present in the current Kalodio frontend (`kalodio/kalodio.py`, `:8280`),
> which serves `newspaper.panos.pet`.
> **Audience:** A future maintainer who will implement these without touching the Go backend.

---

## Context

Kalodio is a deliberately **read-only, stdlib-only Python** frontend. It reads the
clustered view from Redis (`ai_cache`, DB 0) and proxies summaries to the Go
backend (`/summarize`). It must stay that way — **no Go backend changes**, no
third-party Python deps. That constraint shapes every recommendation below:
most missing features are **pure client-side** (vanilla JS in the `PAGE` string),
and where the Go backend already exposes an endpoint, Kalodio should *use it*
rather than reimplement logic.

> **Note:** Modifying `kalodio/kalodio.py` itself (adding a proxy endpoint,
> changing `news_api()` output) is fine — that's Kalodio's own Python backend.
> The constraint is: do not touch `cmd/server/`, `internal/`, `templates/`, or
> any Go code.

### What Kalodio already has (do not re-implement)
These features are present in Kalodio and working. Listed here so a future
implementer doesn't duplicate effort:

- **Category rail** with counts, clickable to filter ✅
- **Hashtag (#tag) filtering** — client-side substring on tags ✅
- **Summarize button** per group — proxied to Go `/summarize` ✅
- **Day/Night theme toggle** with `localStorage` persistence ✅
- **Infinite-scroll lazy rendering** — `IntersectionObserver`, 36/batch ✅
- **Source lists** per story (in collapsible `<details>`) ✅
- **Search** — client-side substring on topic / feed / article title ✅
- **Responsive mobile layout** ✅
- **Stats** — total articles + generated time in masthead ✅
- **Category icons** — the old UI showed emoji (`catIcon`) next to each category
  in the sidebar. Kalodio's rail is plain text. Minor visual polish; add
  a `CAT_ICONS` map in JS and prepend to rail button labels if desired.

### Reference: where the old behavior lives
| Feature | Old UI location |
|--------|-----------------|
| Read/unread | `templates/base.tmpl` → `markAsRead`, `markUnread`, `restoreReadState` + `.card--read` CSS |
| Semantic search | `internal/handler/handler.go` → `semanticSearch()`, `/search-embed`, Redis DB 1 group map |
| Feedback (✕ / 👍) | `templates/partials/articles.tmpl` → `/feedback`, `/feedback/positive`; `handler.go` `HandleFeedback` |
| Auto-refresh | `templates/base.tmpl` → `setInterval(… htmx.ajax('/') …, 600000)` |
| Keyboard shortcuts | `templates/base.tmpl` → `/` focus, `Esc` close |
| Collapsible sections | `templates/base.tmpl` → `.cat__head` toggle `.cat--closed` |
| Fallback banner | `templates/partials/articles.tmpl` → `.notice-banner` `IsFallback` |
| Error toasts | `templates/base.tmpl` → `showToast()` on htmx errors |
| Sources: article titles | `templates/partials/articles.tmpl` line 64 → `<span>{{.Title}}</span>` per source |
| Top Tags sidebar | `templates/base.tmpl` lines 304-311 → `TopTags` section with counts |

### Current Kalodio API surface (do not break)
- `GET /` → HTML page
- `GET /api/news` → `{ total, generated, cats[], groups[], top_tags[] }`
  - group object: `gid, cat, cats[], topic, count, tags[], feeds[], arts, articles[{title,url,feed,time_ago}]`
  - top_tags object: `{ tag, count }` (pre-computed in `ai_cache`, not derived from groups)
- `POST /api/summary` → `{ summary, error }` (proxied to Go `/summarize`)
- `GET /healthz`

### `ai_cache` payload shape (verified against Redis)
The `ai_cache` JSON in Redis DB 0 has exactly these top-level keys:
`categories`, `generated`, `top_tags`, `total`.
There is **no** `is_fallback` / `IsFallback` field — the fallback state is
computed at request time by the Go handler, not persisted in the cache.
This affects Feature 6 below.

---

## Feature 1 — Read / Unread tracking  ⭐ highest value

**What the old UI did:** Clicking a card marked it read; the card dimmed
(`opacity:0.6`) and its title got a strikethrough. State persisted in
`localStorage` key `flow-read` (map of `group_id:count → {t: timestamp}`),
surviving reloads. An "📬 Unread" button restored it.

**Why it matters here:** A newspaper is read once per day. Without read-state,
every return visit shows a full wall of identical unread stories — the single
most disorienting gap versus the old UI.

**Implementation (client-side only):**
1. On `load()` completion, read `localStorage['kalodio-read']` → `Set` of group
   `gid`s that have been seen.
2. When a summary is expanded (the `btn.onclick` open branch in `card()`),
   add `g.gid` to the set, write back to `localStorage`, and add class
   `story--read` to the `article` element.
3. Add CSS:
   ```css
   article.story--read { opacity:.55; }
   article.story--read h3.headline { text-decoration: line-through; }
   article.story--read .btn-sum { display:none; }   /* hide re-summarize */
   ```
4. Add an "📬 Ανάγνωση" / "Μη αναγνωσμένα" toggle button in the `.subline`
   controls that flips a `showRead` flag: when off, `apply()` filters out
   read `gid`s (so the page shows only fresh stories); when on, shows all.
5. Optional: per-card "↺ Unread" affordance to remove a `gid` from the set.
   (The old UI did this via `markUnread`; a small `×` on read cards suffices.)

**Complexity:** Low. ~40 lines JS + CSS. No backend.

**Edge cases:** `gid` can be empty for fallback groups — skip those in the set.
Filtering by read-state must happen in `apply()` so search/category views
respect it, and the unread-count in the rail should reflect it.

---

## Feature 2 — Auto-refresh

**What the old UI did:** Every 10 min (`setInterval`, 600000 ms) a background
`htmx.ajax('/')` refreshed the stream (unless a card was open).

**Why it matters:** Without it, new clusters only appear on manual browser
reload. The watchdog (cronjob `e7fdc297ca9f`) keeps the *cache* fresh hourly,
but the *page* won't reflect it until reloaded.

**Implementation (client-side only):**
1. After `load()`, set:
   ```js
   setInterval(async () => {
     if (document.querySelector('article.story.expanded')) return; // don't yank open card
     const r = await fetch('/api/news'); DATA = await r.json();
     buildRail(); apply();
   }, 10 * 60 * 1000);
   ```
2. Preserve read-state (Feature 1) and current `CAT`/`Q` across the refresh so
   the user's place is kept.
3. Optionally surface a subtle "ανανεώθηκε στις HH:MM" timestamp in the
   `.subline` (the old UI showed `.meta` generated time; Kalodio already shows
   `DATA.generated` in the masthead edition line — reuse it).

**Complexity:** Low. ~10 lines JS.

---

## Feature 3 — Sources list: article titles lost

**What the old UI did:** Each source in the list was rendered as a clickable
**article title** with a `→` arrow, plus a metadata line below:
`FeedName · 2h ago`. You could scan the titles to decide which source to click.

**Current Kalodio gap:** `card()` in `kalodio.py` line 434 renders each source
as `<a href="…">feedName</a>` — the **article title is not shown at all**.
Only the feed name is the link text. You see "kathimerini.gr →" instead of
"Ολυμπιακός - Ατρόμητος: 1-0 → kathimerini.gr · 2h ago".

**Implementation (client-side only):**
Change the source `<li>` template in `card()` from:
```js
'<li><a href="…">'+esc(a.feed)+'</a><span class="when">…</span></li>'
```
to:
```js
'<li><a href="…">'+esc(a.title)+'</a>' +
'<span class="src-meta">'+esc(a.feed)+' · '+esc(a.time_ago)+'</span></li>'
```
Add CSS for `.src-meta` (small, muted, monospace — matching old `.src-meta`).

**Complexity:** Trivial. ~4 lines JS + CSS.

---

## Feature 4 — Top Tags sidebar section

**What the old UI did:** The sidebar had a dedicated "Tags" section (lines
304-311 of `base.tmpl`) showing the top tags with article counts, each
clickable to filter by that tag. It sat below the Topics/Category nav.

**Current Kalodio gap:** The rail (`#rail`) only renders categories. Tags are
shown as small chips on each card (clickable to filter), but there's no
persistent tag overview in the sidebar — you can't see which tags are trending
across the whole edition without scanning every card.

**Implementation (client-side only):**
1. The data is already available — `DATA.top_tags` is a pre-computed array of
   `{tag, count}` objects from `ai_cache`. No need to derive it from groups.
2. In `buildRail()`, after the category buttons, add a "Ετικέτες" heading and
   render the top ~15 tags:
   ```js
   const topTags = (DATA.top_tags || []).slice(0, 15);
   // add heading: "Ετικέτες"
   for (const t of topTags) {
     const b = document.createElement('button');
     b.innerHTML = esc(t.tag) + ' <span class="n">' + t.count + '</span>';
     b.onclick = () => setTag(t.tag);
     rail.appendChild(b);
   }
   ```
3. The "Ετικέτες" section should be visually separated from "Τμήματα" with a
   small heading, matching the old UI's `panel__label` style.

**Complexity:** Low. ~15 lines JS.

---

## Feature 5 — Keyboard shortcuts

**What the old UI did:** `/` focused the search box; `Esc` closed any open card
and the menu.

**Implementation (client-side only):**
```js
document.addEventListener('keydown', e => {
  if (e.key === '/' && !['INPUT','TEXTAREA'].includes(document.activeElement.tagName)) {
    e.preventDefault(); document.getElementById('q').focus();
  }
  if (e.key === 'Escape') {
    document.querySelectorAll('article.story.expanded').forEach(a => {
      a.classList.remove('expanded');
      a.querySelector('.btn-sum').textContent = '✦ Σύνοψη';
      a.querySelector('.summary-box').hidden = true;
    });
  }
});
```

**Complexity:** Trivial. ~12 lines JS.

---

## Feature 6 — Semantic (embedding) search

**What the old UI did:** `handler.go: semanticSearch()` embedded the query via
`POST /search-embed`, ran pgvector nearest-neighbour lookup, resolved group IDs
through Redis DB 1, and returned ranked groups. Free-text queries that weren't
hashtags used this path.

**Why it matters:** Subject search ("election", "πλημμύρες") returns topically
related clusters even when no title shares the keyword. Kalodio today only does
**substring** matching on `topic` / `feed` / `article.title` — much weaker.

**Implementation (Kalodio Python backend + client-side JS):**
1. Add a new `GET /api/search?q=` endpoint in `kalodio.py` that proxies to the
   Go backend's `/search?q=` endpoint (which already supports semantic search
   via pgvector). The Go endpoint returns HTML; the new Kalodio endpoint should
   parse the response and return JSON in the same shape as `/api/news` groups.
   - This modifies `kalodio.py` (Kalodio's own backend) — that's fine. It does
     **not** modify the Go backend.
2. In the client-side `apply()`, branch on text (non-`#`) queries: call
   `fetch('/api/search?q=…')` and replace the working set with the returned
   groups.
3. Keep substring search as the instant client-side fallback when the semantic
   fetch fails or is slow (>~400ms) — exactly like the old UI's
   `semanticSearch` → substring fallback.
4. UX: a mode toggle or just "if query has results from semantic, show those;
   else substring" with a tiny "σχετικά αποτελέσματα" label.

**Complexity:** Medium. ~30 lines in `kalodio.py` (proxy) + ~25 lines JS
(mode switch in `apply()`). No change to Go.

**Note:** Requires the Go backend's `/search` semantic path to be healthy
(pgvector + Redis DB 1). Verify before relying on it; the fallback keeps the
UI usable regardless.

---

## Feature 7 — Error toasts / better failure signal

**What the old UI did:** HTMX `responseError` / `sendError` / `timeout` popped
a `showToast()` in the corner (non-blocking, auto-dismiss).

**Current Kalodio gap:** Only the inline "⚠️ Summary service unreachable" text
on a summary fetch, and a silent console error on `/api/news` failure.

**Implementation (client-side only):**
1. Add a fixed `#toast-container` div + `showToast(msg, type)` helper.
2. Call it on:
   - `/api/news` fetch failure (auto-refresh, Feature 2) → "Σφάλμα φόρτωσης —
     προσπάθεια ξανά."
   - summary fetch error (already returns `error`; surface it as a toast too).
3. Keep it non-blocking and auto-dismiss (~3.5s), matching old behavior.

**Complexity:** Low. ~20 lines JS/CSS.

---

## Feature 8 — AI-grouping fallback banner  ⚠️ blocked

**What the old UI did:** When `IsFallback` was true (cache stale, background
re-cluster running), it showed "⏳ AI grouping is running in the background.
Showing raw articles temporarily."

**Why it matters:** Right now, if the watchdog is mid-rebuild or the cache is
empty, Kalodio silently shows stale/raw content with no signal that data is
transitional.

**⚠️ Blocker:** The `ai_cache` JSON in Redis does **not** contain an
`is_fallback` field. The fallback state is computed at request time by the Go
handler's `GetDataWithFallback()` — it checks whether the AI cache returned
an error and fell back to flat data. Kalodio reads the cache directly, so it
has no way to know if the data is fallback.

**Possible workarounds (all require either Go changes or heuristic):**
- **(A) Heuristic:** Compare `DATA.generated` timestamp to current time. If
  older than e.g. 3 hours, show a "δεδομένα πιθανώς παλιά" warning. This is
  an approximation — it can't distinguish "stale but real AI data" from
  "fallback flat data."
- **(B) Go change (out of scope):** Add `is_fallback: true` to the `ai_cache`
  payload when the data came from `FlatToAI()`. This is the clean fix but
  violates the "no Go changes" constraint.
- **(C) Skip:** Accept that Kalodio shows whatever the cache holds without
  fallback signaling. The watchdog (cronjob `e7fdc297ca9f`) makes fallback
  rare in practice.

**Recommendation:** (C) unless the user specifically wants the banner. If
implemented, (A) is the only option within constraints.

**Complexity:** Low if (A) — ~8 lines JS. Requires (B) for correctness.

---

## Feature 9 — Collapsible category sections  *(optional)*

**What the old UI did:** Clicking a `.cat__head` toggled `.cat--closed`, hiding
that section's cards.

**Why it's low priority:** Kalodio renders all groups in one flat `#grid` (not
per-category sections), so "collapsible sections" doesn't map 1:1. The category
rail already provides isolation (click a category to filter to it). The "fold a
section" affordance is a nice-to-have, not a functional gap.

**If implemented (client-side only):**
- Add a "collapse" control per category in the rail that, when toggled, hides
  all `article.story` whose `g.cats` includes that category.

**Recommendation:** Skip unless a user specifically requests it.

**Complexity:** Low if done; otherwise N/A.

---

## Feature 10 — Feedback loops (✕ not-relevant, 👍 good summary)

**What the old UI did:** Per-source "✕ Not relevant" (`POST /feedback?
irrelevant_url=…`) and "👍 Good summary" (`POST /feedback/positive`) posted
signals back into the system.

**Why it's intentionally absent:** Kalodio's design mandate is **read-only
toward the Go backend** — it proxies *read* of summaries but must not write
state into the dashboard. Re-adding feedback means writing into the Go backend.

**Recommendation:** **Defer / out of scope** unless the user explicitly wants
it. If ever implemented, it would require:
- New Kalodio UI controls (✕ on each source `<li>`, 👍 on each summary box).
- `POST /api/feedback` in `kalodio.py` that forwards to the Go
  `/feedback` / `/feedback/positive` endpoints with the upstream basic-auth
  creds (already loaded via `find_creds()`).
- This breaks the "read-only" principle and should be a conscious decision, not
  a default.

**Complexity:** Medium, and a design-policy change.

---

## Implementation order (suggested)

| # | Feature | Effort | Changes to `kalodio.py`? | Changes to Go? |
|---|---------|--------|--------------------------|----------------|
| 1 | Read/Unread (F1) | Low | No (client-side only) | No |
| 2 | Auto-refresh (F2) | Low | No (client-side only) | No |
| 3 | Sources: article titles (F3) | Trivial | No (client-side only) | No |
| 4 | Top Tags sidebar (F4) | Low | No (client-side only) | No |
| 5 | Keyboard shortcuts (F5) | Trivial | No (client-side only) | No |
| 6 | Semantic search (F6) | Medium | **Yes** (new `/api/search` proxy) | No |
| 7 | Error toasts (F7) | Low | No (client-side only) | No |
| 8 | Fallback banner (F8) | Low/Blocked | No (heuristic only) | **Yes** for correctness |
| 9 | Collapsible sections (F9) | Low/Opt | No (client-side only) | No |
| 10 | Feedback (F10) | Medium | **Yes** (new `/api/feedback` proxy) | **Yes** (policy change) |

Features 1–5 and 7 are pure client-side JS/CSS inside the `PAGE` template
string in `kalodio/kalodio.py`. Feature 6 adds a proxy endpoint in the same
file. Feature 8 is blocked without a Go change (heuristic workaround
available). Feature 9 is optional. Feature 10 is a design-policy decision.

---

## Repo note (important)
There are **two** copies of Kalodio on disk:
- `/home/panos/kalodio/` — older plain directory (not git-tracked, pre-dates the
  Docker build). **Not** the source of truth.
- `/home/panos/daily-news-aggregator/kalodio/` — **git-tracked**, built by
  `docker-compose.yml` (`build: ./kalodio`). This is the canonical source.

Implement against the **tracked** copy. The running container is rebuilt from
it, so edits there flow to `newspaper.panos.pet` on `docker compose up -d --build`.
