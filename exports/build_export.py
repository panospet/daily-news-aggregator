#!/usr/bin/env python3
"""Build a readable markdown export from the flydubai query results."""
import os
import re
from collections import defaultdict

SRC = "/tmp/fd.tsv"
OUT_DIR = "/home/panos/daily-news-aggregator/exports"
OUT = os.path.join(OUT_DIR, "flydubai-2026-10-01.md")

rows = []
for line in open(SRC, encoding="utf-8"):
    line = line.rstrip("\n")
    if not line:
        continue
    parts = line.split("|")
    if len(parts) < 5:
        continue
    when, copies, title, sources, url = parts[0], parts[1], parts[2], parts[3], parts[4]
    rows.append({"when": when, "copies": int(copies), "title": title.strip(),
                 "sources": sources.strip(), "url": url.strip()})

# bucket by day for readability
by_day = defaultdict(list)
for r in rows:
    by_day[r["when"].split()[0]].append(r)

os.makedirs(OUT_DIR, exist_ok=True)
with open(OUT, "w", encoding="utf-8") as f:
    f.write("# Flydubai incident — Miniflux coverage\n\n")
    f.write("Window: articles mentioning Flydubai published in the last 24 hours "
            "(30/09 18:21 – 01/10 20:58 EEST).\n")
    f.write("Source: Miniflux Postgres (`%s`), queried directly. Times are Europe/Athens.\n\n" % "miniflux")
    f.write("**%d mentions across %d distinct stories.** Duplicate rows in Miniflux "
            "(the same article fetched more than once) are collapsed, with the copy count kept.\n\n"
            % (sum(r["copies"] for r in rows), len(rows)))
    for day in sorted(by_day, reverse=True):
        f.write("## %s\n\n" % day)
        for r in sorted(by_day[day], key=lambda x: x["when"], reverse=True):
            extra = " _(%d copies)_" % r["copies"] if r["copies"] > 1 else ""
            f.write("- **%s** — %s%s\n" % (r["when"].split()[1], r["title"], extra))
            f.write("  %s · %s\n" % (r["sources"], r["url"]))
        f.write("\n")

print("wrote %s (%d bytes, %d stories)" % (OUT, os.path.getsize(OUT), len(rows)))
