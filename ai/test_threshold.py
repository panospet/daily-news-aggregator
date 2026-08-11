import json
import re
import html
from itertools import combinations
import numpy as np
from fastembed import TextEmbedding
import spacy
from sklearn.feature_extraction.text import TfidfVectorizer
from sklearn.metrics.pairwise import cosine_distances

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

print("Loading models...")
model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-mpnet-base-v2')
nlp = spacy.load("el_core_news_md")
print("Models loaded.")

pos, neg = load_feedbacks()

def compute_group_distance(group_json, name):
    articles = group_json.get("articles", [])
    if len(articles) < 2:
        return
    
    texts = []
    for a in articles:
        text = extract_text(a.get("content", ""))
        content_words = text.split()[:50]
        content_snippet = " ".join(content_words)
        texts.append(f"{a['title'].strip()} {content_snippet}".strip())
    
    embeddings = np.array(list(model.embed(texts, batch_size=45)))
    dense_distances = cosine_distances(embeddings)
    
    lemmatized_texts = []
    for doc in nlp.pipe(texts, disable=["parser"]):
        lemmas = [t.lemma_ for t in doc if not t.is_punct and not t.is_stop]
        ents = [ent.text.replace(" ", "_") for ent in doc.ents]
        enriched = lemmas + ents * 2
        lemmatized_texts.append(" ".join(enriched))

    vectorizer = TfidfVectorizer(analyzer='word', token_pattern=r'(?u)\b\w+\b')
    tfidf_matrix = vectorizer.fit_transform(lemmatized_texts)
    sparse_distances = cosine_distances(tfidf_matrix)

    alpha = 0.7
    hybrid_distances = alpha * dense_distances + (1 - alpha) * sparse_distances
    
    max_dist = np.max(hybrid_distances)
    print(f"--- Group: {name} (N={len(articles)}) ---")
    print(f"Max Complete-Linkage Distance: {max_dist:.4f}")
    if len(articles) == 2:
        print(f"Pair: {articles[0]['title']}  VS  {articles[1]['title']}")
    else:
        print(f"Articles: {[a['title'] for a in articles]}")
    print()

for fb in neg:
    if fb["id"] in [17, 16, 14]:
        compute_group_distance(fb["group_json"], f"NEGATIVE (ID: {fb['id']} - Should SPLIT)")
        
for fb in pos:
    if fb["id"] == 33:
        compute_group_distance(fb["group_json"], f"POSITIVE (ID: {fb['id']} - Should MERGE)")
