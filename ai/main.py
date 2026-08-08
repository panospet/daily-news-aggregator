import os
import time
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

GEMINI_API_KEY = os.getenv("GEMINI_API_KEY", "")

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

async def call_gemini_naming(topic, articles, client):
    if not GEMINI_API_KEY:
        return {"topic": topic, "core_tags": ["Διάφορα"], "custom_tags": []}

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
3. Αν θέλεις, πρόσθεσε έως 2 δικές σου ειδικές ετικέτες (Custom Tags) που περιγράφουν πιο συγκεκριμένα το θέμα.

Απάντησε ΜΟΝΟ με το παρακάτω JSON format:
{{"topic": "Ο Ελληνικός Τίτλος", "core_tags": ["Κατηγορία 1", "Κατηγορία 2"], "custom_tags": ["Ειδική Ετικέτα 1"]}}"""

        url = f"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash-lite:generateContent?key={GEMINI_API_KEY}"
        req_body = {
            "contents": [{"parts": [{"text": prompt}]}],
            "generationConfig": {
                "temperature": 0.1,
                "responseMimeType": "application/json"
            }
        }
        resp = await client.post(url, json=req_body, timeout=8.0)
        if resp.status_code == 200:
            res_json = resp.json()
            parts = res_json.get("candidates", [{}])[0].get("content", {}).get("parts", [{}])
            if parts and parts[0].get("text"):
                text = parts[0]["text"].strip()
                text = text.replace("```json", "").replace("```", "").strip()
                
                # Extract valid JSON between first { and last }
                start_idx = text.find('{')
                end_idx = text.rfind('}')
                if start_idx != -1 and end_idx != -1 and end_idx > start_idx:
                    text = text[start_idx:end_idx+1]
                
                try:
                    data = json.loads(text)
                    if isinstance(data, dict) and data.get("topic"):
                        return {
                            "topic": data.get("topic", topic).strip(),
                            "core_tags": data.get("core_tags", ["Διάφορα"]),
                            "custom_tags": data.get("custom_tags", [])
                        }
                except Exception as je:
                    print(f"Gemini json error on text {repr(text)}: {je}")
    except Exception as e:
        print(f"Gemini error: {e}")
    return {"topic": topic, "core_tags": ["Διάφορα"], "custom_tags": []}

async def call_gemini_batch_categorize(articles, client):
    if not GEMINI_API_KEY or not articles:
        return [{"core_tags": ["Διάφορα"], "custom_tags": []} for _ in articles]
        
    results = [{"core_tags": ["Διάφορα"], "custom_tags": []} for _ in articles]
    batch_size = 30
    
    async def process_batch(batch_start, batch_articles):
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

            url = f"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash-lite:generateContent?key={GEMINI_API_KEY}"
            req_body = {
                "contents": [{"parts": [{"text": prompt}]}],
                "generationConfig": {
                    "temperature": 0.0,
                    "responseMimeType": "application/json"
                }
            }
            resp = await client.post(url, json=req_body, timeout=15.0)
            
            if resp.status_code == 200:
                res_json = resp.json()
                parts = res_json.get("candidates", [{}])[0].get("content", {}).get("parts", [{}])
                if parts and parts[0].get("text"):
                    text = parts[0]["text"].strip()
                    text = text.replace("```json", "").replace("```", "").strip()
                    try:
                        data = json.loads(text)
                        for k, v in data.items():
                            idx = int(k)
                            if 0 <= idx < len(batch_articles):
                                c_tags = v.get("core_tags", ["Διάφορα"]) if isinstance(v, dict) else ["Διάφορα"]
                                u_tags = v.get("custom_tags", []) if isinstance(v, dict) else []
                                results[batch_start + idx] = {"core_tags": c_tags, "custom_tags": u_tags}
                    except Exception as je:
                        print(f"Gemini batch json error: {je}")
        except Exception as e:
            print(f"Gemini batch class error: {e}")

    for batch_start in range(0, len(articles), batch_size):
        batch_end = min(batch_start + batch_size, len(articles))
        await process_batch(batch_start, articles[batch_start:batch_end])
        
    return results

def cluster_super_group(articles):
    distance_threshold = float(os.getenv("AI_DISTANCE_THRESHOLD", "0.45"))
    if len(articles) < 2:
        return [[a] for a in articles]

    texts = []
    for a in articles:
        content_words = a.get('text', '').split()[:50]
        content_snippet = " ".join(content_words)
        texts.append(f"{a['title'].strip()} {content_snippet}".strip())
    
    t0 = time.time()
    embeddings_gen = model.embed(texts, batch_size=45)
    embeddings = np.array(list(embeddings_gen))
    t1 = time.time()
    
    dense_distances = cosine_distances(embeddings)
    t2 = time.time()
    
    try:
        lemmatized_texts = []
        for doc in nlp.pipe(texts, disable=["parser"]):
            lemmas = [t.lemma_ for t in doc if not t.is_punct and not t.is_stop]
            ents = [ent.text.replace(" ", "_") for ent in doc.ents]
            enriched = lemmas + ents * 2
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

@app.post("/cluster")
def cluster(data: dict):
    raw_articles = data.get("articles", [])

    if not raw_articles:
        return {"categories": [], "total": 0, "generated": time.strftime("%H:%M:%S")}

    articles = []
    for a in raw_articles:
        a["text"] = extract_text(a.get("content", ""))
        articles.append(a)

    all_clusters = cluster_super_group(articles) if articles else []

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

    if GEMINI_API_KEY:
        t_gemini_start = time.time()
        
        async def fetch_all_ai_data():
            async with httpx.AsyncClient() as client:
                async def fetch_name(g):
                    result = await call_gemini_naming(g["topic"], g["articles"], client)
                    g["topic"] = result["topic"]
                    g["core_tags"] = result.get("core_tags", ["Διάφορα"])
                    g["custom_tags"] = result.get("custom_tags", [])
                    g["tags"] = g["core_tags"] + g["custom_tags"]
                
                tasks = [fetch_name(g) for g in clusters_to_name]
                
                # Singletons categorization
                if singletons:
                    articles_for_batch = [s["articles"][0] for s in singletons]
                    async def fetch_singletons():
                        results = await call_gemini_batch_categorize(articles_for_batch, client)
                        for i, s in enumerate(singletons):
                            s["core_tags"] = results[i].get("core_tags", ["Διάφορα"])
                            s["custom_tags"] = results[i].get("custom_tags", [])
                            s["tags"] = s["core_tags"] + s["custom_tags"]
                    tasks.append(fetch_singletons())
                    
                await asyncio.gather(*tasks)
        
        asyncio.run(fetch_all_ai_data())
        print(f"Gemini processing took: {time.time() - t_gemini_start:.2f}s")

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
        "generated": time.strftime("%H:%M:%S")
    }
    return output
