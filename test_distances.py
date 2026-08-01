import json
import numpy as np
from sklearn.cluster import AgglomerativeClustering
from fastembed import TextEmbedding
import re

def extract_text(html_content):
    if not html_content:
        return ""
    plain_text = re.sub(r'<[^>]+>', '', html_content)
    plain_text = re.sub(r'\s+', ' ', plain_text).strip()[:1000]
    return plain_text

# Read cached data to get articles
with open("/app/ai_cache.json") as f:
    data = json.load(f)

# Extract articles from cache (reconstructing flat list)
articles = []
for cat in data['data']['categories']:
    for group in cat['groups']:
        articles.extend(group['articles'])

if not articles:
    print("No articles found")
    exit()

# Take just 20 articles from the same category to see pairwise distances
subset = [a for a in articles if a.get('category') == 'international'][:20]

print(f"Testing {len(subset)} articles from 'international' category")
texts = []
for a in subset:
    text = extract_text(a.get("content", ""))
    full_text = a['title']
    texts.append(full_text)
    print(f"Title: {a['title']}")
    print(f"Extracted Text: {text[:100]}...")

model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2')
embeddings = np.array(list(model.embed(texts)))
norms = np.linalg.norm(embeddings, axis=1, keepdims=True)
embeddings = embeddings / np.where(norms == 0, 1e-12, norms)

dist_matrix = 1 - np.dot(embeddings, embeddings.T)
print("\nDistance Matrix (first 5x5):")
print(np.round(dist_matrix[:5, :5], 3))

# Try clustering
for threshold in [0.25, 0.35, 0.45, 0.55]:
    for linkage in ['average', 'complete']:
        clustering = AgglomerativeClustering(n_clusters=None, metric='cosine', linkage=linkage, distance_threshold=threshold)
        labels = clustering.fit_predict(embeddings)
        from collections import Counter
        counts = Counter(labels)
        sizes = sorted(list(counts.values()), reverse=True)
        print(f"Threshold: {threshold}, Linkage: {linkage} -> {len(counts)} clusters. Sizes: {sizes[:5]}")
