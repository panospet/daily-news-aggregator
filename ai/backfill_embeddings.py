#!/usr/bin/env python3
"""
Backfill script: computes embeddings for all existing articles in Postgres
that don't have an entry in ai_embeddings yet, and stores them.

Usage: python3 ai/backfill_embeddings.py

Requires: psycopg2-binary, fastembed, numpy
Connects directly to Postgres and the AI service.
"""
import os
import sys
import json
import requests
import psycopg2
import re
import html as html_mod

DB_HOST = os.getenv("MF_DB_HOST", "127.0.0.1")
DB_PORT = int(os.getenv("MF_DB_PORT", "5435"))
DB_USER = os.getenv("MF_DB_USER", "miniflux")
DB_PASS = os.getenv("MF_DB_PASSWORD", "")
DB_NAME = os.getenv("MF_DB_NAME", "miniflux")
AI_SERVICE_URL = os.getenv("AI_SERVICE_URL", "http://127.0.0.1:8001")

BATCH_SIZE = 50  # articles per batch sent to /embed


def extract_text(html_content):
    if not html_content:
        return ""
    plain_text = re.sub(r'<[^>]+>', '', html_content)
    plain_text = html_mod.unescape(plain_text)
    plain_text = re.sub(r'\s+', ' ', plain_text).strip()[:1000]
    return plain_text


def main():
    print(f"Connecting to Postgres at {DB_HOST}:{DB_PORT}...")
    conn = psycopg2.connect(
        host=DB_HOST, port=DB_PORT, user=DB_USER, password=DB_PASS, dbname=DB_NAME
    )
    cur = conn.cursor()

    # Ensure the table exists
    cur.execute("CREATE EXTENSION IF NOT EXISTS vector;")
    cur.execute("""
        CREATE TABLE IF NOT EXISTS ai_embeddings (
            article_id BIGINT PRIMARY KEY REFERENCES entries(id) ON DELETE CASCADE,
            embedding vector(768) NOT NULL,
            created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
        )
    """)
    cur.execute("""
        CREATE INDEX IF NOT EXISTS ai_embeddings_cosine_idx
        ON ai_embeddings USING hnsw (embedding vector_cosine_ops)
    """)
    conn.commit()

    # Find articles missing embeddings (last 24h for fast initial run)
    cur.execute("""
        SELECT e.id, e.title, e.content
        FROM entries e
        LEFT JOIN ai_embeddings ae ON ae.article_id = e.id
        WHERE ae.article_id IS NULL
          AND e.published_at >= NOW() - INTERVAL '24 hours'
        ORDER BY e.published_at DESC
    """)
    rows = cur.fetchall()
    total = len(rows)
    print(f"Found {total} articles without embeddings.")

    if total == 0:
        print("Nothing to backfill!")
        cur.close()
        conn.close()
        return

    stored = 0
    for batch_start in range(0, total, BATCH_SIZE):
        batch = rows[batch_start:batch_start + BATCH_SIZE]
        articles = []
        for row in batch:
            art_id, title, content = row
            articles.append({
                "id": art_id,
                "title": title or "",
                "content": content or ""
            })

        print(f"  Batch {batch_start // BATCH_SIZE + 1}: embedding {len(articles)} articles...", end=" ", flush=True)

        try:
            resp = requests.post(
                f"{AI_SERVICE_URL}/embed",
                json={"articles": articles},
                timeout=120
            )
            resp.raise_for_status()
            result = resp.json()
            embeddings = result.get("embeddings", {})

            for art_id_str, emb in embeddings.items():
                art_id = int(art_id_str)
                vec_str = "[" + ",".join(str(f) for f in emb) + "]"
                cur.execute(
                    "INSERT INTO ai_embeddings (article_id, embedding) VALUES (%s, %s::vector) ON CONFLICT (article_id) DO NOTHING",
                    (art_id, vec_str)
                )
                stored += 1

            conn.commit()
            print(f"stored {len(embeddings)} embeddings (total: {stored}/{total})")

        except Exception as e:
            print(f"ERROR: {e}")
            conn.rollback()

    print(f"\nBackfill complete! Stored {stored} embeddings total.")
    cur.close()
    conn.close()


if __name__ == "__main__":
    main()
