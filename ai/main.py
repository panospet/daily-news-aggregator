import os
import time
import hashlib
import re
import requests
import httpx
import asyncio
import numpy as np
import json
from fastapi import FastAPI, Request
from collections import defaultdict
from sklearn.cluster import AgglomerativeClustering
from sklearn.feature_extraction.text import TfidfVectorizer
from sklearn.metrics.pairwise import cosine_distances
from fastembed import TextEmbedding
from contextlib import asynccontextmanager
import html

COMMAND_CODE_API_KEY = os.getenv("COMMAND_CODE_API_KEY", "")
GEMINI_API_KEY = os.getenv("GEMINI_API_KEY", "")
COMMAND_CODE_MODEL = os.getenv("COMMAND_CODE_MODEL", "MiniMaxAI/MiniMax-M3")
fallback_used_globally = False

# In-process LLM caches. The AI service is long-lived, so this avoids asking
# the provider to re-name or re-categorize identical article sets between cache
# rebuilds without making the clustering output less fresh than the source data.
_LLM_CACHE_TTL_SEC = 12 * 3600
_NAMING_CACHE = {}
_SINGLETON_CACHE = {}


def _now() -> float:
    return time.time()


def _article_signature(article: dict) -> str:
    value = article.get("id")
    if value is not None:
        return "id:" + str(value)
    return "title:" + str(article.get("title", "")) + "|" + str(article.get("feed", ""))


def _cluster_signature(articles) -> str:
    ids = sorted(_article_signature(a) for a in articles)
    return hashlib.sha256("|".join(ids).encode("utf-8")).hexdigest()


def _cache_get(cache: dict, key: str):
    entry = cache.get(key)
    if not entry:
        return None
    if _now() - entry.get("ts", 0) > _LLM_CACHE_TTL_SEC:
        del cache[key]
        return None
    return entry.get("value")


def _cache_set(cache: dict, key: str, value):
    cache[key] = {"value": value, "ts": _now()}

CORE_CATEGORIES = [
    "Ελλάδα",
    "Κόσμος",
    "Πολιτική & Διπλωματία",
    "Οικονομία & Ενέργεια",
    "Κοινωνία",
    "Έγκλημα & Αστυνομικό",
    "Περιβάλλον & Φυσικές Καταστροφές",
    "Πόλεμος & Συγκρούσεις",
    "Αθλητισμός",
    "Πολιτισμός & Showbiz",
    "Καιρός",
    "Τεχνολογία & Επιστήμη",
    "Lifestyle & Περίεργα",
    "Μουσική",
    "Νέες Κυκλοφορίες (Μουσική)",
    "Άρθρα Γνώμης",
    "Διάφορα"
]

# Human-readable descriptions for each core category, used for
# embedding-based (local, no-LLM) zero-shot categorization of singleton articles.
CORE_CATEGORY_DESCRIPTIONS = {
    "Ελλάδα": "Ελληνικές ειδήσεις, Ελλάδα, εγχώρια γεγονότα και νέα",
    "Κόσμος": "Διεθνείς ειδήσεις, κόσμος, εξωτερική πολιτική",
    "Πολιτική & Διπλωματία": "Πολιτική, κυβέρνηση, βουλή, διπλωματία, εκλογές",
    "Οικονομία & Ενέργεια": "Οικονομία, αγορές, ενέργεια, επιχειρήσεις, χρηματιστήριο",
    "Κοινωνία": "Κοινωνία, κοινωνικά ζητήματα, εκπαίδευση, υγεία, μετανάστευση",
    "Έγκλημα & Αστυνομικό": "Έγκλημα, αστυνομία, δικαιοσύνη, δίκες, συλλήψεις",
    "Περιβάλλον & Φυσικές Καταστροφές": "Περιβάλλον, κλίμα, φυσικές καταστροφές, πυρκαγιές, σεισμοί, πλημμύρες",
    "Πόλεμος & Συγκρούσεις": "Πόλεμος, συγκρούσεις, στρατός, γεωπολιτική, τρομοκρατία",
    "Αθλητισμός": "Αθλητισμός, ποδόσφαιρο, μπάσκετ, αγώνες, πρωταθλήματα",
    "Πολιτισμός & Showbiz": "Πολιτισμός, κινηματογράφος, τηλεόραση, celebrities, showbiz, τέχνη",
    "Καιρός": "Καιρός, πρόγνωση, θερμοκρασίες, βροχές, καύσωνας",
    "Τεχνολογία & Επιστήμη": "Τεχνολογία, επιστήμη, AI, gadgets, διαστήματα",
    "Lifestyle & Περίεργα": "Lifestyle, περίεργα, διατροφή, ταξίδια, υγεία και ευεξία",
    "Μουσική": "Μουσική, συναυλίες, καλλιτέχνες, φεστιβάλ",
    "Νέες Κυκλοφορίες (Μουσική)": "Νέες μουσικές κυκλοφορίες, νέα τραγούδια, νέα άλμπουμ, single release, νέο single",
    "Άρθρα Γνώμης": "Άρθρα γνώμης, σχόλια, αναλύσεις, editorial, απόψεις",
    "Διάφορα": "Διάφορα, γενικά θέματα",
}


def local_categorize_singletons(articles):
    """Categorize singleton articles locally using embedding similarity to
    category descriptions. No LLM calls. Returns list of dicts with
    core_tags and custom_tags, aligned with input order."""
    if not articles or model is None:
        return [{"core_tags": ["Διάφορα"], "custom_tags": []} for _ in articles]

    cat_names = CORE_CATEGORIES
    category_embs = get_cat_embs()
    if category_embs is None:
        cat_texts = [CORE_CATEGORY_DESCRIPTIONS.get(c, c) for c in cat_names]
        category_embs = np.array(list(model.embed(cat_texts, batch_size=16)))
        norms = np.linalg.norm(category_embs, axis=1, keepdims=True)
        category_embs = category_embs / np.where(norms == 0, 1e-12, norms)

    art_texts = []
    for a in articles:
        content = extract_text(a.get("content", ""))
        snippet = " ".join(content.split()[:40])
        art_texts.append(f"{a.get('title', '').strip()} {snippet}".strip())

    art_embs = np.array(list(model.embed(art_texts, batch_size=16)))
    norms = np.linalg.norm(art_embs, axis=1, keepdims=True)
    art_embs = art_embs / np.where(norms == 0, 1e-12, norms)

    SIM_THRESHOLD = 0.25
    results = []
    for i in range(len(articles)):
        sims = category_embs @ art_embs[i]
        ranked = sorted(range(len(cat_names)), key=lambda j: float(sims[j]), reverse=True)
        top_score = float(sims[ranked[0]])
        chosen = []
        for j in ranked:
            if len(chosen) >= 3:
                break
            if float(sims[j]) >= SIM_THRESHOLD and (not chosen or (top_score - float(sims[j]) <= 0.07)):
                chosen.append(cat_names[j])
            else:
                break
        if not chosen:
            chosen = [cat_names[ranked[0]]]
        # Deduplicate
        seen = set()
        uniq = []
        for c in chosen:
            if c not in seen:
                seen.add(c)
                uniq.append(c)
        results.append({"core_tags": uniq, "custom_tags": []})
    return results


# Global embedding model (loaded on startup)
model = None
nlp = None
cat_embs = None

def get_cat_embs():
    global cat_embs
    if cat_embs is None and model is not None:
        cat_texts = [CORE_CATEGORY_DESCRIPTIONS.get(c, c) for c in CORE_CATEGORIES]
        raw_cat_embs = np.array(list(model.embed(cat_texts, batch_size=16)))
        cat_norms = np.linalg.norm(raw_cat_embs, axis=1, keepdims=True)
        cat_embs = raw_cat_embs / np.where(cat_norms == 0, 1e-12, cat_norms)
    return cat_embs

def clean_title(title: str, feed: str = "") -> str:
    if not title:
        return ""
    # Strip media tags like (vid), [video], (pics), (εικόνες), (βίντεο)
    t = re.sub(r'\s*[\(\[](?:vid|video|pics?|εικόνες|βίντεο|φωτογραφίες)[\)\]]\s*', ' ', title, flags=re.IGNORECASE)
    # Strip publisher branding prefixes or suffixes:
    t = re.sub(r'\s*[-–—|]\s*(?:in\.gr|documento|ertnews|newsit|protothema|gazzetta|sport24|kathimerini|iefimerida|cnn greece|skai|lifo|newsbeast|tovima|ethnos|naftemporiki|avgi|newsbomb|euro2day|sdna|sdna\.gr|gazzetta\.gr|sport24\.gr|news247|news247\.gr|capital|mononews).*$', '', t, flags=re.IGNORECASE)
    if feed:
        clean_feed = re.sub(r'\s*(?:rss|feed|news)\s*', '', feed, flags=re.IGNORECASE).strip()
        if clean_feed and len(clean_feed) > 2:
            t = re.sub(rf'\s*[-–—|:]\s*{re.escape(clean_feed)}.*$', '', t, flags=re.IGNORECASE)
            t = re.sub(rf'^{re.escape(clean_feed)}\s*[-–—|:]\s*', '', t, flags=re.IGNORECASE)
    t = re.sub(r'^\s*(?:in\.gr|documento|ertnews|newsit|protothema|gazzetta|sport24|kathimerini|iefimerida|cnn greece|skai|lifo|newsbeast|tovima|ethnos|naftemporiki|avgi|newsbomb|euro2day)\s*[-–—:]\s*', '', t, flags=re.IGNORECASE)
    cleaned = re.sub(r'\s+', ' ', t).strip()
    return cleaned if cleaned else title.strip()

@asynccontextmanager
async def lifespan(app: FastAPI):
    global model, nlp
    print("Loading embedding model...")
    model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-mpnet-base-v2')
    print("Loading spaCy model...")
    import spacy
    nlp = spacy.load("el_core_news_md")
    print("Pre-embedding category descriptions...")
    get_cat_embs()
    print("Models and category embeddings loaded successfully.")
    yield
    print("Shutting down AI service.")

app = FastAPI(lifespan=lifespan)

def extract_text(html_content):
    if not html_content:
        return ""
    plain_text = re.sub(r'<[^>]+>', '', html_content)
    plain_text = html.unescape(plain_text)
    plain_text = re.sub(r'\s+', ' ', plain_text).strip()[:1000]
    return plain_text


async def call_naming(topic, articles, client):
    global fallback_used_globally
    cache_key = _cluster_signature(articles)
    cached = _cache_get(_NAMING_CACHE, cache_key)
    if cached is not None:
        return cached

    if not COMMAND_CODE_API_KEY and not GEMINI_API_KEY:
        result = {"topic": topic, "core_tags": ["Διάφορα"], "custom_tags": []}
        _cache_set(_NAMING_CACHE, cache_key, result)
        return result

    try:
        articles_str = "\n".join([f"- {a['title']} ({a.get('feed', '')})" for a in articles])
        prompt = f"""Είσαι βοηθός ονοματοδοσίας και κατηγοριοποίησης ειδήσεων. 
Έχω μια ομάδα άρθρων που αναφέρονται στο ίδιο γεγονός. Ο προσωρινός τίτλος είναι: "{topic}".

Παρακάτω είναι τα άρθρα:
{articles_str}

Αποστολή σου:
1. Γράψε έναν κατάλληλο, ελκυστικό, πολύ σύντομο (έως 8-10 λέξεις) τίτλο θεματικής (Topic) στα ΕΛΛΗΝΙΚΑ.
2. Επίλεξε από 1 έως 3 κατηγορίες από την παρακάτω αυστηρή λίστα (Core Categories):
{json.dumps(CORE_CATEGORIES, ensure_ascii=False)}
3. Αν θέλεις, πρόσθεσε έως 2 δικές σου ειδικές ετικέτες (Custom Tags) που περιγράφουν πιο συγκεκριμένα το θέμα. ΕΙΔΙΚΑ αν επιλέξεις την κατηγορία "Νέες Κυκλοφορίες (Μουσική)", ΠΡΕΠΕΙ ΟΠΩΣΔΗΠΟΤΕ να προσθέσεις το μουσικό είδος (π.χ. "Rock", "Pop", "Hip Hop") στις ειδικές ετικέτες.

Απάντησε ΜΟΝΟ με το παρακάτω JSON format:
{{"topic": "Ο Ελληνικός Τίτλος", "core_tags": ["Κατηγορία 1", "Κατηγορία 2"], "custom_tags": ["Ειδική Ετικέτα 1"]}}"""

        async def _try_command_code():
            if not COMMAND_CODE_API_KEY: return None
            url = "https://api.commandcode.ai/provider/v1/chat/completions"
            headers = {"Authorization": f"Bearer {COMMAND_CODE_API_KEY}", "Content-Type": "application/json"}
            req_body = {"model": COMMAND_CODE_MODEL, "messages": [{"role": "user", "content": prompt}], "temperature": 0.1, "response_format": {"type": "json_object"}}
            resp = await client.post(url, headers=headers, json=req_body, timeout=30.0)
            if resp.status_code == 200:
                res_json = resp.json()
                choices = res_json.get("choices", [])
                if choices:
                    text = choices[0].get("message", {}).get("content", "").strip()
                    return text
            return None
            
        async def _try_fallback():
            if not GEMINI_API_KEY: return None
            url = f"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key={GEMINI_API_KEY}"
            req_body = {"contents": [{"parts": [{"text": prompt}]}], "generationConfig": {"temperature": 0.1, "responseMimeType": "application/json"}}
            resp = await client.post(url, json=req_body, timeout=30.0)
            if resp.status_code == 200:
                res_json = resp.json()
                cands = res_json.get("candidates", [])
                if cands:
                    return cands[0].get("content", {}).get("parts", [{}])[0].get("text", "").strip()
            return None

        text = await _try_command_code()
        if text is None:
            text = await _try_fallback()
            if text is not None:
                fallback_used_globally = True
            
        if text:
            text = text.replace("```json", "").replace("```", "").strip()
            start_idx = text.find('{')
            end_idx = text.rfind('}')
            if start_idx != -1 and end_idx != -1 and end_idx > start_idx:
                text = text[start_idx:end_idx+1]
            try:
                data = json.loads(text)
                if isinstance(data, dict):
                    for k in ["response", "text", "content", "answer"]:
                        if k in data and isinstance(data[k], str) and "topic" in data[k]:
                            try:
                                inner_data = json.loads(data[k])
                                if "topic" in inner_data: data = inner_data; break
                            except: pass
                if isinstance(data, dict) and data.get("topic"):
                    result = {"topic": data.get("topic", topic).strip(), "core_tags": data.get("core_tags", ["Διάφορα"]), "custom_tags": data.get("custom_tags", [])}
                    _cache_set(_NAMING_CACHE, cache_key, result)
                    return result
            except Exception as je:
                print(f"LLM json error on text {repr(text)}: {je}")
    except Exception as e:
        import traceback; traceback.print_exc()
    result = {"topic": topic, "core_tags": ["Διάφορα"], "custom_tags": []}
    _cache_set(_NAMING_CACHE, cache_key, result)
    return result


    try:
        articles_str = "\n".join([f"- {a['title']} ({a.get('feed', '')})" for a in articles])
        prompt = f"""Είσαι βοηθός ονοματοδοσίας και κατηγοριοποίησης ειδήσεων. 
Έχω μια ομάδα άρθρων που αναφέρονται στο ίδιο γεγονός. Ο προσωρινός τίτλος είναι: "{topic}".

Παρακάτω είναι τα άρθρα:
{articles_str}

Αποστολή σου:
1. Γράψε έναν κατάλληλο, ελκυστικό, πολύ σύντομο (έως 8-10 λέξεις) τίτλο θεματικής (Topic) στα ΕΛΛΗΝΙΚΑ.
2. Επίλεξε από 1 έως 3 κατηγορίες από την παρακάτω αυστηρή λίστα (Core Categories):
{json.dumps(CORE_CATEGORIES, ensure_ascii=False)}
3. Αν θέλεις, πρόσθεσε έως 2 δικές σου ειδικές ετικέτες (Custom Tags) που περιγράφουν πιο συγκεκριμένα το θέμα. ΕΙΔΙΚΑ αν επιλέξεις την κατηγορία "Νέες Κυκλοφορίες (Μουσική)", ΠΡΕΠΕΙ ΟΠΩΣΔΗΠΟΤΕ να προσθέσεις το μουσικό είδος (π.χ. "Rock", "Pop", "Hip Hop") στις ειδικές ετικέτες.

Απάντησε ΜΟΝΟ με το παρακάτω JSON format:
{{"topic": "Ο Ελληνικός Τίτλος", "core_tags": ["Κατηγορία 1", "Κατηγορία 2"], "custom_tags": ["Ειδική Ετικέτα 1"]}}"""

        url = "https://api.commandcode.ai/provider/v1/chat/completions"
        headers = {
            "Authorization": f"Bearer {COMMAND_CODE_API_KEY}",
            "Content-Type": "application/json"
        }
        req_body = {
            "model": "deepseek/deepseek-v4-flash-fast",
            "messages": [{"role": "user", "content": prompt}],
            "temperature": 0.1,
            "response_format": {"type": "json_object"}
        }
        resp = await client.post(url, headers=headers, json=req_body, timeout=30.0)
        if resp.status_code == 200:
            res_json = resp.json()
            choices = res_json.get("choices", [])
            if choices:
                text = choices[0].get("message", {}).get("content", "").strip()
                text = text.replace("```json", "").replace("```", "").strip()
                
                # Extract valid JSON between first { and last }
                start_idx = text.find('{')
                end_idx = text.rfind('}')
                if start_idx != -1 and end_idx != -1 and end_idx > start_idx:
                    text = text[start_idx:end_idx+1]
                
                try:
                    data = json.loads(text)
                    if isinstance(data, dict):
                        # Deepseek often wraps the requested JSON in a "response" or "text" key
                        for k in ["response", "text", "content", "answer"]:
                            if k in data and isinstance(data[k], str) and "topic" in data[k]:
                                try:
                                    inner_data = json.loads(data[k])
                                    if "topic" in inner_data:
                                        data = inner_data
                                        break
                                except:
                                    pass

                    if isinstance(data, dict) and data.get("topic"):
                        return {
                            "topic": data.get("topic", topic).strip(),
                            "core_tags": data.get("core_tags", ["Διάφορα"]),
                            "custom_tags": data.get("custom_tags", [])
                        }
                except Exception as je:
                    print(f"LLM json error on text {repr(text)}: {je}")
    except Exception as e:
        import traceback; traceback.print_exc()
    return {"topic": topic, "core_tags": ["Διάφορα"], "custom_tags": []}


async def call_batch_categorize(articles, client):
    global fallback_used_globally
    if (not COMMAND_CODE_API_KEY and not GEMINI_API_KEY) or not articles:
        return [{"core_tags": ["Διάφορα"], "custom_tags": []} for _ in articles]
        
    signatures = [_article_signature(a) for a in articles]
    cached_results = [None for _ in articles]
    missing_indices = []
    for i, article in enumerate(articles):
        cached = _cache_get(_SINGLETON_CACHE, signatures[i])
        if cached is not None:
            cached_results[i] = cached
        else:
            missing_indices.append(i)

    if not missing_indices:
        return [cached_results[i] for i in range(len(articles))]

    articles_to_process = [articles[i] for i in missing_indices]
    missing_results = [{"core_tags": ["Διάφορα"], "custom_tags": []} for _ in articles_to_process]

    batch_size = 30
    max_concurrent = 10
    semaphore = asyncio.Semaphore(max_concurrent)

    async def process_batch(batch_start, batch_articles):
        global fallback_used_globally
        async with semaphore:
            try:
                articles_str = ""
                for i, a in enumerate(batch_articles):
                    articles_str += f"\nΆρθρο {i}:\nΤίτλος: {a['title']}\nΚείμενο: {a['text'][:200]}\n"
                prompt = f"""Είσαι βοηθός κατηγοριοποίησης ειδήσεων. Έχω μια λίστα από ανεξάρτητα άρθρα.
Για το καθένα:
1. Επίλεξε από 1 έως 3 κατηγορίες από την παρακάτω αυστηρή λίστα (Core Categories):
{json.dumps(CORE_CATEGORIES, ensure_ascii=False)}
2. Πρόσθεσε προαιρετικά έως 2 δικές σου ειδικές ετικέτες (Custom Tags).

Απάντησε ΜΟΝΟ με ένα JSON object όπου τα keys είναι οι αριθμοί των άρθρων (0 έως {len(batch_articles)-1}) και τα values είναι αντικείμενα με "core_tags" και "custom_tags". Παράδειγμα:
{{"0": {{"core_tags": ["Ελλάδα"], "custom_tags": ["Αθήνα"]}}, "1": {{"core_tags": ["Μουσική", "Νέες Κυκλοφορίες (Μουσική)"], "custom_tags": ["Rock"]}}}}
            
Λίστα Άρθρων:
{articles_str}"""

                async def _try_command_code():
                    if not COMMAND_CODE_API_KEY: return None
                    url = "https://api.commandcode.ai/provider/v1/chat/completions"
                    headers = {"Authorization": f"Bearer {COMMAND_CODE_API_KEY}", "Content-Type": "application/json"}
                    req_body = {"model": COMMAND_CODE_MODEL, "messages": [{"role": "user", "content": prompt}], "temperature": 0.0, "response_format": {"type": "json_object"}}
                    resp = await client.post(url, headers=headers, json=req_body, timeout=180.0)
                    if resp.status_code == 200:
                        res_json = resp.json()
                        choices = res_json.get("choices", [])
                        if choices:
                            return choices[0].get("message", {}).get("content", "").strip()
                    return None

                async def _try_fallback():
                    if not GEMINI_API_KEY: return None
                    url = f"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key={GEMINI_API_KEY}"
                    req_body = {"contents": [{"parts": [{"text": prompt}]}], "generationConfig": {"temperature": 0.0, "responseMimeType": "application/json"}}
                    resp = await client.post(url, json=req_body, timeout=180.0)
                    if resp.status_code == 200:
                        res_json = resp.json()
                        cands = res_json.get("candidates", [])
                        if cands:
                            return cands[0].get("content", {}).get("parts", [{}])[0].get("text", "").strip()
                    return None

                text = await _try_command_code()
                if text is None:
                    text = await _try_fallback()
                    if text is not None:
                        fallback_used_globally = True
                        
                if text:
                    text = text.replace("```json", "").replace("```", "").strip()
                    try:
                        data = json.loads(text)
                        for k, v in data.items():
                            idx = int(k)
                            if 0 <= idx < len(batch_articles):
                                c_tags = v.get("core_tags", ["Διάφορα"]) if isinstance(v, dict) else ["Διάφορα"]
                                u_tags = v.get("custom_tags", []) if isinstance(v, dict) else []
                                missing_results[batch_start + idx] = {"core_tags": c_tags, "custom_tags": u_tags}
                    except Exception as je:
                        print(f"LLM batch json error: {je}")
            except Exception as e:
                print(f"LLM batch class error: {type(e).__name__} - {e}")

    tasks = [process_batch(b, articles_to_process[b:min(b + batch_size, len(articles_to_process))]) for b in range(0, len(articles_to_process), batch_size)]
    await asyncio.gather(*tasks)

    for pos, original_index in enumerate(missing_indices):
        cached_results[original_index] = missing_results[pos]
        _cache_set(_SINGLETON_CACHE, signatures[original_index], missing_results[pos])

    return [cached_results[i] for i in range(len(articles))]


def _same_script_matrix(texts) -> np.ndarray:
    """True[i, j] when texts[i] and texts[j] are written in the same script (e.g. both Greek).
    Cross-script pairs (translated duplicates) must never be vetoed by the lexical-overlap
    check, since their titles share no words by construction."""
    def script(text: str) -> str:
        greek = sum(1 for c in text if '\u0370' <= c <= '\u03ff' or '\u1f00' <= c <= '\u1fff')
        latin = sum(1 for c in text if c.isascii() and c.isalpha())
        return 'GR' if greek >= latin else 'EN'

    scripts = [script(text) for text in texts]
    return np.array([[a == b for b in scripts] for a in scripts], dtype=bool)

def cluster_super_group(articles, precomputed_embeddings=None):
    distance_threshold = float(os.getenv("AI_DISTANCE_THRESHOLD", "0.45"))
    if len(articles) < 2:
        if articles and model is not None:
            content_words = articles[0].get('text', '').split()[:50]
            content_snippet = " ".join(content_words)
            text = f"{articles[0].get('title', '').strip()} {content_snippet}".strip()
            emb = np.array(list(model.embed([text], batch_size=16)))[0]
            norm = np.linalg.norm(emb)
            articles[0]["_emb"] = emb / (norm or 1.0)
        return [[a] for a in articles]

    texts = []
    for a in articles:
        content_words = a.get('text', '').split()[:50]
        content_snippet = " ".join(content_words)
        texts.append(f"{a['title'].strip()} {content_snippet}".strip())
    
    t0 = time.time()
    
    if precomputed_embeddings is not None:
        embeddings_list = [None] * len(articles)
        missing_texts = []
        missing_indices = []
        
        for i, a in enumerate(articles):
            a_id = str(a.get('id', ''))
            if a_id and a_id in precomputed_embeddings:
                embeddings_list[i] = precomputed_embeddings[a_id]
            else:
                missing_texts.append(texts[i])
                missing_indices.append(i)
                
        if missing_texts:
            embeddings_gen = model.embed(missing_texts, batch_size=16)
            missing_embeddings = list(embeddings_gen)
            for idx, emb in zip(missing_indices, missing_embeddings):
                embeddings_list[idx] = emb
                
        embeddings = np.array(embeddings_list)
        reused = len(articles) - len(missing_texts)
        print(f"Embeddings: reused {reused}, computed {len(missing_texts)}")
    else:
        embeddings_gen = model.embed(texts, batch_size=16)
        embeddings = np.array(list(embeddings_gen))
        
    t1 = time.time()
    
    emb_norms = np.linalg.norm(embeddings, axis=1, keepdims=True)
    norm_embeddings = embeddings / np.where(emb_norms == 0, 1e-12, emb_norms)
    for i, a in enumerate(articles):
        a["_emb"] = norm_embeddings[i]
    
    dense_distances = cosine_distances(embeddings)
    t2 = time.time()
    
    try:
        lemmatized_texts = []
        for doc in nlp.pipe(texts, disable=["parser"]):
            lemmas = [t.lemma_ for t in doc if not t.is_punct and not t.is_stop]
            ents = [ent.text.replace(" ", "_") for ent in doc.ents]
            enriched = lemmas + ents * 5
            lemmatized_texts.append(" ".join(enriched))

        vectorizer = TfidfVectorizer(analyzer='word', token_pattern=r'(?u)\b\w+\b')
        tfidf_matrix = vectorizer.fit_transform(lemmatized_texts)
        sparse_distances = cosine_distances(tfidf_matrix)
    except Exception as e:
        print(f"TFIDF/spaCy error: {e}")
        sparse_distances = np.ones((len(texts), len(texts)))
        np.fill_diagonal(sparse_distances, 0)
        
    t3 = time.time()
    alpha = 0.7
    hybrid_distances = alpha * dense_distances + (1 - alpha) * sparse_distances

    # STRONG MATCH OVERRIDE:
    # If the AI embedding is highly confident (distance < 0.35), trust it entirely.
    # This solves edge cases like transliterated names (e.g. "Hayden Panettiere" vs "Χέιντεν Πανετιέρ")
    # where the AI knows they are the same, but the TF-IDF keyword matcher applies a massive penalty.
    #
    # VETO for same-language pairs with zero lexical overlap: when two titles share no
    # significant words at all (TF-IDF distance > 0.90), a sub-0.35 embedding distance is
    # usually "same domain, different story" (e.g. two minister quotes on different subjects,
    # or two different matches in the same league). Cross-language pairs keep the override —
    # their TF-IDF distance is ~1.0 by construction, and embedding-only matching is the
    # only way to detect translated duplicates. Calibrated Aug 2026 on ~3700 real pairs:
    # the veto removed 139 false merges and re-united 27 true ones on a live 748-article batch.
    same_script_mask = _same_script_matrix(texts)
    lexical_overlap_mask = sparse_distances <= 0.80
    strong_match_mask = dense_distances < 0.35
    vetoed_match_mask = strong_match_mask & same_script_mask & ~lexical_overlap_mask
    apply_mask = strong_match_mask & ~vetoed_match_mask
    hybrid_distances[apply_mask] = dense_distances[apply_mask]

    clustering = AgglomerativeClustering(
        n_clusters=None,
        metric='precomputed',
        linkage='average',
        distance_threshold=distance_threshold
    )
    labels = clustering.fit_predict(hybrid_distances)
    t4 = time.time()
    print(f"cluster_super_group({len(texts)}): Embed: {t1-t0:.2f}s, Dense: {t2-t1:.2f}s, TFIDF: {t3-t2:.2f}s, Cluster: {t4-t3:.2f}s")

    groups_map = defaultdict(list)
    for idx, label in enumerate(labels):
        groups_map[int(label)].append(articles[idx])

    return list(groups_map.values())

@app.post("/embed")
def embed_articles(data: dict):
    articles = data.get("articles", [])
    embeddings_dict = {}
    
    if not articles:
        return {"embeddings": embeddings_dict}
        
    texts = []
    ids = []
    for a in articles:
        content = extract_text(a.get("content", ""))
        content_words = content.split()[:50]
        content_snippet = " ".join(content_words)
        texts.append(f"{a.get('title', '').strip()} {content_snippet}".strip())
        ids.append(str(a.get("id", "")))
        
    embeddings_gen = model.embed(texts, batch_size=16)
    embeddings = list(embeddings_gen)
    
    for a_id, emb in zip(ids, embeddings):
        if a_id:
            embeddings_dict[a_id] = emb.tolist()
            
    return {"embeddings": embeddings_dict}

import unicodedata

def strip_accents(text):
    """Strip diacritical marks (accents) from Greek text."""
    nfkd = unicodedata.normalize('NFD', text)
    return ''.join(c for c in nfkd if unicodedata.category(c) != 'Mn')

@app.post("/search-embed")
def search_embed(data: dict):
    query = data.get("query", "")
    if not query:
        return {"embeddings": []}
    
    normalized = strip_accents(query)
    
    # Embed both original and accent-stripped versions
    texts = [query]
    if normalized != query:
        texts.append(normalized)
    
    embeddings_gen = model.embed(texts)
    embeddings = [e.tolist() for e in embeddings_gen]
    
    return {"embeddings": embeddings}


def process_groups_embedding_only(all_clusters):
    category_embs = get_cat_embs()
    processed_groups = []

    for cluster_arts in all_clusters:
        cluster_arts.sort(key=lambda x: x.get("published_at", ""), reverse=True)

        valid_embs = [a["_emb"] for a in cluster_arts if "_emb" in a]
        if len(valid_embs) == len(cluster_arts) and len(valid_embs) > 0:
            cluster_embs = np.array(valid_embs)
            centroid = np.mean(cluster_embs, axis=0, keepdims=True)
            centroid_norm = centroid / (np.linalg.norm(centroid) or 1.0)
        else:
            texts = [f"{a.get('title', '')} {' '.join(extract_text(a.get('content', '')).split()[:40])}".strip() for a in cluster_arts]
            if model is not None:
                cluster_embs = np.array(list(model.embed(texts, batch_size=16)))
                t_norms = np.linalg.norm(cluster_embs, axis=1, keepdims=True)
                cluster_embs = cluster_embs / np.where(t_norms == 0, 1e-12, t_norms)
                centroid = np.mean(cluster_embs, axis=0, keepdims=True)
                centroid_norm = centroid / (np.linalg.norm(centroid) or 1.0)
            else:
                cluster_embs = np.zeros((len(cluster_arts), 1))
                centroid_norm = np.zeros((1, 1))

        # 1. Topic Naming via Medoid Selection
        if len(cluster_arts) == 1:
            chosen_title = clean_title(cluster_arts[0].get("title", ""), cluster_arts[0].get("feed", ""))
        else:
            sims = (cluster_embs @ centroid_norm.T).flatten()
            best_idx = int(np.argmax(sims))
            chosen_title = clean_title(cluster_arts[best_idx].get("title", ""), cluster_arts[best_idx].get("feed", ""))
            if not chosen_title:
                chosen_title = cluster_arts[0].get("title", "")

        # 2. Core Category Tagging via Centroid vs CORE_CATEGORIES
        if category_embs is not None and centroid_norm.shape[1] == category_embs.shape[1]:
            cat_sims = (category_embs @ centroid_norm.T).flatten()
            ranked_cats = sorted(range(len(CORE_CATEGORIES)), key=lambda j: float(cat_sims[j]), reverse=True)
            top_score = float(cat_sims[ranked_cats[0]])

            chosen_cats = []
            for j in ranked_cats:
                if len(chosen_cats) >= 3:
                    break
                if float(cat_sims[j]) >= 0.25 and (not chosen_cats or (top_score - float(cat_sims[j]) <= 0.07)):
                    chosen_cats.append(CORE_CATEGORIES[j])
                else:
                    break
            if not chosen_cats:
                chosen_cats = [CORE_CATEGORIES[ranked_cats[0]]]
        else:
            chosen_cats = ["Διάφορα"]

        # 3. Custom Tags via Named Entities from the chosen topic
        custom_tags = []
        if len(cluster_arts) > 1 and nlp is not None:
            try:
                doc = nlp(chosen_title)
                seen_tags = set(chosen_cats)
                for ent in doc.ents:
                    ent_text = ent.text.strip()
                    if (len(ent_text) > 3 
                        and not ent_text.isdigit() 
                        and ent_text not in seen_tags 
                        and ent_text.lower() not in {"βίντεο", "εικόνες", "φωτογραφίες"}):
                        custom_tags.append(ent_text)
                        seen_tags.add(ent_text)
                        if len(custom_tags) >= 2:
                            break
            except Exception:
                pass

        # Clean internal _emb before returning
        for a in cluster_arts:
            a.pop("_emb", None)

        group_data = {
            "topic": chosen_title,
            "articles": cluster_arts,
            "count": len(cluster_arts),
            "core_tags": chosen_cats,
            "custom_tags": custom_tags,
            "tags": chosen_cats + custom_tags
        }
        processed_groups.append(group_data)

    return processed_groups


@app.post("/cluster")
def cluster(data: dict):
    global fallback_used_globally
    fallback_used_globally = False
    raw_articles = data.get("articles", [])
    precomputed = data.get("embeddings", None)

    if not raw_articles:
        return {"categories": [], "total": 0, "generated": time.strftime("%H:%M:%S"),
        "is_fallback": fallback_used_globally}

    articles = []
    for a in raw_articles:
        a["text"] = extract_text(a.get("content", ""))
        articles.append(a)

    all_clusters = cluster_super_group(articles, precomputed_embeddings=precomputed) if articles else []

    t_proc_start = time.time()
    all_groups = process_groups_embedding_only(all_clusters)
    print(f"Local embedding naming and categorization took: {time.time() - t_proc_start:.2f}s for {len(all_groups)} groups")

    category_groups = defaultdict(list)
    total_articles = 0
    
    # If API key is missing or failed, ensure fallback logic
    for group in all_groups:
        if not group.get("core_tags"):
            group["core_tags"] = ["Διάφορα"]
        if not group.get("tags"):
            group["tags"] = group["core_tags"] + group.get("custom_tags", [])
            
        # Add the group to every core category it was tagged with
        for cat in group["core_tags"]:
            if cat not in CORE_CATEGORIES:
                cat = "Διάφορα"
            category_groups[cat].append(group)
        total_articles += group["count"]

    # Deduplicate total_articles correctly (since groups are duplicated in categories)
    unique_articles = sum(group["count"] for group in all_groups)

    for cat in category_groups:
        category_groups[cat].sort(key=lambda g: g["count"], reverse=True)

    output = {
        "categories": [
            {
                "category": cat,
                "groups": category_groups.get(cat, []),
                "count": len(category_groups.get(cat, []))
            }
            for cat in CORE_CATEGORIES
            if len(category_groups.get(cat, [])) > 0
        ],
        "total": unique_articles,
        "generated": time.strftime("%H:%M:%S"),
        "is_fallback": fallback_used_globally
    }
    return output
