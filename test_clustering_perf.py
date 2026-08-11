import psycopg2
import requests
import time
import json
import os
from datetime import datetime, timedelta

# Connect to db
conn = psycopg2.connect(
    dbname="miniflux",
    user="miniflux",
    password="REDACTED",
    host="127.0.0.1",
    port=5435
)
cur = conn.cursor()
twelve_hours_ago = datetime.utcnow() - timedelta(hours=12)

cur.execute("""
    SELECT e.id, e.title, e.content, e.url, e.published_at, c.title AS category, f.title AS feed 
    FROM entries e 
    JOIN feeds f ON e.feed_id = f.id 
    JOIN categories c ON f.category_id = c.id 
    WHERE e.published_at >= %s 
    ORDER BY e.published_at DESC
""", (twelve_hours_ago,))

rows = cur.fetchall()
articles = []
for r in rows:
    articles.append({
        "id": str(r[0]),
        "title": r[1],
        "content": r[2],
        "url": r[3],
        "published_at": r[4].isoformat() if r[4] else None,
        "category": r[5],
        "feed": r[6]
    })
print(f"Fetched {len(articles)} articles.")

t0 = time.time()
resp = requests.post("http://localhost:8001/cluster", json={"articles": articles})
t1 = time.time()

print(f"Status Code: {resp.status_code}")
print(f"Time taken: {t1 - t0:.2f}s")
if resp.status_code == 200:
    data = resp.json()
    print(f"Total returned: {data.get('total')}")
    for cat in data.get("categories", []):
        print(f"Category: {cat['category']} | Groups: {len(cat['groups'])}")
        # print some groups
        counts = [g['count'] for g in cat['groups']]
        print(f"Group counts: {counts[:10]}...")
