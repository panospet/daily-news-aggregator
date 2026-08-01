import os
import time
import re
import requests
import numpy as np
from fastapi import FastAPI, Request
from collections import defaultdict
from sklearn.cluster import AgglomerativeClustering
from fastembed import TextEmbedding
import concurrent.futures
from contextlib import asynccontextmanager

GEMINI_API_KEY = os.getenv("GEMINI_API_KEY", "")

NEW_RELEASE_PATTERN = re.compile(
    r'\b(?:νέο τραγούδι|νέο album|νέο άλμπουμ|κυκλοφόρησε|κυκλοφορεί|νέο single|βίντεο κλιπ|video clip|πρεμιέρα|new album|new single|new track|new song|releases|released|drops|out now|premiere)\b',
    re.IGNORECASE
)

PREFERRED_ORDER = [
    "Ελληνική ειδησεογραφία",
    "Ξένη ειδησεογραφία",
    "Αθλητικές ειδήσεις",
    "Νέες μουσικές κυκλοφορίες",
    "Μουσικά νέα",
    "Διάφορες ειδήσεις",
]

def is_greek(text: str) -> bool:
    # Check for presence of greek characters (U+0370 to U+03FF)
    return any('\u0370' <= c <= '\u03FF' for c in text)

def assign_category(article: dict) -> str:
    # Feed category from Miniflux
    base_cat = article.get("category", "").lower()
    title = article.get("title", "")
    content = article.get("text", "")
    combined_text = f"{title} {content}"

    if base_cat in ["sports", "panionara"]:
        return "Αθλητικές ειδήσεις"
    
    if base_cat == "music":
        if NEW_RELEASE_PATTERN.search(combined_text):
            return "Νέες μουσικές κυκλοφορίες"
        return "Μουσικά νέα"

    # For anything else (international, news, politics, random, etc)
    if is_greek(combined_text):
        return "Ελληνική ειδησεογραφία"
    
    # If no greek letters, assume foreign news
    return "Ξένη ειδησεογραφία"

# Global embedding model (loaded on startup)
model = None

@asynccontextmanager
async def lifespan(app: FastAPI):
    global model
    print("Loading embedding model...")
    model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-mpnet-base-v2')
    print("Model loaded successfully.")
    yield
    print("Shutting down AI service.")

app = FastAPI(lifespan=lifespan)

def extract_text(html_content):
    if not html_content:
        return ""
    plain_text = re.sub(r'<[^>]+>', '', html_content)
    plain_text = re.sub(r'\s+', ' ', plain_text).strip()[:1000]
    return plain_text

def call_gemini_naming(topic, articles):
    if not GEMINI_API_KEY:
        return topic

    try:
        articles_str = "\n".join([f"- {a['title']} ({a['feed']})" for a in articles])
        prompt = f"""Είσαι βοηθός ονοματοδοσίας ειδήσεων. 
Έχω μια ομάδα άρθρων που αναφέρονται στο ίδιο γεγονός. Ο προσωρινός τίτλος είναι: "{topic}".

Παρακάτω είναι τα άρθρα:
{articles_str}

Γράψε έναν κατάλληλο, ελκυστικό, πολύ σύντομο (έως 8-10 λέξεις) τίτλο θεματικής (Topic) στα ΕΛΛΗΝΙΚΑ για αυτό το γκρουπ ειδήσεων, ο οποίος να αντικατοπτρίζει σωστά το περιεχόμενο.
Απάντησε ΜΟΝΟ με το JSON format:
{{"topic": "Ο Ελληνικός Τίτλος"}}"""

        url = f"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash-lite:generateContent?key={GEMINI_API_KEY}"
        req_body = {
            "contents": [{"parts": [{"text": prompt}]}],
            "generationConfig": {
                "temperature": 0.1,
                "responseMimeType": "application/json"
            }
        }
        resp = requests.post(url, json=req_body, timeout=5)
        if resp.status_code == 200:
            res_json = resp.json()
            parts = res_json.get("candidates", [{}])[0].get("content", {}).get("parts", [{}])
            if parts and parts[0].get("text"):
                text = parts[0]["text"].strip()
                import json
                text = text.replace("```json", "").replace("```", "").strip()
                try:
                    data = json.loads(text)
                    if isinstance(data, dict) and data.get("topic"):
                        return data["topic"].strip()
                except Exception as je:
                    print(f"Gemini json error on text {repr(text)}: {je}")
    except Exception as e:
        print(f"Gemini error: {e}")
    return topic

def cluster_super_group(articles):
    distance_threshold = float(os.getenv("AI_DISTANCE_THRESHOLD", "0.45"))
    if len(articles) < 2:
        return [[a] for a in articles]

    texts = []
    for a in articles:
        content_words = a.get('text', '').split()[:50]
        content_snippet = " ".join(content_words)
        texts.append(f"{a['title'].strip()} {content_snippet}".strip())
    
    embeddings_gen = model.embed(texts, batch_size=16)
    embeddings = np.array(list(embeddings_gen))
    norms = np.linalg.norm(embeddings, axis=1, keepdims=True)
    embeddings = embeddings / np.where(norms == 0, 1e-12, norms)

    clustering = AgglomerativeClustering(
        n_clusters=None,
        metric='cosine',
        linkage='complete',
        distance_threshold=distance_threshold
    )
    labels = clustering.fit_predict(embeddings)

    groups_map = defaultdict(list)
    for idx, label in enumerate(labels):
        groups_map[int(label)].append(articles[idx])

    return list(groups_map.values())

@app.post("/cluster")
def cluster(data: dict):
    raw_articles = data.get("articles", [])

    if not raw_articles:
        return {"categories": [], "total": 0, "generated": time.strftime("%H:%M:%S")}

    # Parse and clean articles
    articles = []
    for a in raw_articles:
        a["text"] = extract_text(a.get("content", ""))
        articles.append(a)

    super_buckets = defaultdict(list)
    for a in articles:
        super_cat = assign_category(a)
        super_buckets[super_cat].append(a)

    all_clusters = []
    for super_cat, super_arts in super_buckets.items():
        if len(super_arts) == 0:
            continue
        clusters = cluster_super_group(super_arts)
        all_clusters.extend(clusters)

    category_groups = defaultdict(list)
    total_articles = 0

    for cluster_arts in all_clusters:
        counts = defaultdict(int)
        for a in cluster_arts:
            display_name = assign_category(a)
            counts[display_name] += 1
        majority_cat = max(counts, key=counts.get)

        # Sort by date (newest first)
        cluster_arts.sort(key=lambda x: x.get("published_at", ""), reverse=True)

        temp_topic = cluster_arts[0]["title"]
        group_data = {
            "topic": temp_topic,
            "articles": cluster_arts,
            "count": len(cluster_arts)
        }
        category_groups[majority_cat].append(group_data)
        total_articles += len(cluster_arts)

    for cat in category_groups:
        category_groups[cat].sort(key=lambda g: g["count"], reverse=True)

    clusters_to_name = []
    for cat in category_groups:
        for group in category_groups[cat]:
            if group["count"] > 1:
                clusters_to_name.append(group)
    
    if clusters_to_name and GEMINI_API_KEY:
        def fetch_name(g):
            new_topic = call_gemini_naming(g["topic"], g["articles"])
            g["topic"] = new_topic

        with concurrent.futures.ThreadPoolExecutor(max_workers=20) as executor:
            executor.map(fetch_name, clusters_to_name)

    output = {
        "categories": [
            {
                "category": cat,
                "groups": category_groups.get(cat, []),
                "count": len(category_groups.get(cat, []))
            }
            for cat in PREFERRED_ORDER
            if len(category_groups.get(cat, [])) > 0
        ],
        "total": total_articles,
        "generated": time.strftime("%H:%M:%S")
    }
    return output
