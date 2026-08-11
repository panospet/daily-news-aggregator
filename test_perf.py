from fastembed import TextEmbedding
import time
import os

os.environ["OMP_NUM_THREADS"] = "4"
print("Loading model...")
model = TextEmbedding(model_name='sentence-transformers/paraphrase-multilingual-mpnet-base-v2', threads=4)
texts = ["This is a test document to embed. It has some text in it."] * 600

print("Embedding...")
t0 = time.time()
embeddings = list(model.embed(texts, batch_size=32))
t1 = time.time()
print(f"Time for 600 documents (threads=4, batch=32): {t1-t0:.2f}s")
