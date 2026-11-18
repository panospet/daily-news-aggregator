#!/usr/bin/env python3
"""Does 'Reasoning' actually cost latency on the summarizer prompt?

Same production-style prompt as bench_models.py (12 real articles from the cached group).
For each model:
  pass A (streaming)  -> time to first byte, time to first CONTENT token, reasoning deltas,
                         total time, usage if the provider reports it
  pass B (non-stream) -> usage.completion_tokens_details.reasoning_tokens, JSON validity
"""
import json
import os
import re
import time
import urllib.request

ENV = {}
for line in open(os.path.join(os.path.dirname(__file__), ".env")):
    m = re.match(r"^([A-Z_0-9]+)=(.*)$", line.strip())
    if m:
        ENV[m.group(1)] = m.group(2).strip().strip('"').strip("'")
HDRS = {"Content-Type": "application/json",
        "Authorization": "Bearer " + ENV.get("COMMAND_CODE_API_KEY", ""),
        "User-Agent": "Mozilla/5.0 (X11; Linux x86_64) Chrome/120 Safari/537.36"}
API = "https://api.commandcode.ai/provider/v1/chat/completions"

d = json.load(open("/tmp/ai_cache.json"))
arts = None
for c in d["data"]["categories"]:
    for g in c["groups"]:
        if g.get("group_id") == "7564f59af04d":
            arts = g["articles"]
            break
    if arts:
        break

HEAD = """Είσαι βοηθός σύνοψης ειδήσεων. Θέμα: "Επίθεση σε πιλοτήριο πτήσης Flydubai"

Παρακάτω είναι άρθρα που καλύπτουν αυτό το θέμα, αριθμημένα [1], [2], ...

ΑΥΣΤΗΡΟΙ ΚΑΝΟΝΕΣ ΕΔΑΦΟΥΣ (grounding):
- Χρησιμοποιείς ΜΟΝΟ πληροφορίες που υπάρχουν ρητά στα παρακάτω άρθρα.
- ΑΠΑΓΟΡΕΥΕΤΑΙ να αναφέρεις σκορ, ονόματα, ημερομηνίες ή αριθμούς που δεν εμφανίζονται στα άρθρα.
- Μετά από κάθε πρόταση με γεγονότα βάζεις παραπομπή πηγής, π.χ. [1] ή [2][5].

1. Γράψε μια εκτενή και αναλυτική περίληψη (στα ΕΛΛΗΝΙΚΑ, 2-3 παραγράφους), με παραπομπές [n].
2. Σύνθεσε ΟΛΕΣ τις μοναδικές πληροφορίες από τα διάφορα άρθρα.
3. Γράψε έναν σύντομο τίτλο θεματικής (Topic) στα ΕΛΛΗΝΙΚΑ.

CRITICAL INSTRUCTION: return ONE line of valid JSON: {"summary": "...", "topic": "..."}

Άρθρα:
"""


def build():
    tag = re.compile(r"<[^>]*>")
    out = []
    for i, a in enumerate(arts[:12]):
        out.append("[%d] Title: %s\n   Source: %s (%s)\n   Content: %s\n\n" % (
            i + 1, a.get("title"), a.get("feed"), a.get("category"),
            " ".join(tag.sub(" ", a.get("content") or "").split())))
    return HEAD + "".join(out)


PROMPT = build()
MODELS = [("MiniMaxAI/MiniMax-M3", "current"),
          ("MiniMaxAI/MiniMax-M2.7", "same price, no-reasoning"),
          ("xiaomi/mimo-v2.5", "cheaper, no-reasoning"),
          ("xiaomi/mimo-v2.6-flash", "cheapest")]

print("prompt chars: %d (~%d tokens)\n" % (len(PROMPT), len(PROMPT) // 4))


def stream_pass(model):
    payload = {"model": model, "messages": [{"role": "user", "content": PROMPT}],
               "temperature": 0.2, "max_tokens": 65536, "stream": True,
               "stream_options": {"include_usage": True},
               "response_format": {"type": "json_object"}}
    req = urllib.request.Request(API, data=json.dumps(payload).encode(), headers=HDRS)
    t0 = time.time()
    first_any = first_content = None
    reasoning_chars = content_chars = 0
    usage = {}
    with urllib.request.urlopen(req, timeout=600) as r:
        for raw in r:
            line = raw.decode("utf-8", "ignore").strip()
            if not line.startswith("data:"):
                continue
            body = line[5:].strip()
            if body == "[DONE]":
                break
            try:
                chunk = json.loads(body)
            except Exception:
                continue
            if chunk.get("usage"):
                usage = chunk["usage"]
            for ch in chunk.get("choices") or []:
                delta = ch.get("delta") or {}
                if delta.get("reasoning_content") or delta.get("reasoning"):
                    reasoning_chars += len(delta.get("reasoning_content") or delta.get("reasoning") or "")
                    if first_any is None:
                        first_any = time.time() - t0
                if delta.get("content"):
                    content_chars += len(delta["content"])
                    if first_any is None:
                        first_any = time.time() - t0
                    if first_content is None:
                        first_content = time.time() - t0
    return {"ttfb": first_any, "ttfc": first_content, "total": time.time() - t0,
            "reasoning_chars": reasoning_chars, "content_chars": content_chars, "usage": usage}


def plain_pass(model):
    payload = {"model": model, "messages": [{"role": "user", "content": PROMPT}],
               "temperature": 0.2, "max_tokens": 65536,
               "response_format": {"type": "json_object"}}
    req = urllib.request.Request(API, data=json.dumps(payload).encode(), headers=HDRS)
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=600) as r:
        body = json.loads(r.read())
    el = time.time() - t0
    u = body.get("usage") or {}
    msg = (body.get("choices") or [{}])[0].get("message", {}) or {}
    txt = msg.get("content") or ""
    rtok = ((u.get("completion_tokens_details") or {}).get("reasoning_tokens")
            or u.get("reasoning_tokens"))
    ok, cites, greek = "no", 0, 0
    try:
        s, e = txt.find("{"), txt.rfind("}")
        obj = json.loads(txt[s:e + 1])
        ok = "yes" if obj.get("summary") and obj.get("topic") else "missing fields"
        cites = len(re.findall(r"\[\d+\]", obj.get("summary", "")))
        greek = sum(1 for ch in obj.get("summary", "") if "Α" <= ch <= "ω")
    except Exception as exc:
        ok = "PARSE FAIL: " + str(exc)[:36]
    return {"latency": el, "usage": u, "reasoning_tokens": rtok, "usable": ok,
            "cites": cites, "greek": greek, "has_reasoning_field": bool(msg.get("reasoning_content"))}


for model, note in MODELS:
    print("=== %s  (%s)" % (model, note))
    try:
        s = stream_pass(model)
        print("   stream : ttfb %.1fs | first CONTENT %.1fs | total %.1fs | reasoning chars %d | content chars %d"
              % (s["ttfb"] or -1, s["ttfc"] or -1, s["total"], s["reasoning_chars"], s["content_chars"]))
        if s["usage"]:
            u = s["usage"]
            details = u.get("completion_tokens_details") or {}
            print("            usage: prompt=%s completion=%s reasoning=%s" % (
                u.get("prompt_tokens"), u.get("completion_tokens"),
                details.get("reasoning_tokens") or u.get("reasoning_tokens")))
    except Exception as exc:
        print("   stream FAILED: %s" % str(exc)[:130])
    try:
        p = plain_pass(model)
        u = p["usage"]
        print("   plain  : %.1fs | prompt=%s completion=%s reasoning=%s | json=%s | cites=%d | greek=%d | reasoning field=%s"
              % (p["latency"], u.get("prompt_tokens"), u.get("completion_tokens"),
                 p["reasoning_tokens"], p["usable"], p["cites"], p["greek"], p["has_reasoning_field"]))
    except Exception as exc:
        print("   plain FAILED: %s" % str(exc)[:130])
    print()
