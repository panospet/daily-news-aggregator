import json
import re
import html
from itertools import combinations
import numpy as np
from fastembed import TextEmbedding
import spacy
from sklearn.feature_extraction.text import TfidfVectorizer
from sklearn.metrics.pairwise import cosine_distances
from sklearn.cluster import AgglomerativeClustering

def extract_text(html_content):
    if not html_content: return ""
    plain_text = re.sub(r'<[^>]+>', '', html_content)
    plain_text = html.unescape(plain_text)
    return re.sub(r'\s+', ' ', plain_text).strip()[:1000]

def load_feedbacks():
    pos = []
    with open("/app/pos_fb.json") as f:
        for line in f:
            if line.strip(): pos.append(json.loads(line))
    neg = []
    with open("/app/neg_fb.json") as f:
        for line in f:
            if line.strip(): neg.append(json.loads(line))
    return pos, neg

model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-mpnet-base-v2')
nlp = spacy.load("el_core_news_md")

pos, neg = load_feedbacks()

def get_hybrid_distances(articles):
    texts = []
    for a in articles:
        text = extract_text(a.get("content", ""))
        content_words = text.split()[:50]
        texts.append(f"{a['title'].strip()} {' '.join(content_words)}".strip())
    
    embeddings = np.array(list(model.embed(texts, batch_size=45)))
    dense = cosine_distances(embeddings)
    
    lemmatized = []
    for doc in nlp.pipe(texts, disable=["parser"]):
        lemmas = [t.lemma_ for t in doc if not t.is_punct and not t.is_stop]
        ents = [ent.text.replace(" ", "_") for ent in doc.ents]
        lemmatized.append(" ".join(lemmas + ents * 2))

    vec = TfidfVectorizer(analyzer='word', token_pattern=r'(?u)\b\w+\b')
    sparse = cosine_distances(vec.fit_transform(lemmatized))
    return 0.7 * dense + 0.3 * sparse

def test_linkage(name, articles, linkage):
    if len(articles) < 2: return
    dists = get_hybrid_distances(articles)
    print(f"--- Group: {name} (N={len(articles)}) ---")
    
    for thresh in [0.45, 0.50, 0.55, 0.60]:
        cl = AgglomerativeClustering(n_clusters=None, metric='precomputed', linkage=linkage, distance_threshold=thresh)
        labels = cl.fit_predict(dists)
        num_clusters = len(set(labels))
        print(f"  {linkage.capitalize()} Linkage @ {thresh:.2f}: {num_clusters} clusters")
    print()

for fb in neg:
    if fb["id"] in [17, 16, 14]:
        test_linkage(f"NEGATIVE ID {fb['id']} (Ideal: N clusters)", fb["group_json"]["articles"], 'complete')
        test_linkage(f"NEGATIVE ID {fb['id']} (Ideal: N clusters)", fb["group_json"]["articles"], 'average')
        
for fb in pos:
    if fb["id"] == 33:
        test_linkage(f"POSITIVE ID {fb['id']} (Ideal: 1 cluster)", fb["group_json"]["articles"], 'complete')
        test_linkage(f"POSITIVE ID {fb['id']} (Ideal: 1 cluster)", fb["group_json"]["articles"], 'average')
