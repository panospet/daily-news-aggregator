import os
import sys
import time
import json
import psycopg2
# Redirect stderr to devnull to suppress warnings (warnings, collations, etc.)
sys.stderr = open(os.devnull, 'w')
import requests
from fastembed import TextEmbedding
from sklearn.cluster import AgglomerativeClustering
import numpy as np
from collections import defaultdict

# Load config from .env or default values
DB_HOST = os.getenv("MF_DB_HOST", "127.0.0.1")
DB_PORT = int(os.getenv("MF_DB_PORT", "5433"))
DB_NAME = os.getenv("MF_DB_NAME", "miniflux")
DB_USER = os.getenv("MF_DB_USER", "external_reader")
DB_PASS = os.getenv("MF_DB_PASSWORD", "")
GEMINI_API_KEY = os.getenv("GEMINI_API_KEY", "")

# Super-category groups for category-gated clustering.
# Articles from different super-categories are NEVER clustered together.
# This prevents sports articles from clustering with news, music from sports, etc.
SUPER_CATEGORIES = {
    "international": "news",
    "news": "news",
    "politics": "news",
    "sports": "sports",
    "panionara": "sports",
    "music": "music",
    "random": "random",
}

# Display names for each Miniflux category
CAT_DISPLAY = {
    "international": "ειδήσεις Κόσμος",
    "news": "ειδήσεις Ελλάδα",
    "sports": "αθλητικές ειδήσεις",
    "music": "μουσικά νέα",
    "politics": "ειδήσεις Ελλάδα",
    "random": "διάφορες ειδήσεις",
    "panionara": "αθλητικές ειδήσεις",
}

# Preferred display order
PREFERRED_ORDER = [
    "ειδήσεις Κόσμος",
    "ειδήσεις Ελλάδα",
    "αθλητικές ειδήσεις",
    "μουσικά νέα",
    "διάφορες ειδήσεις",
]


def fetch_articles(hours=12):
    """Fetch articles from database that are younger than X hours"""
    dsn = f"host={DB_HOST} port={DB_PORT} dbname={DB_NAME} user={DB_USER} password={DB_PASS} sslmode=disable"
    conn = psycopg2.connect(dsn)
    cur = conn.cursor()

    since = time.time() - (hours * 3600)
    since_formatted = time.strftime('%Y-%m-%d %H:%M:%S', time.gmtime(since))

    query = """
        SELECT e.id, e.title, e.url, e.published_at, f.title AS feed, c.title AS category
        FROM entries e
        JOIN feeds f ON f.id = e.feed_id
        JOIN categories c ON c.id = f.category_id
        WHERE e.published_at >= %s
          AND (f.disabled = false OR f.disabled IS NULL)
        ORDER BY e.published_at DESC
    """
    cur.execute(query, (since_formatted,))
    rows = cur.fetchall()

    articles = []
    seen = {}
    for row in rows:
        title = row[1].strip()
        if title not in seen:
            seen[title] = {
                "id": row[0],
                "title": title,
                "url": row[2],
                "published_at": str(row[3]),
                "feed": row[4],
                "category": row[5]
            }
            articles.append(seen[title])

    cur.close()
    conn.close()
    return articles


def call_gemini_naming(topic, articles):
    """Call Gemini 2.5 Flash Lite to produce a short human-friendly Greek topic name"""
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
                data = json.loads(text)
                if data.get("topic"):
                    return data["topic"].strip()
    except Exception:
        pass
    return topic


def cluster_super_group(articles, model, distance_threshold=0.45):
    """Cluster a list of articles (all from the same super-category) using embeddings."""
    if len(articles) < 2:
        return [[a] for a in articles]  # all singletons

    # Embed and normalize
    titles = [a["title"] for a in articles]
    embeddings_gen = model.embed(titles)
    embeddings = np.array(list(embeddings_gen))
    norms = np.linalg.norm(embeddings, axis=1, keepdims=True)
    embeddings = embeddings / np.where(norms == 0, 1e-12, norms)

    # Agglomerative clustering
    clustering = AgglomerativeClustering(
        n_clusters=None,
        metric='cosine',
        linkage='average',
        distance_threshold=distance_threshold
    )
    labels = clustering.fit_predict(embeddings)

    # Collect clusters
    groups_map = defaultdict(list)
    for idx, label in enumerate(labels):
        groups_map[int(label)].append(articles[idx])

    return list(groups_map.values())


def group_articles(articles):
    if not articles:
        return {"categories": [], "total": 0, "generated": time.strftime("%H:%M:%S")}

    # 1. Partition articles by super-category
    super_buckets = defaultdict(list)
    for a in articles:
        super_cat = SUPER_CATEGORIES.get(a["category"], "random")
        super_buckets[super_cat].append(a)

    # 2. Load the embedding model ONCE
    model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2')

    # 3. Cluster within each super-category independently
    all_clusters = []
    for super_cat, super_arts in super_buckets.items():
        if len(super_arts) == 0:
            continue
        clusters = cluster_super_group(super_arts, model)
        all_clusters.extend(clusters)

    # 4. Assign each cluster to a display category and name it
    category_groups = defaultdict(list)
    total_articles = 0

    for cluster_arts in all_clusters:
        # Determine majority display category within this cluster
        counts = defaultdict(int)
        for a in cluster_arts:
            display_name = CAT_DISPLAY.get(a["category"], "διάφορες ειδήσεις")
            counts[display_name] += 1
        majority_cat = max(counts, key=counts.get)

        # Sort by date (newest first)
        cluster_arts.sort(key=lambda x: x["published_at"], reverse=True)

        # Temporary topic: newest article's title
        temp_topic = cluster_arts[0]["title"]

        # For multi-article clusters, name with Gemini
        if len(cluster_arts) > 1:
            topic = call_gemini_naming(temp_topic, cluster_arts)
        else:
            topic = temp_topic

        group_data = {
            "topic": topic,
            "articles": cluster_arts,
            "count": len(cluster_arts)
        }
        category_groups[majority_cat].append(group_data)
        total_articles += len(cluster_arts)

    # Sort groups by count (largest first) within each category
    for cat in category_groups:
        category_groups[cat].sort(key=lambda g: g["count"], reverse=True)

    # Build output in preferred order
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


if __name__ == "__main__":
    try:
        hours = 12
        if len(sys.argv) > 1:
            hours = int(sys.argv[1])

        articles = fetch_articles(hours=hours)
        if not articles:
            print(json.dumps({"categories": [], "total": 0, "generated": time.strftime("%H:%M:%S")}))
            sys.exit(0)

        output = group_articles(articles)
        print(json.dumps(output))
    except Exception as e:
        # Return valid empty state JSON in case of failure
        print(json.dumps({
            "categories": [],
            "total": 0,
            "generated": time.strftime("%H:%M:%S")
        }))
        sys.exit(1)
