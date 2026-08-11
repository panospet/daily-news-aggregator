import json
import numpy as np
from sklearn.feature_extraction.text import TfidfVectorizer
from sklearn.metrics.pairwise import cosine_distances
from sklearn.cluster import AgglomerativeClustering
from fastembed import TextEmbedding
import spacy

print("Loading models...")
model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-mpnet-base-v2')
nlp = spacy.load("el_core_news_md")

import html

def extract_text(html_content):
    import re
    if not html_content: return ""
    plain_text = re.sub(r'<[^>]+>', '', html_content)
    plain_text = html.unescape(plain_text)
    plain_text = re.sub(r'\s+', ' ', plain_text).strip()[:1000]
    return plain_text

def test_clustering(filepath, label):
    with open(filepath, 'r') as f:
        data = json.load(f)
    
    articles = data.get("articles", [])
    if len(articles) < 2: return

    texts = []
    for a in articles:
        content = extract_text(a.get("content", ""))
        content_snippet = " ".join(content.split()[:50])
        texts.append(f"{a['title'].strip()} {content_snippet}".strip())

    embeddings = np.array(list(model.embed(texts, batch_size=16)))
    dense_distances = cosine_distances(embeddings)

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

    alpha = 0.7
    hybrid_distances = alpha * dense_distances + (1 - alpha) * sparse_distances

    print(f"\n==============================================")
    print(f"[{label}]")
    
    for thresh in [0.50, 0.55, 0.58, 0.60, 0.62, 0.65]:
        clustering = AgglomerativeClustering(
            n_clusters=None,
            metric='precomputed',
            linkage='complete',
            distance_threshold=thresh
        )
        labels = clustering.fit_predict(hybrid_distances)
        
        from collections import defaultdict
        groups = defaultdict(list)
        for idx, lbl in enumerate(labels):
            groups[lbl].append(articles[idx]["title"])
            
        print(f"\n--- Threshold {thresh} ---")
        for g, titles in groups.items():
            print(f"Group {g}:")
            for t in titles:
                print(f"  - {t}")

test_clustering("/tmp/neg_feedback.json", "NEGATIVE FEEDBACK (Should SPLIT)")
test_clustering("/tmp/pos_feedback.json", "POSITIVE FEEDBACK (Should STAY TOGETHER)")
