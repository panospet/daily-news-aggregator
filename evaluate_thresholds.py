import json
import numpy as np
from sklearn.cluster import AgglomerativeClustering

# Read cached data to get texts
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

print(f"Total articles: {len(articles)}")

# We need the embeddings. Since ai-service has fastembed loaded, we can just hit it.
# Actually, we can just run fastembed locally in the script.
from fastembed import TextEmbedding
model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2')

texts = [f"{a['title']} {a.get('content', '')[:1000]}".strip() for a in articles]
embeddings = list(model.embed(texts))
embeddings = np.array(embeddings)
norms = np.linalg.norm(embeddings, axis=1, keepdims=True)
embeddings = embeddings / np.where(norms == 0, 1e-12, norms)

for threshold in [0.25, 0.30, 0.35, 0.40, 0.45]:
    clustering = AgglomerativeClustering(
        n_clusters=None,
        metric='cosine',
        linkage='average',
        distance_threshold=threshold
    )
    labels = clustering.fit_predict(embeddings)
    from collections import Counter
    counts = Counter(labels)
    cluster_sizes = sorted(list(counts.values()), reverse=True)
    print(f"Threshold: {threshold} -> Total clusters: {len(counts)}. Top 5 sizes: {cluster_sizes[:5]}")
