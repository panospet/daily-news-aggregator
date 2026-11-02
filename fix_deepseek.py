import re
def extract_deepseek_json(text):
    import json
    # Attempt normal parse
    try:
        data = json.loads(text)
        if "text" in data and "topic" not in data:
            data = json.loads(data["text"])
        return data
    except:
        pass
    # Regex fallback
    match = re.search(r'\{.*"topic".*\}', text, re.DOTALL)
    if match:
        try:
            return json.loads(match.group(0))
        except:
            pass
    return None
