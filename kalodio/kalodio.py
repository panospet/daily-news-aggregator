#!/usr/bin/env python3
"""
ΤΟ ΚΑΛΩΔΙΟ (The Wire) — an alternative editorial frontend for the
daily-news-aggregator stack.

Reads the clustered AI data straight from Redis (`ai_cache`, DB 0) —
the same data the existing dashboard uses — and renders it as a
broadsheet-style newspaper UI. Summaries are POSTed to the existing
Go backend (which owns Gemini) and the HTML response is stripped to
plain text, so this frontend stays read-only toward the old app.

Zero changes to the existing codebase. Runs on its own port (default 8280).
"""

import html
import json
import os
import re
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs, quote

# ---------------------------------------------------------------------------
# Configuration (env-overridable)
# ---------------------------------------------------------------------------
REDIS_HOST = os.getenv("KALODIO_REDIS_HOST", "127.0.0.1")
REDIS_PORT = int(os.getenv("KALODIO_REDIS_PORT", "6379"))
UPSTREAM = os.getenv("KALODIO_UPSTREAM", "http://127.0.0.1:8080")
UPSTREAM_USER = os.getenv("KALODIO_AUTH_USER", "")
UPSTREAM_PASS = os.getenv("KALODIO_AUTH_PASS", "")
PORT = int(os.getenv("KALODIO_PORT", "8280"))
BIND = os.getenv("KALODIO_BIND", "0.0.0.0")  # 0.0.0.0 so Tailscale/LAN can reach it

# Redis access without extra deps: RESP over a plain socket.
import socket


class MiniRedis:
    """Tiny RESP client — just enough for GET with a connection pool of one."""

    def __init__(self, host, port):
        self.host, self.port = host, port
        self.lock = threading.Lock()
        self.sock = None

    def _connect(self):
        s = socket.create_connection((self.host, self.port), timeout=5)
        s.settimeout(5)
        self.sock = s

    def _read_resp(self):
        f = self.sock.makefile("rb")
        line = f.readline()
        if not line:
            raise ConnectionError("empty reply")
        t, rest = line[:1], line[1:].strip()
        if t == b"$":
            n = int(rest)
            if n == -1:
                return None
            data = f.read(n)
            f.read(2)  # CRLF
            return data
        if t in (b"+", b":"):
            return rest
        if t == b"-":
            raise ConnectionError(rest.decode(errors="replace"))
        raise ConnectionError(f"unexpected RESP type {t!r}")

    def get(self, key):
        with self.lock:
            try:
                if self.sock is None:
                    self._connect()
                self.sock.sendall(b"*2\r\n$3\r\nGET\r\n$" + str(len(key)).encode() + b"\r\n" + key.encode() + b"\r\n")
                return self._read_resp()
            except (OSError, ConnectionError):
                # reconnect once
                try:
                    self.sock.close()
                except OSError:
                    pass
                self.sock = None
                self._connect()
                self.sock.sendall(b"*2\r\n$3\r\nGET\r\n$" + str(len(key)).encode() + b"\r\n" + key.encode() + b"\r\n")
                return self._read_resp()


redis = MiniRedis(REDIS_HOST, REDIS_PORT)

# ---------------------------------------------------------------------------
# Data layer
# ---------------------------------------------------------------------------
_CACHE = {"ts": 0.0, "data": None}
_CACHE_LOCK = threading.Lock()
_TTL = 60  # seconds — cheap: one Redis GET of ~4MB JSON


def load_data():
    with _CACHE_LOCK:
        if _CACHE["data"] is not None and time.time() - _CACHE["ts"] < _TTL:
            return _CACHE["data"]
    raw = redis.get("ai_cache")
    if raw is None:
        return _CACHE["data"]  # stale-but-better-than-nothing
    payload = json.loads(raw)
    data = payload.get("data") or {}
    if data.get("categories"):
        _CACHE["data"] = data
        _CACHE["ts"] = time.time()
    return _CACHE["data"]


def all_groups(data):
    """Deduped flat list of (category, group) — groups are multi-tagged."""
    seen, out = set(), []
    for cat in data.get("categories", []):
        for g in cat.get("groups", []):
            gid = g.get("group_id") or ""
            if gid and gid in seen:
                continue
            if gid:
                seen.add(gid)
            out.append((cat["category"], g))
    return out


def group_feeds(g):
    return sorted({a.get("feed", "") for a in g.get("articles", []) if a.get("feed")})


# ---------------------------------------------------------------------------
# Summaries — proxied to the existing Go backend (it owns the Gemini key)
# ---------------------------------------------------------------------------
_TAG_RE = re.compile(r"<[^>]+>")


def fetch_summary(category, group_id):
    """POST /summarize upstream, return (plain_text, ok)."""
    body = f"category={quote(category)}&group_id={quote(group_id)}".encode()
    req = urllib.request.Request(
        UPSTREAM + "/summarize",
        data=body,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    if UPSTREAM_USER:
        import base64
        token = base64.b64encode(f"{UPSTREAM_USER}:{UPSTREAM_PASS}".encode()).decode()
        req.add_header("Authorization", "Basic " + token)
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            raw = resp.read().decode("utf-8", errors="replace")
    except (urllib.error.URLError, urllib.error.HTTPError, OSError) as e:
        return f"⚠️ Summary service unreachable: {e}", False

    if "summary-refresh" in raw or "Feedback received" in raw:
        return "⚠️ Data changed upstream — refresh and try again.", False
    m = re.search(r'<div class="summary">(.*?)</div>', raw, re.S)
    if not m:
        return "Summary unavailable.", False
    text = html.unescape(_TAG_RE.sub("", m.group(1)))
    text = re.sub(r"\n{3,}", "\n\n", text).strip()
    return text, True


# ---------------------------------------------------------------------------
# Rendering — pure-Python, zero deps, editorial broadsheet style
# ---------------------------------------------------------------------------
PAGE = """<!DOCTYPE html>
<html lang="el" data-theme="day">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ΤΟ ΚΑΛΩΔΙΟ — Η ροή σου, ως εφημερίδα</title>
<style>
:root{
  --ink:#1a1712; --paper:#f6f1e7; --paper2:#efe7d7; --accent:#8f2d1e;
  --rule:#1a1712; --muted:#6b6357; --card:#fbf8f1; --chip:#efe7d7;
}
[data-theme=night]{
  --ink:#e8e2d5; --paper:#14120e; --paper2:#1c1913; --accent:#e07a5f;
  --rule:#3a352b; --muted:#9a9184; --card:#1c1913; --chip:#26221a;
}
*{box-sizing:border-box; margin:0; padding:0}
html{scroll-behavior:smooth}
body{
  background:var(--paper); color:var(--ink);
  font-family:Georgia,'Times New Roman',serif;
  line-height:1.5;
}
/* ---------- masthead ---------- */
header{
  border-bottom:4px double var(--rule);
  padding:1.2rem 2rem .9rem;
  display:grid; grid-template-columns:1fr auto 1fr; align-items:baseline;
  gap:1rem;
}
.mh-side{font-size:.72rem; text-transform:uppercase; letter-spacing:.14em; color:var(--muted)}
.mh-side.right{text-align:right}
h1.masthead{
  font-size:clamp(2.2rem,6vw,4.2rem); font-weight:900; letter-spacing:.06em;
  text-transform:uppercase; text-align:center; white-space:nowrap;
}
h1 .amp{color:var(--accent)}
.subline{
  display:flex; justify-content:space-between; align-items:center; gap:1rem;
  padding:.45rem 2rem; border-bottom:1px solid var(--rule);
  font-size:.78rem; text-transform:uppercase; letter-spacing:.12em;
  color:var(--muted); flex-wrap:wrap;
}
/* ---------- controls ---------- */
.controls{display:flex; gap:.6rem; align-items:center; flex-wrap:wrap}
input#q{
  font:inherit; font-size:.85rem; padding:.35rem .7rem;
  background:var(--card); color:var(--ink);
  border:1px solid var(--rule); border-radius:2px; min-width:220px;
}
button.ghost{
  font:inherit; font-size:.75rem; text-transform:uppercase; letter-spacing:.1em;
  background:none; border:1px solid var(--rule); color:var(--ink);
  padding:.3rem .7rem; cursor:pointer; border-radius:2px;
}
button.ghost:hover{background:var(--chip)}
/* ---------- layout ---------- */
.wrap{display:grid; grid-template-columns:210px 1fr; gap:0; min-height:60vh}
nav.rail{
  border-right:1px solid var(--rule); padding:1.2rem 1rem;
  position:sticky; top:0; align-self:start; max-height:100vh; overflow:auto;
}
nav.rail h2{font-size:.72rem; text-transform:uppercase; letter-spacing:.16em; color:var(--muted); margin-bottom:.6rem}
nav.rail button{
  display:flex; justify-content:space-between; width:100%;
  font:inherit; font-size:.9rem; text-align:left;
  background:none; border:none; color:var(--ink); cursor:pointer;
  padding:.28rem .35rem; border-radius:2px;
}
nav.rail button:hover{background:var(--chip)}
nav.rail button.active{font-weight:700; color:var(--accent)}
nav.rail button .n{font-size:.72rem; color:var(--muted)}
main{padding:1.4rem 2rem 4rem}
/* ---------- section head ---------- */
.sec-head{
  display:flex; align-items:baseline; gap:.8rem;
  border-bottom:3px solid var(--rule); margin-bottom:1.1rem; padding-bottom:.3rem;
}
.sec-head h2{font-size:1.5rem; text-transform:uppercase; letter-spacing:.08em}
.sec-head .count{color:var(--muted); font-size:.85rem; font-style:italic}
/* ---------- story cards: newspaper column feel ---------- */
.grid{display:grid; grid-template-columns:repeat(auto-fill,minmax(330px,1fr)); gap:1.1rem}
#sentinel{height:2px}
article.story{
  background:var(--card); border:1px solid var(--rule);
  padding:1rem 1.1rem; display:flex; flex-direction:column; gap:.5rem;
  break-inside:avoid; position:relative;
}
article.story:hover{box-shadow:4px 4px 0 var(--rule); transform:translate(-2px,-2px)}
article.story.expanded{grid-column:1/-1}
article.story.expanded h3.headline{font-size:1.5rem}
article.story.expanded .summary-box{max-width:75ch}
.kicker{font-size:.68rem; text-transform:uppercase; letter-spacing:.16em; color:var(--accent)}
h3.headline{font-size:1.22rem; line-height:1.25; font-weight:700}
h3.headline a{color:inherit; text-decoration:none}
h3.headline a:hover{text-decoration:underline; text-decoration-color:var(--accent)}
.byline{font-size:.75rem; color:var(--muted); font-style:italic}
.tags{display:flex; gap:.35rem; flex-wrap:wrap; margin-top:.15rem}
.tag{
  font-size:.66rem; text-transform:uppercase; letter-spacing:.08em;
  border:1px solid var(--rule); background:var(--chip);
  padding:.12rem .45rem; border-radius:2px; cursor:pointer; color:inherit;
}
.tag:hover{background:var(--accent); color:var(--paper); border-color:var(--accent)}
details.sources{margin-top:auto}
details.sources summary{
  font-size:.72rem; text-transform:uppercase; letter-spacing:.12em;
  cursor:pointer; color:var(--muted); user-select:none;
}
details.sources ul{list-style:none; margin:.4rem 0 0; padding:0}
details.sources li{font-size:.82rem; padding:.14rem 0; border-top:1px dotted var(--muted)}
details.sources a{color:inherit; text-decoration:none}
details.sources a:hover{color:var(--accent)}
details.sources .when{color:var(--muted); font-size:.72rem; margin-left:.4rem}
.btn-sum{
  align-self:flex-start; margin-top:.4rem;
  font:inherit; font-size:.72rem; text-transform:uppercase; letter-spacing:.1em;
  background:var(--ink); color:var(--paper); border:none;
  padding:.4rem .8rem; cursor:pointer;
}
.btn-sum:hover{background:var(--accent)}
.summary-box{
  margin-top:.5rem; border-left:3px solid var(--accent);
  padding:.5rem .7rem; background:var(--paper2);
  font-size:.9rem; white-space:pre-wrap;
}
.summary-box .spin::after{content:"…"; animation:dots 1.2s steps(4) infinite}
@keyframes dots{0%{content:""}25%{content:"."}50%{content:".."}75%{content:"..."}}
/* ---------- empty / footer ---------- */
.empty{padding:3rem 0; text-align:center; color:var(--muted); font-style:italic}
footer{
  border-top:4px double var(--rule); margin-top:2rem;
  padding:1rem 2rem; font-size:.75rem; color:var(--muted);
  display:flex; justify-content:space-between; flex-wrap:wrap; gap:.5rem;
}
@media (max-width:760px){
  .wrap{grid-template-columns:1fr}
  nav.rail{position:static; max-height:none; border-right:none; border-bottom:1px solid var(--rule);
    display:flex; flex-wrap:wrap; gap:.2rem; align-items:center}
  nav.rail h2{width:100%}
  nav.rail button{width:auto}
  main{padding:1rem 1rem 3rem}
  header{grid-template-columns:1fr; justify-items:center; gap:.2rem}
  .mh-side{display:none}
}
/* ---- read state ---- */
article.story--read{opacity:.5}
article.story--read h3.headline{text-decoration:line-through}
article.story--read .btn-sum{display:none}
/* ---- source metadata ---- */
.src-meta{display:block; font-size:.72rem; color:var(--muted); margin-top:.12rem}
/* ---- toast notifications ---- */
#toast-container{position:fixed; top:1rem; right:1rem; z-index:200; display:flex; flex-direction:column; gap:.5rem}
.toast{background:var(--card); border:1px solid var(--rule); border-radius:4px; padding:.65rem 1rem; font-size:.82rem; transform:translateX(120%); transition:transform .3s; max-width:320px}
.toast.show{transform:translateX(0)}
.toast.error{border-color:var(--accent); color:var(--accent)}
.toast.info{border-color:var(--muted); color:var(--muted)}
/* ---- fallback / notice banner ---- */
.notice-banner{background:var(--chip); border:1px solid var(--rule); border-radius:4px; padding:.7rem 1rem; margin-bottom:1rem; font-size:.82rem; color:var(--muted)}
/* ---- rail section headings ---- */
.rail-section{font-size:.68rem; text-transform:uppercase; letter-spacing:.14em; color:var(--muted); margin-top:1rem; margin-bottom:.4rem; padding-top:.6rem; border-top:1px solid var(--rule)}
/* ---- unread toggle ---- */
.unread-toggle{font:inherit; font-size:.72rem; text-transform:uppercase; letter-spacing:.08em; background:none; border:1px solid var(--rule); color:var(--muted); padding:.25rem .6rem; cursor:pointer; border-radius:2px}
.unread-toggle.active{background:var(--accent); color:var(--paper); border-color:var(--accent)}
</style>
</head>
<body>
<div id="toast-container"></div>
<div id="fallback-banner" class="notice-banner" hidden></div>
<header>
  <div class="mh-side" id="edition">—</div>
  <h1 class="masthead">ΤΟ ΚΑΛΩΔΙΟ<span class="amp">.</span></h1>
  <div class="mh-side right">Η ροή σου — ως εφημερίδα</div>
</header>
<div class="subline">
  <div class="controls">
    <input id="q" type="search" placeholder="Αναζήτηση τίτλων… (ή #ετικέτα)" autocomplete="off">
    <button class="ghost" onclick="setCat('all')">Όλα</button>
    <button class="unread-toggle" id="unreadBtn" onclick="toggleReadFilter()">Νέα μόνο</button>
  </div>
  <div class="controls">
    <span id="clock"></span>
    <button class="ghost" id="themeBtn" onclick="toggleTheme()">☾ Νύχτα</button>
  </div>
</div>
<div class="wrap">
  <nav class="rail" id="rail"><h2>Τμήματα</h2></nav>
  <main>
    <div class="sec-head"><h2 id="secTitle">Πρώτη Σελίδα</h2><span class="count" id="secCount"></span></div>
    <div class="grid" id="grid"></div>
    <div class="empty" id="empty" hidden>Κανένα ρεπορτάζ δεν ταιριάζει — δοκίμασε άλλο τμήμα ή αναζήτηση.</div>
  </main>
</div>
<footer>
  <span>ΤΟ ΚΑΛΩΔΙΟ — εναλλακτικό μέτωπο για το news aggregator σου</span>
  <span>Δεδομένα: ai_cache · Συνοψίσεις: μέσω του υπάρχοντος backend · :8280</span>
</footer>
<script>
let DATA=null, CAT='all', Q='';
const rail=document.getElementById('rail'), grid=document.getElementById('grid'),
      empty=document.getElementById('empty');

function esc(s){const d=document.createElement('div');d.textContent=s??'';return d.innerHTML}
function plural(n){return n===1?'πηγή':'πηγές'}

async function load(){
  const r=await fetch('/api/news');
  DATA=await r.json();
  buildRail(); apply();
  document.getElementById('edition').textContent='Εκδοση '+DATA.generated+' — '+DATA.total+' αρθρα';
}
function buildRail(){
  rail.querySelectorAll('button,.rail-section').forEach(b=>b.remove());
  const mk=(id,label,n)=>{
    const b=document.createElement('button');
    b.innerHTML=esc(label)+' <span class="n">'+n+'</span>';
    b.dataset.cat=id; b.onclick=()=>setCat(id);
    if((CAT===id)||(CAT==='all'&&id==='all')) b.classList.add('active');
    rail.appendChild(b);
  };
  const unreadCount=SHOW_READ?DATA.groups.length:DATA.groups.filter(g=>!isRead(g.gid)).length;
  mk('all','Πρώτη Σελίδα',unreadCount);
  for(const c of DATA.cats) mk(c.name,c.name,c.count);
  // F4: top tags sidebar section
  const topTags=(DATA.top_tags||[]).slice(0,15);
  if(topTags.length){
    const h=document.createElement('div'); h.className='rail-section'; h.textContent='Ετικέτες';
    rail.appendChild(h);
    for(const t of topTags){
      const b=document.createElement('button');
      b.innerHTML=esc(t.tag)+' <span class="n">'+t.count+'</span>';
      b.onclick=()=>setTag(t.tag);
      rail.appendChild(b);
    }
  }
}
function setCat(c){CAT=c;Q='';document.getElementById('q').value='';buildRail();apply()}
function setTag(t){CAT='all';Q='#'+t;document.getElementById('q').value=Q;buildRail();apply()}

function apply(){
  let items=DATA.groups;
  if(CAT!=='all') items=items.filter(g=>g.cats.includes(CAT));
  if(Q.startsWith('#')){
    const t=Q.slice(1).toLowerCase();
    items=items.filter(g=>g.tags.some(x=>x.toLowerCase().includes(t)));
  }else if(Q){
    const q=Q.toLowerCase();
    items=items.filter(g=>
      g.topic.toLowerCase().includes(q)||
      g.feeds.some(f=>f.toLowerCase().includes(q))||
      g.articles.some(a=>a.title.toLowerCase().includes(q)));
  }
  // F1: filter out read stories when "Νέα μόνο" is active
  if(!SHOW_READ) items=items.filter(g=>!isRead(g.gid));
  VIEW=items.slice().sort((a,b)=>b.count-a.count||b.arts-a.arts);
  SHOWN=0;
  document.getElementById('secTitle').textContent=
    Q? (Q.startsWith('#')?'Ετικέτα: '+Q.slice(1):'Αναζήτηση: '+Q) :
    (CAT==='all'?'Πρώτη Σελίδα':CAT);
  document.getElementById('secCount').textContent=VIEW.length+' θέματα';
  empty.hidden=VIEW.length>0;
  grid.innerHTML='';
  loadMore();
}
/* ---- lazy loading: render in batches as you scroll ---- */
const BATCH=36;
let VIEW=[], SHOWN=0, io=null;
const sentinel=document.createElement('div'); sentinel.id='sentinel';

function loadMore(){
  const frag=document.createDocumentFragment();
  const end=Math.min(SHOWN+BATCH, VIEW.length);
  for(let i=SHOWN;i<end;i++) frag.appendChild(card(VIEW[i]));
  SHOWN=end;
  grid.appendChild(frag);
  document.getElementById('secCount').textContent=
    SHOWN+' / '+VIEW.length+' θέματα';
  if(!sentinel.isConnected) grid.after(sentinel);
  armObserver();
  if(SHOWN>=VIEW.length && io){ io.disconnect(); }
}
function armObserver(){
  if(io) io.disconnect();
  if(!('IntersectionObserver' in window)){
    // ancient browser fallback: render everything
    while(SHOWN<VIEW.length){ loadMore(); } return;
  }
  io=new IntersectionObserver(entries=>{
    for(const e of entries){
      if(e.isIntersecting && SHOWN<VIEW.length){
        io.unobserve(e.target);
        loadMore();
      }
    }
  },{rootMargin:'900px 0px'});   // start loading ~2 screens ahead
  io.observe(sentinel);
}
function card(g){
  const el=document.createElement('article'); el.className='story'+(isRead(g.gid)?' story--read':'');
  const srcs=g.articles.map(a=>
    '<li><a href="'+esc(a.url)+'" target="_blank" rel="noopener">'+esc(a.title)+'</a>'+
    '<span class="src-meta">'+esc(a.feed)+' · '+esc(a.time_ago)+'</span></li>').join('');
  el.innerHTML=
    '<div class="kicker">'+esc(g.cat)+'</div>'+
    '<h3 class="headline">'+esc(g.topic)+'</h3>'+
    '<div class="byline">'+g.count+' '+plural(g.count)+' · '+esc(g.feeds.slice(0,4).join(', '))+
      (g.feeds.length>4?' …':'')+'</div>';
  // tags — built via DOM to avoid inline-handler quoting issues
  const tagWrap=document.createElement('div'); tagWrap.className='tags';
  for(const t of g.tags.slice(0,4)){
    const b=document.createElement('button'); b.className='tag'; b.textContent=t;
    b.onclick=()=>setTag(t); tagWrap.appendChild(b);
  }
  el.appendChild(tagWrap);
  const det=document.createElement('details'); det.className='sources';
  det.innerHTML='<summary>Οι '+g.count+' '+plural(g.count)+'</summary><ul>'+srcs+'</ul>';
  el.appendChild(det);
  if(g.gid){
    const btn=document.createElement('button'); btn.className='btn-sum';
    btn.textContent='✦ Σύνοψη';
    const box=document.createElement('div'); box.className='summary-box'; box.hidden=true;
    const cat=g.cat;
    btn.onclick=()=>{
      const article=btn.closest('article');
      const isOpen=!box.hidden;
      if(isOpen){
        // shrink: hide summary, drop full-row span
        box.hidden=true;
        article.classList.remove('expanded');
        btn.textContent='✦ Σύνοψη';
      }else{
        box.hidden=false;
        if(!box.dataset.loaded){
          box.innerHTML='<span class="spin">Ο συντάκτης γράφει</span>';
          summarize(btn,box,g.gid,cat);
        }
        article.classList.add('expanded');
        btn.textContent='× Κλείσιμο';
        markRead(g.gid); article.classList.add('story--read');
        article.scrollIntoView({behavior:'smooth',block:'nearest'});
      }
    };
    el.appendChild(btn); el.appendChild(box);
  }
  return el;
}
async function summarize(btn,box,gid,cat){
  btn.disabled=true;
  try{
    const r=await fetch('/api/summary',{method:'POST',
      headers:{'Content-Type':'application/json'},
      body:JSON.stringify({category:cat,group_id:gid})});
    const j=await r.json();
    box.dataset.loaded='1';
    box.textContent=j.summary||j.error||'—';
  }catch(e){box.textContent='⚠️ '+e}
  btn.disabled=false;
}
function toggleTheme(){
  const h=document.documentElement;
  const night=h.dataset.theme==='night';
  h.dataset.theme=night?'day':'night';
  document.getElementById('themeBtn').textContent=night?'☾ Νύχτα':'☀ Μέρα';
  try{localStorage.setItem('kalodio-theme',h.dataset.theme)}catch(e){}
}
try{const t=localStorage.getItem('kalodio-theme');if(t)document.documentElement.dataset.theme=t;
  if(document.documentElement.dataset.theme==='night')document.getElementById('themeBtn').textContent='☀ Μέρα';
}catch(e){}
document.getElementById('q').addEventListener('input',e=>{
  Q=e.target.value.trim();apply();
  // F6: debounced semantic search for text queries
  clearTimeout(window._searchTimer);
  if(Q&&!Q.startsWith('#')){window._searchTimer=setTimeout(()=>semanticSearch(Q),400)}
});
async function semanticSearch(query){
  try{
    const r=await fetch('/api/search?q='+encodeURIComponent(query));
    if(!r.ok)return;const j=await r.json();
    if(j.groups&&j.groups.length>0){
      VIEW=j.groups;SHOWN=0;
      document.getElementById('secTitle').textContent='Αναζήτηση: '+query;
      document.getElementById('secCount').textContent=VIEW.length+' θέματα';
      empty.hidden=VIEW.length>0;grid.innerHTML='';loadMore();
    }
  }catch(e){/* silent — substring results already shown */}
}
setInterval(()=>{document.getElementById('clock').textContent=
  new Date().toLocaleTimeString('el-GR',{hour:'2-digit',minute:'2-digit'})},1000);

/* ---- read state management (F1) ---- */
const READ_KEY='kalodio-read';
let READ_SET=new Set(JSON.parse(localStorage.getItem(READ_KEY)||'[]'));
let SHOW_READ=true;
function saveRead(){try{localStorage.setItem(READ_KEY,JSON.stringify([...READ_SET]))}catch(e){}}
function markRead(gid){if(gid){READ_SET.add(gid);saveRead()}}
function isRead(gid){return gid&&READ_SET.has(gid)}
function toggleReadFilter(){
  SHOW_READ=!SHOW_READ;
  const btn=document.getElementById('unreadBtn');
  if(btn){btn.classList.toggle('active',!SHOW_READ);btn.textContent=SHOW_READ?'Νέα μόνο':'Όλα'}
  buildRail();apply();
}

/* ---- toast notifications (F7) ---- */
function showToast(msg,type){
  const c=document.getElementById('toast-container');if(!c)return;
  const t=document.createElement('div');t.className='toast '+(type||'info');t.textContent=msg;
  c.appendChild(t);requestAnimationFrame(()=>t.classList.add('show'));
  setTimeout(()=>{t.classList.remove('show');setTimeout(()=>t.remove(),300)},4000);
}

/* ---- keyboard shortcuts (F5) ---- */
document.addEventListener('keydown',e=>{
  if(e.key==='/'&&!['INPUT','TEXTAREA'].includes(document.activeElement.tagName)){
    e.preventDefault();document.getElementById('q').focus();
  }
  if(e.key==='Escape'){
    document.querySelectorAll('article.story.expanded').forEach(a=>{
      a.classList.remove('expanded');
      const btn=a.querySelector('.btn-sum');if(btn)btn.textContent='✦ Σύνοψη';
      const box=a.querySelector('.summary-box');if(box)box.hidden=true;
    });
    document.getElementById('q').blur();
  }
});

/* ---- auto-refresh (F2) — every 10 min ---- */
setInterval(async()=>{
  if(document.querySelector('article.story.expanded'))return;
  try{const r=await fetch('/api/news');if(!r.ok)return;DATA=await r.json();buildRail();apply();
    checkFallback();
  }catch(e){showToast('Σφάλμα φόρτωσης — προσπάθεια ξανά.','error')}
},10*60*1000);

/* ---- fallback banner (F8) — heuristic: generated > 3h ago ---- */
function checkFallback(){
  const b=document.getElementById('fallback-banner');if(!b||!DATA)return;
  try{
    const gen=new Date(DATA.generated);const ageH=(Date.now()-gen.getTime())/3.6e6;
    if(ageH>3){b.textContent='⏳ Τα δεδομένα είναι παλιότερα από 3 ώρες — ενδέχεται να ανανεώνονται.';b.hidden=false}
    else{b.hidden=true}
  }catch(e){b.hidden=true}
}

load();
checkFallback();
</script>
</body>
</html>"""


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # quieter logs
        pass

    def _send(self, code, body, ctype="text/html; charset=utf-8"):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        parsed = urlparse(self.path)
        if parsed.path == "/":
            self._send(200, PAGE.encode())
        elif parsed.path == "/api/news":
            self._send(200, json.dumps(news_api()).encode(), "application/json")
        elif parsed.path == "/api/search":
            self._handle_search(parsed)
        elif parsed.path == "/healthz":
            self._send(200, b"ok", "text/plain")
        else:
            self._send(404, b"not found", "text/plain")

    def _handle_search(self, parsed):
        """Proxy semantic search to the Go backend, return JSON groups."""
        q = parse_qs(parsed.query).get("q", [""])[0]
        if not q:
            self._send(200, json.dumps({"groups": []}).encode(), "application/json")
            return
        try:
            req = urllib.request.Request(
                UPSTREAM + "/search?q=" + quote(q),
                headers={"HX-Request": "true", "Accept": "text/html"},
            )
            if UPSTREAM_USER:
                import base64
                token = base64.b64encode(
                    f"{UPSTREAM_USER}:{UPSTREAM_PASS}".encode()
                ).decode()
                req.add_header("Authorization", "Basic " + token)
            with urllib.request.urlopen(req, timeout=30) as resp:
                html_resp = resp.read().decode("utf-8", errors="replace")
            # Extract group IDs from data-group-id attributes (preserve order, dedupe)
            gids = list(dict.fromkeys(re.findall(r'data-group-id="([^"]*)"', html_resp)))
            # Look up groups from ai_cache
            data = load_data() or {}
            gid_set = set(gids)
            result_groups, seen = [], set()
            for cat_name, g in all_groups(data):
                gid = g.get("group_id") or ""
                if not gid or gid in seen or gid not in gid_set:
                    continue
                seen.add(gid)
                arts = [
                    {"title": a.get("title", ""), "url": a.get("url", "#"),
                     "feed": a.get("feed", ""), "time_ago": a.get("time_ago", "")}
                    for a in g.get("articles", [])
                ]
                result_groups.append({
                    "gid": gid, "cat": cat_name, "cats": [cat_name],
                    "topic": g.get("topic", ""), "count": g.get("count", len(arts)),
                    "tags": g.get("tags", [])[:6], "feeds": group_feeds(g),
                    "arts": len(arts), "articles": arts,
                })
            self._send(200, json.dumps({"groups": result_groups}).encode(), "application/json")
        except Exception as e:  # noqa: BLE001
            self._send(200, json.dumps({"groups": [], "error": str(e)}).encode(), "application/json")

    def do_POST(self):
        if urlparse(self.path).path != "/api/summary":
            self._send(404, b"not found", "text/plain")
            return
        try:
            length = int(self.headers.get("Content-Length", 0))
            payload = json.loads(self.rfile.read(length) or b"{}")
            text, ok = fetch_summary(payload.get("category", ""), payload.get("group_id", ""))
            self._send(200, json.dumps({"summary": text if ok else "", "error": None if ok else text}).encode(),
                       "application/json")
        except Exception as e:  # noqa: BLE001
            self._send(500, json.dumps({"summary": "", "error": str(e)}).encode(), "application/json")


def news_api():
    data = load_data() or {}
    cats, groups = [], []
    # collect ALL category memberships per group_id (groups are cross-listed
    # upstream; rail counts and filtering must agree)
    memberships = {}  # group_id -> [cat names]
    for cat in data.get("categories", []):
        cname = cat["category"]
        cats.append({"name": cname, "count": len(cat.get("groups", []))})
        for g in cat.get("groups", []):
            gid = g.get("group_id") or ""
            if gid:
                memberships.setdefault(gid, [])
                if cname not in memberships[gid]:
                    memberships[gid].append(cname)
    seen = set()
    for cat_name, g in all_groups(data):
        gid = g.get("group_id") or ""
        if gid and gid in seen:
            continue
        if gid:
            seen.add(gid)
        arts = [
            {"title": a.get("title", ""), "url": a.get("url", "#"), "feed": a.get("feed", ""),
             "time_ago": a.get("time_ago", "")}
            for a in g.get("articles", [])
        ]
        groups.append({
            "gid": gid,
            "cat": cat_name,
            "cats": memberships.get(gid, [cat_name]),
            "topic": g.get("topic", ""),
            "count": g.get("count", len(arts)),
            "tags": g.get("tags", [])[:6],
            "feeds": group_feeds(g),
            "arts": len(arts),
            "articles": arts,
        })
    return {"total": data.get("total", len(groups)), "generated": data.get("generated", "—"),
            "cats": cats, "groups": groups, "top_tags": data.get("top_tags", [])}


def find_creds():
    """Best-effort: pull AUTH_USERNAME/AUTH_PASSWORD from env or repo .env so
    the summary proxy works out of the box. Never logged, only sent upstream."""
    global UPSTREAM_USER, UPSTREAM_PASS
    if UPSTREAM_USER and UPSTREAM_PASS:
        return
    paths = ("/home/panos/daily-news-aggregator/.env",
             os.path.join(os.path.dirname(os.path.abspath(__file__)), ".env"))
    for path in paths:
        try:
            with open(path) as f:
                for line in f:
                    if line.startswith("AUTH_USERNAME="):
                        UPSTREAM_USER = line.split("=", 1)[1].strip()
                    elif line.startswith("AUTH_PASSWORD="):
                        UPSTREAM_PASS = line.split("=", 1)[1].strip()
        except OSError:
            pass


if __name__ == "__main__":
    find_creds()
    srv = ThreadingHTTPServer((BIND, PORT), Handler)
    print(f" ΤΟ ΚΑΛΩΔΙΟ serving on http://127.0.0.1:{PORT} (upstream {UPSTREAM})")
    srv.serve_forever()
