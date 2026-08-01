import os
import sys
import time
import json
import psycopg2
from sentence_transformers import SentenceTransformer
from sklearn.cluster import AgglomerativeClustering
import numpy as np

# Load config from .env or default values
DB_HOST = os.getenv("MF_DB_HOST", "127.0.0.1")
DB_PORT = int(os.getenv("MF_DB_PORT", "5433"))
DB_NAME = os.getenv("MF_DB_NAME", "miniflux")
DB_USER = os.getenv("MF_DB_USER", "external_reader")
DB_PASS = os.getenv("MF_DB_PASSWORD", "")

def fetch_articles(hours=6):
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
    # Deduplicate exact titles using dict
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

def group_articles(articles):
    if not articles:
        return []
        
    t0 = time.time()
    # 1. Load local lightweight embedding model
    print("[timing] Loading SentenceTransformer...", flush=True)
    model = SentenceTransformer('paraphrase-multilingual-MiniLM-L12-v2', cache_folder='/home/panos/.hermes/models')
    print(f"[timing] Model loaded in {time.time() - t0:.2f}s", flush=True)
    
    # 2. Extract and encode titles
    titles = [a["title"] for a in articles]
    t1 = time.time()
    print(f"[timing] Encoding {len(titles)} titles...", flush=True)
    embeddings = model.encode(titles, batch_size=32, show_progress_bar=False, convert_to_numpy=True)
    print(f"[timing] Encoded in {time.time() - t1:.2f}s", flush=True)
    
    # Normalize vectors for Cosine Similarity
    norms = np.linalg.norm(embeddings, axis=1, keepdims=True)
    embeddings = embeddings / np.where(norms == 0, 1e-12, norms)
    
    # 3. Perform Agglomerative Clustering using a cosine distance threshold
    # Cosine distance = 1 - Cosine Similarity
    # Similarity threshold: 0.65 -> Distance threshold: 0.35
    distance_threshold = 0.35
    
    clustering = AgglomerativeClustering(
        n_clusters=None,
        metric='cosine',
        linkage='average',
        distance_threshold=distance_threshold
    )
    
    labels = clustering.fit_predict(embeddings)
    
    # Group articles by labels
    groups_map = {}
    for idx, label in enumerate(labels):
        label = int(label)
        if label not in groups_map:
            groups_map[label] = []
        groups_map[label].append(articles[idx])
        
    # Reassemble categories and display matching order
    # Mapping internal to display name
    cat_display_names = {
        "international": "ειδήσεις Κόσμος",
        "news":          "ειδήσεις Ελλάδα",
        "sports":        "αθλητικές ειδήσεις",
        "music":         "μουσικά νέα",
        "politics":      "ειδήσεις Ελλάδα",
        "random":        "διάφορες ειδήσεις",
        "panionara":     "αθλητικές ειδήσεις",
    }
    
    # Partition clustered groups into their final categories
    # We assign the category based on the majority / first article in the group
    category_groups = {
        "ειδήσεις Κόσμος": [],
        "ειδήσεις Ελλάδα": [],
        "μουσικά νέα": [],
        "αθλητικές ειδήσεις": [],
        "διάφορες ειδήσεις": []
    }
    
    total_grouped = 0
    
    for label, group_arts in groups_map.items():
        # Decide category by looking at articles' original categories
        # Let's count them
        counts = {}
        for a in group_arts:
            display_name = cat_display_names.get(a["category"], "διάφορες ειδήσεις")
            counts[display_name] = counts.get(display_name, 0) + 1
            
        majority_cat = max(counts, key=counts.get)
        
        # Sort group by publication date (newest first)
        group_arts.sort(key=lambda x: x["published_at"], reverse=True)
        
        # Topic representation: for now, use the title of the newest article
        # Later, we can call Gemini-2.5-flash-lite on the fly to name it when expanded,
        # but for list rendering we can use a temporary title.
        topic = group_arts[0]["title"]
        
        group_data = {
            "topic": topic,
            "articles": group_arts,
            "count": len(group_arts)
        }
        category_groups[majority_cat].append(group_data)
        total_grouped += len(group_arts)
        
    # Sort category groups so larger groups appear first
    for cat in category_groups:
        category_groups[cat].sort(key=lambda g: g["count"], reverse=True)
        
    # Format response similarly to our Go struct
    output = {
        "categories": [
            {
                "category": cat,
                "groups": category_groups[cat],
                "count": len(category_groups[cat])
            }
            for cat in ["ειδήσεις Κόσμος", "ειδήσεις Ελλάδα", "αθλητικές ειδήσεις", "μουσικά νέα", "διάφορες ειδήσεις"]
            if len(category_groups[cat]) > 0
        ],
        "total": total_grouped,
        "generated": time.strftime("%H:%M:%S")
    }
    
    return output

if __name__ == "__main__":
    t_start = time.time()
    try:
        articles = fetch_articles(hours=12)
        print(f"Fetched {len(articles)} articles from Database.", flush=True)
        if not articles:
            print("No articles to cluster.", flush=True)
            sys.exit(0)
            
        output = group_articles(articles)
        print(f"\n[timing] SUCCESS: Overall process took {time.time() - t_start:.2f}s", flush=True)
        
        # Print a small overview of top clustered topics
        print("\n--- Top Clustered Topics Overview ---")
        for cat_group in output["categories"]:
            print(f"\n📁 Category: {cat_group['category']}")
            for idx, g in enumerate(cat_group["groups"]):
                if g["count"] > 1:
                    print(f"  • Cluster {idx+1} ({g['count']} articles):")
                    print(f"    Topic placeholder: {g['topic']}")
                    for a in g["articles"]:
                        print(f"      - {a['title']} ({a['feed']})")
                else:
                    # just print 1 example singleton
                    if idx == 0 or idx == len(cat_group["groups"]) - 1:
                        print(f"  • Singleton example: '{g['topic']}' ({g['articles'][0]['feed']})")
                        
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)
