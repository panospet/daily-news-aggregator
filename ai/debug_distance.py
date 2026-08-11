import json
import numpy as np
from sklearn.feature_extraction.text import TfidfVectorizer
from sklearn.metrics.pairwise import cosine_distances
from fastembed import TextEmbedding
import spacy
import html

model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-mpnet-base-v2')
nlp = spacy.load("el_core_news_md")

def extract_text(html_content):
    import re
    if not html_content: return ""
    plain_text = re.sub(r'<[^>]+>', '', html_content)
    plain_text = html.unescape(plain_text)
    plain_text = re.sub(r'\s+', ' ', plain_text).strip()[:1000]
    return plain_text

with open("/tmp/neg_feedback.json", 'r') as f:
    data = json.load(f)

articles = data.get("articles", [])
texts = []
for a in articles:
    content = extract_text(a.get("content", ""))
    content_snippet = " ".join(content.split()[:50])
    texts.append(f"{a['title'].strip()} {content_snippet}".strip())

embeddings = np.array(list(model.embed(texts, batch_size=16)))
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

print(f"Article 1: {texts[0]}")
print(f"Article 2: {texts[1]}")
print(f"Dense Distance: {dense_distances[0][1]:.4f}")
print(f"Sparse Distance: {sparse_distances[0][1]:.4f}")
print(f"Hybrid Distance: {hybrid_distances[0][1]:.4f}")
print(f"\nLemmatized 1: {lemmatized_texts[0]}")
print(f"Lemmatized 2: {lemmatized_texts[1]}")
