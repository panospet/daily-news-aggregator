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

# Global embedding model (loaded on startup)
model = None
nlp = None

@asynccontextmanager
async def lifespan(app: FastAPI):
    global model, nlp
    print("Loading embedding model...")
    model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-mpnet-base-v2')
    print("Loading spaCy model...")
    import spacy
    nlp = spacy.load("el_core_news_md")
    print("Models loaded successfully.")
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
            req_body = {"model": "MiniMaxAI/MiniMax-M3", "messages": [{"role": "user", "content": prompt}], "temperature": 0.1, "response_format": {"type": "json_object"}}
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
                    req_body = {"model": "MiniMaxAI/MiniMax-M3", "messages": [{"role": "user", "content": prompt}], "temperature": 0.0, "response_format": {"type": "json_object"}}
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

    clusters_to_name = []
    singletons = []
    for cluster_arts in all_clusters:
        cluster_arts.sort(key=lambda x: x.get("published_at", ""), reverse=True)
        group_data = {
            "topic": cluster_arts[0]["title"],
            "articles": cluster_arts,
            "count": len(cluster_arts),
            "core_tags": [],
            "custom_tags": [],
            "tags": []
        }
        if len(cluster_arts) > 1:
            clusters_to_name.append(group_data)
        else:
            singletons.append(group_data)

    if COMMAND_CODE_API_KEY or GEMINI_API_KEY:
        t_llm_start = time.time()
        
        async def fetch_all_ai_data():
            async with httpx.AsyncClient() as client:
                async def fetch_name(g):
                    result = await call_naming(g["topic"], g["articles"], client)
                    g["topic"] = result["topic"]
                    g["core_tags"] = result.get("core_tags", ["Διάφορα"])
                    g["custom_tags"] = result.get("custom_tags", [])
                    g["tags"] = g["core_tags"] + g["custom_tags"]
                
                tasks = [fetch_name(g) for g in clusters_to_name]
                
                # Singletons categorization
                if singletons:
                    articles_for_batch = [s["articles"][0] for s in singletons]
                    async def fetch_singletons():
                        results = await call_batch_categorize(articles_for_batch, client)
                        for i, s in enumerate(singletons):
                            s["core_tags"] = results[i].get("core_tags", ["Διάφορα"])
                            s["custom_tags"] = results[i].get("custom_tags", [])
                            s["tags"] = s["core_tags"] + s["custom_tags"]
                    tasks.append(fetch_singletons())
                    
                await asyncio.gather(*tasks)
        
        asyncio.run(fetch_all_ai_data())
        print(f"Gemini processing took: {time.time() - t_llm_start:.2f}s")

    category_groups = defaultdict(list)
    total_articles = 0

    all_groups = clusters_to_name + singletons
    
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
