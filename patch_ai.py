import re

with open('ai/main.py', 'r') as f:
    code = f.read()

code = code.replace(
    'COMMAND_CODE_API_KEY = os.getenv("COMMAND_CODE_API_KEY", "")',
    'COMMAND_CODE_API_KEY = os.getenv("COMMAND_CODE_API_KEY", "")\nGEMINI_API_KEY = os.getenv("GEMINI_API_KEY", "")\nfallback_used_globally = False'
)

# In call_gemini_naming, add fallback
new_naming = '''
async def call_gemini_naming(topic, articles, client):
    global fallback_used_globally
    if not COMMAND_CODE_API_KEY and not GEMINI_API_KEY:
        return {"topic": topic, "core_tags": ["Διάφορα"], "custom_tags": []}

    try:
        articles_str = "\\n".join([f"- {a['title']} ({a.get('feed', '')})" for a in articles])
        prompt = f"""Είσαι βοηθός ονοματοδοσίας και κατηγοριοποίησης ειδήσεων. 
Έχω μια ομάδα άρθρων που αναφέρονται στο ίδιο γεγονός. Ο προσωρινός τίτλος είναι: "{topic}".

Παρακάτω είναι τα άρθρα:
{articles_str}

Αποστολή σου:
1. Γράψε έναν κατάλληλο, ελκυστικό, πολύ σύντομο (έως 8-10 λέξεις) τίτλο θεματικής (Topic) στα ΕΛΛΗΝΙΚΑ.
2. Επίλεξε από 1 έως 3 κατηγορίες από την παρακάτω αυστηρή λίστα (Core Categories):
{json.dumps(CORE_CATEGORIES, ensure_ascii=False)}
3. Αν θέλεις, πρόσθεσε έως 2 δικές σου ειδικές ετικέτες (Custom Tags) που περιγράφουν πιο συγκεκριμένα το θέμα. ΕΙΔΙΚΑ αν επιλέξεις την κατηγορία "Νέες Κυκλοφορίες (Μουσική)", ΠΡΕΠΕΙ ΟΠΩΣΔΗΠΟΤΕ να προσθέσεις το μουσικό είδος (π.χ. "Rock", "Pop", "Hip Hop") στις ειδικές ετικέτες.

Απάντησε ΜΟΝΟ με το παρακάτω JSON format:
{{"topic": "Ο Ελληνικός Τίτλος", "core_tags": ["Κατηγορία 1", "Κατηγορία 2"], "custom_tags": ["Ειδική Ετικέτα 1"]}}"""

        async def _try_command_code():
            if not COMMAND_CODE_API_KEY: return None
            url = "https://api.commandcode.ai/provider/v1/chat/completions"
            headers = {"Authorization": f"Bearer {COMMAND_CODE_API_KEY}", "Content-Type": "application/json"}
            req_body = {"model": "MiniMaxAI/MiniMax-M3", "messages": [{"role": "user", "content": prompt}], "temperature": 0.1, "response_format": {"type": "json_object"}}
            resp = await client.post(url, headers=headers, json=req_body, timeout=30.0)
            if resp.status_code == 200:
                res_json = resp.json()
                choices = res_json.get("choices", [])
                if choices:
                    text = choices[0].get("message", {}).get("content", "").strip()
                    return text
            return None
            
        async def _try_gemini():
            if not GEMINI_API_KEY: return None
            url = f"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key={GEMINI_API_KEY}"
            req_body = {"contents": [{"parts": [{"text": prompt}]}], "generationConfig": {"temperature": 0.1, "responseMimeType": "application/json"}}
            resp = await client.post(url, json=req_body, timeout=30.0)
            if resp.status_code == 200:
                res_json = resp.json()
                cands = res_json.get("candidates", [])
                if cands:
                    return cands[0].get("content", {}).get("parts", [{}])[0].get("text", "").strip()
            return None

        text = await _try_command_code()
        if text is None:
            text = await _try_gemini()
            if text is not None:
                fallback_used_globally = True
            
        if text:
            text = text.replace("```json", "").replace("```", "").strip()
            start_idx = text.find('{')
            end_idx = text.rfind('}')
            if start_idx != -1 and end_idx != -1 and end_idx > start_idx:
                text = text[start_idx:end_idx+1]
            try:
                data = json.loads(text)
                if isinstance(data, dict):
                    for k in ["response", "text", "content", "answer"]:
                        if k in data and isinstance(data[k], str) and "topic" in data[k]:
                            try:
                                inner_data = json.loads(data[k])
                                if "topic" in inner_data: data = inner_data; break
                            except: pass
                if isinstance(data, dict) and data.get("topic"):
                    return {"topic": data.get("topic", topic).strip(), "core_tags": data.get("core_tags", ["Διάφορα"]), "custom_tags": data.get("custom_tags", [])}
            except Exception as je:
                print(f"Gemini json error on text {repr(text)}: {je}")
    except Exception as e:
        import traceback; traceback.print_exc()
    return {"topic": topic, "core_tags": ["Διάφορα"], "custom_tags": []}
'''

code = re.sub(r'async def call_gemini_naming.*?return \{"topic": topic, "core_tags": \["Διάφορα"\], "custom_tags": \[\]\}', new_naming, code, flags=re.DOTALL)


new_batch = '''
async def call_gemini_batch_categorize(articles, client):
    global fallback_used_globally
    if (not COMMAND_CODE_API_KEY and not GEMINI_API_KEY) or not articles:
        return [{"core_tags": ["Διάφορα"], "custom_tags": []} for _ in articles]
        
    results = [{"core_tags": ["Διάφορα"], "custom_tags": []} for _ in articles]

    batch_size = 30
    max_concurrent = 10
    semaphore = asyncio.Semaphore(max_concurrent)

    async def process_batch(batch_start, batch_articles):
        global fallback_used_globally
        async with semaphore:
            try:
                articles_str = ""
                for i, a in enumerate(batch_articles):
                    articles_str += f"\\nΆρθρο {i}:\\nΤίτλος: {a['title']}\\nΚείμενο: {a['text'][:200]}\\n"
                prompt = f"""Είσαι βοηθός κατηγοριοποίησης ειδήσεων. Έχω μια λίστα από ανεξάρτητα άρθρα.
Για το καθένα:
1. Επίλεξε από 1 έως 3 κατηγορίες από την παρακάτω αυστηρή λίστα (Core Categories):
{json.dumps(CORE_CATEGORIES, ensure_ascii=False)}
2. Πρόσθεσε προαιρετικά έως 2 δικές σου ειδικές ετικέτες (Custom Tags).

Απάντησε ΜΟΝΟ με ένα JSON object όπου τα keys είναι οι αριθμοί των άρθρων (0 έως {len(batch_articles)-1}) και τα values είναι αντικείμενα με "core_tags" και "custom_tags". Παράδειγμα:
{{"0": {{"core_tags": ["Ελλάδα"], "custom_tags": ["Αθήνα"]}}, "1": {{"core_tags": ["Μουσική", "Νέες Κυκλοφορίες (Μουσική)"], "custom_tags": ["Rock"]}}}}
            
Λίστα Άρθρων:
{articles_str}"""

                async def _try_command_code():
                    if not COMMAND_CODE_API_KEY: return None
                    url = "https://api.commandcode.ai/provider/v1/chat/completions"
                    headers = {"Authorization": f"Bearer {COMMAND_CODE_API_KEY}", "Content-Type": "application/json"}
                    req_body = {"model": "MiniMaxAI/MiniMax-M3", "messages": [{"role": "user", "content": prompt}], "temperature": 0.0, "response_format": {"type": "json_object"}}
                    resp = await client.post(url, headers=headers, json=req_body, timeout=180.0)
                    if resp.status_code == 200:
                        res_json = resp.json()
                        choices = res_json.get("choices", [])
                        if choices:
                            return choices[0].get("message", {}).get("content", "").strip()
                    return None

                async def _try_gemini():
                    if not GEMINI_API_KEY: return None
                    url = f"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key={GEMINI_API_KEY}"
                    req_body = {"contents": [{"parts": [{"text": prompt}]}], "generationConfig": {"temperature": 0.0, "responseMimeType": "application/json"}}
                    resp = await client.post(url, json=req_body, timeout=180.0)
                    if resp.status_code == 200:
                        res_json = resp.json()
                        cands = res_json.get("candidates", [])
                        if cands:
                            return cands[0].get("content", {}).get("parts", [{}])[0].get("text", "").strip()
                    return None

                text = await _try_command_code()
                if text is None:
                    text = await _try_gemini()
                    if text is not None:
                        fallback_used_globally = True
                        
                if text:
                    text = text.replace("```json", "").replace("```", "").strip()
                    try:
                        data = json.loads(text)
                        for k, v in data.items():
                            idx = int(k)
                            if 0 <= idx < len(batch_articles):
                                c_tags = v.get("core_tags", ["Διάφορα"]) if isinstance(v, dict) else ["Διάφορα"]
                                u_tags = v.get("custom_tags", []) if isinstance(v, dict) else []
                                results[batch_start + idx] = {"core_tags": c_tags, "custom_tags": u_tags}
                    except Exception as je:
                        print(f"Gemini batch json error: {je}")
            except Exception as e:
                print(f"Gemini batch class error: {type(e).__name__} - {e}")

    tasks = [process_batch(b, articles[b:min(b + batch_size, len(articles))]) for b in range(0, len(articles), batch_size)]
    await asyncio.gather(*tasks)

    return results
'''
code = re.sub(r'async def call_gemini_batch_categorize.*?return results', new_batch, code, flags=re.DOTALL)

# Add fallback_used_globally reset in cluster()
code = code.replace(
    'def cluster(data: dict):',
    'def cluster(data: dict):\n    global fallback_used_globally\n    fallback_used_globally = False'
)

# Add is_fallback to output in cluster()
code = code.replace(
    '"generated": time.strftime("%H:%M:%S")',
    '"generated": time.strftime("%H:%M:%S"),\n        "is_fallback": fallback_used_globally'
)

# Add if COMMAND_CODE_API_KEY or GEMINI_API_KEY check
code = code.replace(
    'if COMMAND_CODE_API_KEY:',
    'if COMMAND_CODE_API_KEY or GEMINI_API_KEY:'
)


with open('ai/main.py', 'w') as f:
    f.write(code)

